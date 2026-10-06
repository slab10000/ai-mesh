package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SessionWindow struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	Agent     string `json:"agent"`
	Project   string `json:"project"`
	Resume    bool   `json:"resume"`
}
type SessionGroup struct {
	ID        string          `json:"id"`
	Socket    string          `json:"socket"`
	Token     string          `json:"token"`
	Windows   []SessionWindow `json:"windows"`
	Back      []string        `json:"back"`
	CreatedAt string          `json:"created_at"`
}
type RemoteSession struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Project string `json:"project"`
	Resume  bool   `json:"resume"`
	Socket  string `json:"socket"`
	Token   string `json:"token"`
	Window  string `json:"window"`
}
type SwitchRequest struct {
	Target  string `json:"target"`
	Agent   string `json:"agent,omitempty"`
	Project string `json:"project,omitempty"`
	Window  string `json:"window"`
	Back    bool   `json:"back"`
}

func (s *Store) tmuxName() string { return "ai-mesh-" + digest([]byte(s.Root))[:12] }
func (s *Store) tmux(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-u", "-L", s.tmuxName(), "-f", "/dev/null"}, args...)...)
}
func (s *Store) tmuxRun(args ...string) error {
	b, e := s.tmux(args...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("tmux: %w: %s", e, strings.TrimSpace(string(b)))
	}
	return nil
}
func supportedInteractive(agent string) bool {
	return agent == "codex" || agent == "claude" || agent == "gemini" || agent == "opencode" || agent == "shell"
}
func (s *Store) session(id string) (SessionGroup, error) {
	var g SessionGroup
	if !validID.MatchString(id) {
		return g, errors.New("invalid session ID")
	}
	e := readJSON(s.path("sessions", id+".json"), &g)
	return g, e
}
func (s *Store) saveSession(g SessionGroup) error {
	return writeJSON(s.path("sessions", g.ID+".json"), g)
}

