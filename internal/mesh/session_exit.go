package mesh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// InteractiveExit preserves a provider's exit status without adding Mesh's
// error prefix to the provider's own farewell or error message.
type InteractiveExit struct{ Code int }

func (e *InteractiveExit) Error() string { return fmt.Sprintf("agent exited with status %d", e.Code) }

func interactiveResult(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		return &InteractiveExit{Code: code}
	}
	return err
}

func interactiveCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *InteractiveExit
	if errors.As(interactiveResult(err), &exit) {
		return exit.Code
	}
	return 1
}

type terminalExit struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

func meshTerminalName(name string) bool {
	return (strings.HasPrefix(name, "mesh-") || strings.HasPrefix(name, "remote-")) && validID.MatchString(name)
}

// Hooks run only on lifecycle events; no polling or provider-specific parsing
// of Ctrl-C, conversation IDs, or resume instructions is needed.
func (s *Store) configureTerminalExit(session, pane string) error {
	if !meshTerminalName(session) {
		return errors.New("invalid Mesh terminal")
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	if err := s.tmuxRun("set-option", "-w", "-t", pane, "remain-on-exit", "on"); err != nil {
		return err
	}
	if err := s.tmuxRun("set-option", "-w", "-t", pane, "remain-on-exit-format", ""); err != nil {
		return err
	}
	command := shellJoin("env", "MESH_HOME="+s.Root, "MESH_USER_HOME="+s.UserHome, exe, "_terminal-exit", session)
	for _, hook := range []string{"pane-died", "client-attached", "after-select-window"} {
		if err := s.tmuxRun("set-hook", "-t", session, hook, "run-shell -b "+quote(command)); err != nil {
			return err
		}
	}
	return nil
}

// RefreshTerminalExit also upgrades hooks on preserved sessions without
// restarting their providers. A completed pane can then release an old client.
func (s *Store) RefreshTerminalExit(session string) error {
	if !meshTerminalName(session) {
		return errors.New("invalid Mesh terminal")
	}
	panes, err := s.tmux("list-panes", "-s", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		return err
	}
	for _, pane := range strings.Fields(string(panes)) {
		if err := s.configureTerminalExit(session, pane); err != nil {
			return err
		}
	}
	return s.TerminalExited(session)
}

func (s *Store) markTerminalFinished(err error) {
	if pane := os.Getenv("TMUX_PANE"); strings.HasPrefix(pane, "%") {
		_ = s.tmuxRun("set-option", "-p", "-t", pane, "@mesh-finished", strconv.Itoa(interactiveCode(err)))
	}
}

func (s *Store) clearTerminalFinished() {
	if pane := os.Getenv("TMUX_PANE"); strings.HasPrefix(pane, "%") {
		_ = s.tmuxRun("set-option", "-p", "-u", "-t", pane, "@mesh-finished")
	}
}

// Older running wrappers predate the completion marker. Their runtime record
// still confirms a native exit, allowing a binary/hook upgrade without killing
// live conversations. An unreachable peer never counts as a finished agent.
func (s *Store) recordedTerminalExit(session, window string) bool {
	var state SessionRuntime
	if strings.HasPrefix(session, "remote-") {
		var spec RemoteSession
		if readJSON(s.path("sessions", session+".json"), &spec) != nil {
			return false
		}
		_ = readJSON(s.runtimePath(spec.Group, spec.Window), &state)
	} else {
		group, err := s.session(strings.TrimPrefix(session, "mesh-"))
		if err != nil {
			return false
		}
		c, err := s.Config()
		if err != nil {
			return false
		}
		for _, w := range group.Windows {
			if w.ID != window {
				continue
			}
			peer, local, err := c.Resolve(w.MachineID)
			if err != nil {
				return false
			}
			if local {
				_ = readJSON(s.runtimePath(group.ID, w.ID), &state)
			} else {
				var remote SessionState
				if s.Call(peer, Request{Action: "session-state", ID: group.ID[:12] + "-" + w.ID}, &remote) != nil {
					return false
				}
				state = remote.Runtime
			}
			break
		}
	}
	return !state.Active && state.FinishedAt != ""
}

// TerminalExited runs after tmux has drained the pane output. Only clients
// viewing the finished pane are detached; hidden agents and other groups live
// on. A missing completion marker is a transport/wrapper failure, not proof
// that the agent exited, so those panes remain available for reconnection.
func (s *Store) TerminalExited(session string) error {
	if !meshTerminalName(session) {
		return errors.New("invalid Mesh terminal")
	}
	return withLock(s.path("sessions", "exit-"+session+".lock"), func() error {
		panes, err := s.tmux("list-panes", "-s", "-t", session, "-F", "#{pane_id}\t#{pane_dead}\t#{@mesh-finished}\t#{window_name}\t#{pane_dead_status}").Output()
		if err != nil {
			return nil // The session may have been removed while the hook queued.
		}
		for _, row := range strings.Split(strings.TrimSuffix(string(panes), "\n"), "\n") {
			parts := strings.Split(row, "\t")
			if len(parts) != 5 || parts[1] != "1" {
				continue
			}
			// Hidden finished panes must not trigger remote reachability checks
			// or delay another client's exit when a peer is offline.
			clients, err := s.tmux("list-clients", "-t", session, "-F", "#{client_name}\t#{pane_id}").Output()
			if err != nil || !strings.Contains(string(clients), "\t"+parts[0]+"\n") {
				continue
			}
			if parts[2] == "" {
				if !s.recordedTerminalExit(session, parts[3]) {
					continue
				}
				parts[2] = parts[4]
			}
			code, err := strconv.Atoi(parts[2])
			if err != nil || code < 0 || code > 255 {
				continue
			}
			// Refresh after a possible SSH runtime lookup. list-clients expands
			// pane_id in each client's session; display-message -c alone can
			// resolve it in a different, more recently active session.
			clients, err = s.tmux("list-clients", "-t", session, "-F", "#{client_name}\t#{pane_id}").Output()
			if err != nil {
				continue
			}
			output, err := s.tmux("capture-pane", "-p", "-J", "-t", parts[0]).Output()
			if err != nil {
				return err
			}
			exe, err := executable()
			if err != nil {
				return err
			}
			for _, client := range strings.Split(strings.TrimSuffix(string(clients), "\n"), "\n") {
				fields := strings.Split(client, "\t")
				if len(fields) != 2 || fields[1] != parts[0] {
					continue
				}
				id := newID()
				path := s.path("sessions", "exits", id+".json")
				if err := writeJSON(path, terminalExit{Output: strings.Trim(string(output), "\n"), Code: code}); err != nil {
					return err
				}
				command := shellJoin("env", "MESH_HOME="+s.Root, "MESH_USER_HOME="+s.UserHome, exe, "_exit-output", id)
				// -E restores the real terminal and replaces its tmux client with
				// our replay helper. No "Pane is dead" or detach banner is printed.
				if err := s.tmuxRun("detach-client", "-t", fields[0], "-E", command); err != nil {
					_ = os.Remove(path) // Client disconnected before we detached it.
				}
			}
		}
		return nil
	})
}

func (s *Store) ExitOutput(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid terminal exit receipt")
	}
	path := s.path("sessions", "exits", id+".json")
	var exit terminalExit
	if err := readJSON(path, &exit); err != nil {
		return err
	}
	_ = os.Remove(path)
	if exit.Output != "" {
		fmt.Fprintln(os.Stdout, exit.Output)
	}
	if exit.Code != 0 {
		return &InteractiveExit{Code: exit.Code}
	}
	return nil
}
