package mesh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Some providers execute every conversation through a shared daemon. Its
// ancestry and inherited environment belong to the daemon's first caller,
// not necessarily this conversation. A launch-scoped binding takes priority.
func (s *Store) boundSession() (SessionRuntime, bool, error) {
	value := os.Getenv("MESH_BINDING")
	if value == "" {
		return SessionRuntime{}, false, nil
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validID.MatchString(parts[0]) || !validID.MatchString(parts[1]) {
		return SessionRuntime{}, true, errors.New("invalid Mesh conversation binding")
	}
	var runtime SessionRuntime
	if err := readJSON(s.runtimePath(parts[0], parts[1]), &runtime); err != nil {
		return runtime, true, err
	}
	if !runtime.Active || runtime.Group != parts[0] || runtime.Window != parts[1] {
		return runtime, true, errors.New("bound Mesh conversation is no longer running")
	}
	return runtime, true, nil
}

// Shell snapshots can restore another conversation's MESH_* variables. Match
// the calling process to its live provider before trusting those variables.
// Only process IDs are inspected; command lines and environments may contain
// credentials and must never be dumped to resolve a session.
func (s *Store) ancestorSession() (SessionRuntime, bool, error) {
	paths, _ := filepath.Glob(s.path("runtime", "*", "*.json"))
	if len(paths) == 0 {
		return SessionRuntime{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return SessionRuntime{}, false, errors.New("cannot verify the calling Mesh conversation; request the provider's normal approval for this Mesh command")
	}
	parents := map[int]int{}
	for _, row := range strings.Split(string(data), "\n") {
		fields := strings.Fields(row)
		if len(fields) == 2 {
			pid, _ := strconv.Atoi(fields[0])
			ppid, _ := strconv.Atoi(fields[1])
			parents[pid] = ppid
		}
	}
	live := map[int]SessionRuntime{}
	for _, path := range paths {
		var runtime SessionRuntime
		if readJSON(path, &runtime) == nil && runtime.Active && runtime.PID > 1 && validID.MatchString(runtime.Group) && validID.MatchString(runtime.Window) {
			live[runtime.PID] = runtime
		}
	}
	seen := map[int]bool{}
	for pid := os.Getppid(); pid > 1 && !seen[pid]; pid = parents[pid] {
		seen[pid] = true
		if runtime, ok := live[pid]; ok {
			return runtime, true, nil
		}
	}
	return SessionRuntime{}, false, nil
}

func (s *Store) controlEnvironment() (string, string, string, error) {
	runtime, ok, err := s.boundSession()
	if !ok && err == nil {
		runtime, ok, err = s.ancestorSession()
	}
	if err != nil {
		return "", "", "", err
	}
	if ok {
		// Look up credentials for the proven process identity, never take a
		// controller token from another shell snapshot. Remote providers use
		// the SSH-forwarded controller socket recorded on their own host.
		if g, err := s.session(runtime.Group); err == nil {
			for _, w := range g.Windows {
				if w.ID == runtime.Window {
					return g.Socket, g.Token, w.ID, nil
				}
			}
		}
		var remote RemoteSession
		id := runtime.Group[:min(12, len(runtime.Group))] + "-" + runtime.Window
		if err := readJSON(s.path("sessions", "remote-"+id+".json"), &remote); err == nil && remote.Group == runtime.Group && remote.Window == runtime.Window {
			return remote.Socket, remote.Token, remote.Window, nil
		}
		return "", "", "", errors.New("cannot read this conversation's Mesh controller; request the provider's normal approval for this Mesh command")
	}
	socket, token, window := os.Getenv("MESH_CONTROL_SOCKET"), os.Getenv("MESH_CONTROL_TOKEN"), os.Getenv("MESH_WINDOW")
	if socket == "" || token == "" || window == "" {
		return "", "", "", errors.New("computer switching requires a session launched with mesh codex, mesh claude, mesh shell, or mesh connect")
	}
	return socket, token, window, nil
}