func (s *Store) StartSession(agent, target, project string, resume bool) error {
	if !supportedInteractive(agent) {
		return errors.New("unsupported interactive agent")
	}
	if _, e := exec.LookPath("tmux"); e != nil {
		return errors.New("interactive sessions require tmux on both computers")
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	p, local, e := c.Resolve(target)
	if e != nil {
		return e
	}
	if !local && !p.Incoming {
		return errors.New("target declines incoming access")
	}
	if project == "" && local {
		project, _ = os.Getwd()
	}
	if local && project != "" && project != "~" && !strings.HasPrefix(project, "~/") {
		project, e = filepath.Abs(project)
		if e != nil {
			return e
		}
	}
	if resume {
		entries, _ := os.ReadDir(s.path("sessions"))
		for i := len(entries) - 1; i >= 0; i-- {
			name := entries[i].Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			g, e := s.session(strings.TrimSuffix(name, ".json"))
			if e != nil {
				continue
			}
			for _, w := range g.Windows {
				if w.MachineID == p.ID && w.Agent == agent && (project == "" || project == w.Project) {
					if s.tmux("has-session", "-t", "mesh-"+g.ID).Run() == nil {
						if e := s.ensureController(g); e != nil {
							return e
						}
						_ = s.tmuxRun("select-window", "-t", "mesh-"+g.ID+":"+w.ID)
						if dead, e := s.tmux("display-message", "-p", "-t", "mesh-"+g.ID+":"+w.ID, "#{pane_dead}").Output(); e == nil && strings.TrimSpace(string(dead)) == "1" {
							if e := s.tmuxRun("respawn-pane", "-t", "mesh-"+g.ID+":"+w.ID); e != nil {
								return e
							}
						}
						return s.attach(g.ID)
					}
				}
			}
		}
	}
	g := SessionGroup{ID: newID(), Token: newID() + newID(), CreatedAt: now()}
	socketDir := filepath.Join(os.TempDir(), "mesh-"+digest([]byte(s.Root))[:12])
	if e := os.MkdirAll(socketDir, 0700); e != nil {
		return e
	}
	g.Socket = filepath.Join(socketDir, g.ID[:12]+".sock")
	if len(g.Socket) > 100 {
		return errors.New("temporary path too long for session control socket; use a shorter TMPDIR")
	}
	w := SessionWindow{ID: "w" + newID()[:8], MachineID: p.ID, Agent: agent, Project: project, Resume: resume}
	g.Windows = []SessionWindow{w}
	if e := s.saveSession(g); e != nil {
		return e
	}
	if e := s.createWindow(g, w, true); e != nil {
		return e
	}
	if e := s.ensureController(g); e != nil {
		return e
	}
	return s.attach(g.ID)
}

func (s *Store) createWindow(g SessionGroup, w SessionWindow, first bool) error {
	exe, e := executable()
	if e != nil {
		return e
	}
	command := shellJoin("env", "MESH_HOME="+s.Root, "MESH_USER_HOME="+s.UserHome, exe, "_pane", g.ID, w.ID)
	if first {
		e = s.tmuxRun("new-session", "-d", "-s", "mesh-"+g.ID, "-n", w.ID, "sleep 86400")
	} else {
		e = s.tmuxRun("new-window", "-d", "-t", "mesh-"+g.ID, "-n", w.ID, "sleep 86400")
	}
	if e != nil {
		return e
	}
	c, _ := s.Config()
	p, _, _ := c.Resolve(w.MachineID)
	label := p.Name + " / " + w.Agent
	_ = s.tmuxRun("set-option", "-w", "-t", "mesh-"+g.ID+":"+w.ID, "automatic-rename", "off")
	_ = s.tmuxRun("set-option", "-w", "-t", "mesh-"+g.ID+":"+w.ID, "remain-on-exit", "on")
	_ = s.tmuxRun("set-option", "-w", "-t", "mesh-"+g.ID+":"+w.ID, "@mesh-label", label)
	_ = s.tmuxRun("set-option", "-t", "mesh-"+g.ID, "status-left", " ai-mesh | #{@mesh-label} ")
	_ = s.tmuxRun("set-option", "-t", "mesh-"+g.ID, "status-left-length", "80")
	_ = s.tmuxRun("set-option", "-t", "mesh-"+g.ID, "status-right", " Ctrl-b: m computers | b back | d detach ")
	_ = s.tmuxRun("set-option", "-t", "mesh-"+g.ID, "status-right-length", "60")
	_ = s.tmuxRun("set-option", "-w", "-t", "mesh-"+g.ID+":"+w.ID, "window-status-format", "")
	_ = s.tmuxRun("set-option", "-w", "-t", "mesh-"+g.ID+":"+w.ID, "window-status-current-format", "")
	for key, action := range map[string]string{"m": "_menu", "b": "_return"} {
		control := shellJoin("env", "MESH_HOME="+s.Root, "MESH_USER_HOME="+s.UserHome, exe, action, "#{session_name}", "#{window_name}")
		if e := s.tmuxRun("bind-key", key, "run-shell", "-b", control); e != nil {
			return e
		}
	}
	// Set remain-on-exit before the agent can exit, so startup/authentication
	// errors remain visible even when the provider quits immediately.
	return s.tmuxRun("respawn-pane", "-k", "-t", "mesh-"+g.ID+":"+w.ID, command)
}

func (s *Store) attach(id string) error {
	cmd := s.tmux("attach-session", "-t", "mesh-"+id)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = childEnv(nil)
	return cmd.Run()
}

func (s *Store) ensureController(g SessionGroup) error {
	if conn, e := net.DialTimeout("unix", g.Socket, 200*time.Millisecond); e == nil {
		_ = conn.Close()
		return nil
	}
	exe, e := executable()
	if e != nil {
		return e
	}
	log, e := os.OpenFile(s.path("logs", "session-"+g.ID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	cmd := exec.Command(exe, "_controller", g.ID)
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e := cmd.Start(); e != nil {
		return e
	}
	_ = cmd.Process.Release()
	for i := 0; i < 40; i++ {
		if conn, e := net.DialTimeout("unix", g.Socket, 100*time.Millisecond); e == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	details, _ := os.ReadFile(log.Name())
	return fmt.Errorf("session controller failed to start: %s (log: %s)", strings.TrimSpace(string(details)), log.Name())
}

func (s *Store) Controller(id string) error {
	g, e := s.session(id)
	if e != nil {
		return e
	}
	lock, e := os.OpenFile(s.path("sessions", id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return e
	}
	_ = os.Remove(g.Socket)
	listener, e := net.Listen("unix", g.Socket)
	if e != nil {
		return e
	}
	defer os.Remove(g.Socket)
	defer listener.Close()
	if e := os.Chmod(g.Socket, 0600); e != nil {
		return e
	}
	var mu sync.Mutex
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	server.Handler = http.HandlerFunc(func(out http.ResponseWriter, in *http.Request) {
		if in.Method != "POST" || in.URL.Path != "/switch" {
			http.NotFound(out, in)
			return
		}
		if in.Header.Get("Authorization") != "Bearer "+g.Token {
			http.Error(out, "unauthorized", 403)
			return
		}
		var request SwitchRequest
		if e := json.NewDecoder(io.LimitReader(in.Body, 8192)).Decode(&request); e != nil {
			http.Error(out, e.Error(), 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fresh, e := s.session(id)
		if e != nil {
			http.Error(out, e.Error(), 500)
			return
		}
		var current *SessionWindow
		for i := range fresh.Windows {
			if fresh.Windows[i].ID == request.Window {
				current = &fresh.Windows[i]
				break
			}
		}
		if current == nil {
			http.Error(out, "unknown originating window", 400)
			return
		}
		target := ""
		if request.Back {
			if len(fresh.Back) == 0 {
				http.Error(out, "no previous session", 409)
				return
			}
			target = fresh.Back[len(fresh.Back)-1]
			fresh.Back = fresh.Back[:len(fresh.Back)-1]
		} else {
			config, e := s.Config()
			if e != nil {
				http.Error(out, e.Error(), 500)
				return
			}
			peer, _, e := config.Resolve(request.Target)
			if e != nil {
				http.Error(out, e.Error(), 400)
				return
			}
			if peer.ID != config.Self.ID && !peer.Incoming {
				http.Error(out, "destination declines incoming access", 403)
				return
			}
			agent := request.Agent
			if agent == "" {
				agent = current.Agent
			}
			if !supportedInteractive(agent) {
				http.Error(out, "unsupported agent", 400)
				return
			}
			for _, w := range fresh.Windows {
				if w.MachineID == peer.ID && w.Agent == agent && (request.Project == "" || request.Project == w.Project) {
					target = w.ID
					break
				}
			}
			if target == "" {
				w := SessionWindow{ID: "w" + newID()[:8], MachineID: peer.ID, Agent: agent, Project: request.Project}
				target = w.ID
				fresh.Windows = append(fresh.Windows, w)
				if e := s.saveSession(fresh); e != nil {
					http.Error(out, e.Error(), 500)
					return
				}
				if e := s.createWindow(fresh, w, false); e != nil {
					http.Error(out, e.Error(), 500)
					return
				}
			}
			if target != current.ID {
				fresh.Back = append(fresh.Back, current.ID)
			}
		}
		if e := s.saveSession(fresh); e != nil {
			http.Error(out, e.Error(), 500)
			return
		}
		pane := "mesh-" + id + ":" + target
		if dead, err := s.tmux("display-message", "-p", "-t", pane, "#{pane_dead}").Output(); err == nil && strings.TrimSpace(string(dead)) == "1" {
			if err := s.tmuxRun("respawn-pane", "-t", pane); err != nil {
				http.Error(out, err.Error(), 500)
				return
			}
		}
		out.WriteHeader(200)
		_, _ = out.Write([]byte("Switch requested; your current session is preserved.\n"))
		if f, ok := out.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(350 * time.Millisecond)
			if e := s.tmuxRun("select-window", "-t", "mesh-"+id+":"+target); e != nil {
				fmt.Fprintln(os.Stderr, e)
			}
		}()
	})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if s.tmux("has-session", "-t", "mesh-"+id).Run() != nil {
				_ = server.Close()
				return
			}
		}
	}()
	e = server.Serve(listener)
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

func RequestSwitch(target, agent, project string, back bool) error {
	socket, token, window := os.Getenv("MESH_CONTROL_SOCKET"), os.Getenv("MESH_CONTROL_TOKEN"), os.Getenv("MESH_WINDOW")
	if socket == "" || token == "" || window == "" {
		return errors.New("computer switching requires a session launched with mesh codex, mesh claude, mesh shell, or mesh connect")
	}
	if e := requestSwitch(socket, token, window, target, agent, project, back); e != nil {
		return e
	}
	fmt.Println("Switch requested; your current session is preserved.")
	return nil
}

func requestSwitch(socket, token, window, target, agent, project string, back bool) error {
	body, _ := json.Marshal(SwitchRequest{target, agent, project, window, back})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, _ := http.NewRequest("POST", "http://mesh/switch", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+token)
	response, e := client.Do(request)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, 8192))
	if e != nil {
		return e
	}
	if response.StatusCode != 200 {
		return errors.New(strings.TrimSpace(string(b)))
	}
	return nil
}

func (s *Store) Pane(groupID, windowID string) error {
	g, e := s.session(groupID)
	if e != nil {
		return e
	}
	var w SessionWindow
	found := false
	for _, candidate := range g.Windows {
		if candidate.ID == windowID {
			w = candidate
			found = true
			break
		}
	}
	if !found {
		return errors.New("unknown window")
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	p, local, e := c.Resolve(w.MachineID)
	if e != nil {
		return e
	}
	if local {
		return s.launchAgent(w.Agent, w.Project, w.Resume, g.Socket, g.Token, w.ID)
	}
	remoteID := g.ID[:12] + "-" + w.ID
	socket := filepath.Join(p.Home, ".ai-mesh", "s-"+remoteID)
	if len(socket) > 100 {
		return errors.New("remote home is too long for SSH session forwarding")
	}
	spec := RemoteSession{remoteID, w.Agent, w.Project, w.Resume, socket, g.Token, w.ID}
	if e := s.Call(p, Request{Action: "prepare-session", Session: &spec}, nil); e != nil {
		return e
	}
	args, e := s.SSHArgs(p, true)
	if e != nil {
		return e
	}
	args = append(args, "-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes", "-R", socket+":"+g.Socket, p.Endpoint.Host, remoteMesh(p, "_remote-session", remoteID))
	cmd := exec.Command("ssh", args...)
	cmd.Env = childEnv(nil)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *Store) prepareSession(spec RemoteSession) error {
	if !validID.MatchString(spec.ID) || !supportedInteractive(spec.Agent) || !validID.MatchString(spec.Window) {
		return errors.New("invalid session")
	}
	if filepath.Dir(spec.Socket) != filepath.Join(s.UserHome, ".ai-mesh") || !strings.HasPrefix(filepath.Base(spec.Socket), "s-") {
		return errors.New("invalid forwarding socket")
	}
	if len(spec.Token) != 64 {
		return errors.New("invalid session token")
	}
	if info, e := os.Lstat(spec.Socket); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("forwarding path is not a socket")
		}
		if e := os.Remove(spec.Socket); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return writeJSON(s.path("sessions", "remote-"+spec.ID+".json"), spec)
}

func (s *Store) RemoteSession(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	if !c.Self.Incoming {
		return errors.New("incoming access disabled")
	}
	var spec RemoteSession
	if e := readJSON(s.path("sessions", "remote-"+id+".json"), &spec); e != nil {
		return e
	}
	if _, e := exec.LookPath("tmux"); e != nil {
		return errors.New("install tmux on this computer to preserve interactive sessions")
	}
	name := "remote-" + id
	if s.tmux("has-session", "-t", name).Run() != nil {
		exe, e := executable()
		if e != nil {
			return e
		}
		command := shellJoin("env", "MESH_HOME="+s.Root, exe, "_native", id)
		if e := s.tmuxRun("new-session", "-d", "-s", name, command); e != nil {
			return e
		}
		_ = s.tmuxRun("set-option", "-t", name, "status-left", " "+c.Self.Name+" | "+spec.Agent+" ")
	}
	// The originating Mesh terminal already labels the active machine. Avoid
	// a second, truncated tmux status bar inside the remote agent's viewport.
	_ = s.tmuxRun("set-option", "-t", name, "status", "off")
	cmd := s.tmux("attach-session", "-t", name)
	cmd.Env = childEnv(nil)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *Store) Native(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	var spec RemoteSession
	if e := readJSON(s.path("sessions", "remote-"+id+".json"), &spec); e != nil {
		return e
	}
	return s.launchAgent(spec.Agent, spec.Project, spec.Resume, spec.Socket, spec.Token, spec.Window)
}
func (s *Store) launchAgent(agent, project string, resume bool, socket, token, window string) error {
	if !supportedInteractive(agent) {
		return errors.New("unsupported agent")
	}
	if project == "" {
		project = s.UserHome
	} else if project == "~" {
		project = s.UserHome
	} else if strings.HasPrefix(project, "~/") {
		project = filepath.Join(s.UserHome, project[2:])
	} else if !filepath.IsAbs(project) {
		project = filepath.Join(s.UserHome, project)
	}
	i, e := os.Stat(project)
	if e != nil {
		return fmt.Errorf("project directory: %w", e)
	}
	if !i.IsDir() {
		return errors.New("project is not a directory")
	}
	args := []string{}
	if agent == "shell" {
		if resume {
			return errors.New("use mesh shell --resume only to reattach an existing Mesh shell")
		}
		agent = os.Getenv("SHELL")
		if agent == "" {
			agent = "/bin/sh"
		}
		args = []string{"-i"}
	}
	if resume {
		switch agent {
		case "codex":
			args = []string{"resume"}
		case "claude":
			args = []string{"--resume"}
		default:
			return errors.New("native resume is supported for codex and claude")
		}
	}
	exe, e := executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(agent, args...)
	cmd.Dir = project
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome, "MESH_CONTROL_SOCKET": socket, "MESH_CONTROL_TOKEN": token, "MESH_WINDOW": window, "PATH": filepath.Dir(exe) + string(os.PathListSeparator) + os.Getenv("PATH")})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Keyboard navigation is handled by the local Mesh controller, independently
// of an AI provider's shell sandbox or willingness to invoke a switch command.
func (s *Store) SessionControl(group, window, target string, back bool) error {
	g, e := s.session(strings.TrimPrefix(group, "mesh-"))
	if e != nil {
		return e
	}
	return requestSwitch(g.Socket, g.Token, window, target, "", "", back)
}

func (s *Store) ComputerMenu(group, window string) error {
	g, e := s.session(strings.TrimPrefix(group, "mesh-"))
	if e != nil {
		return e
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	exe, e := executable()
	if e != nil {
		return e
	}
	peers := []Peer{c.Self}
	for _, p := range c.Peers {
		if p.Incoming {
			peers = append(peers, p)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	args := []string{"display-menu", "-t", "mesh-" + g.ID + ":" + window, "-T", "Choose computer", "-x", "C", "-y", "C"}
	keys := "123456789abcdefghijklmnopqrstuvwxyz"
	for i, p := range peers {
		key := ""
		if i < len(keys) {
			key = keys[i : i+1]
		}
		command := shellJoin("env", "MESH_HOME="+s.Root, "MESH_USER_HOME="+s.UserHome, exe, "_select", g.ID, window, p.ID)
		args = append(args, p.Name, key, "run-shell -b "+quote(command))
	}
	return s.tmuxRun(args...)
}
