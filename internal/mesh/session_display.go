package mesh

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
)

func (s *Store) frontendWindows(group string) ([]string, error) {
	data, err := s.tmux("list-clients", "-t", "mesh-"+group, "-F", "#{window_name}").Output()
	if err != nil {
		return nil, fmt.Errorf("cannot inspect the Mesh terminal: %w", err)
	}
	return strings.Fields(string(data)), nil
}

func (s *Store) requireFrontend(group string) error {
	windows, err := s.frontendWindows(group)
	if err != nil {
		return err
	}
	if len(windows) == 0 {
		return errors.New("this Mesh session has no attached terminal; no visible switch occurred. Use the attached Mesh conversation, or reattach with mesh codex --resume")
	}
	return nil
}

type SessionState struct {
	Runtime  SessionRuntime `json:"runtime"`
	Attached bool           `json:"attached"`
}

func (s *Store) remoteSessionState(id string) (SessionState, error) {
	var spec RemoteSession
	if !validID.MatchString(id) {
		return SessionState{}, errors.New("invalid session ID")
	}
	if err := readJSON(s.path("sessions", "remote-"+id+".json"), &spec); err != nil {
		return SessionState{}, err
	}
	var state SessionState
	_ = readJSON(s.runtimePath(spec.Group, spec.Window), &state.Runtime)
	data, _ := s.tmux("list-clients", "-t", "remote-"+id, "-F", "#{client_name}").Output()
	state.Attached = strings.TrimSpace(string(data)) != ""
	return state, nil
}

// Confirm the transport and provider are running, then select and verify the
// window through the attached frontend. A successful file transfer alone is
// not a successful handoff. Other windows and provider processes stay alive.
func (s *Store) displayWindow(group SessionGroup, window string) error {
	if err := s.requireFrontend(group.ID); err != nil {
		return err
	}
	var selected SessionWindow
	for _, w := range group.Windows {
		if w.ID == window {
			selected = w
		}
	}
	if selected.ID == "" {
		return errors.New("unknown destination window")
	}
	c, err := s.Config()
	if err != nil {
		return err
	}
	peer, local, err := c.Resolve(selected.MachineID)
	if err != nil {
		return err
	}
	pane := "mesh-" + group.ID + ":" + window
	deadline := time.Now().Add(15 * time.Second)
	for {
		dead, err := s.tmux("display-message", "-p", "-t", pane, "#{pane_dead}").Output()
		if err != nil || strings.TrimSpace(string(dead)) != "0" {
			return errors.New("destination terminal exited before it was ready; source conversation preserved")
		}
		var state SessionState
		if local {
			_ = readJSON(s.runtimePath(group.ID, window), &state.Runtime)
			state.Attached = state.Runtime.PID > 1 && syscall.Kill(state.Runtime.PID, 0) == nil
		} else {
			err = s.Call(peer, Request{Action: "session-state", ID: group.ID[:12] + "-" + window}, &state)
		}
		if err == nil && state.Runtime.Active && state.Runtime.PID > 1 && state.Attached {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("destination conversation did not attach in time; retry the same handoff ID or switch again")
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err := s.tmuxRun("select-window", "-t", pane); err != nil {
		return err
	}
	windows, err := s.frontendWindows(group.ID)
	if err != nil {
		return err
	}
	for _, visible := range windows {
		if visible == window {
			return nil
		}
	}
	return errors.New("destination was selected but no attached terminal displays it")
}
