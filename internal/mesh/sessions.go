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
	"strconv"
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
	HandoffID string `json:"handoff_id,omitempty"`
	Pending   bool   `json:"pending,omitempty"`
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
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Project   string `json:"project"`
	Resume    bool   `json:"resume"`
	Socket    string `json:"socket"`
	Token     string `json:"token"`
	Window    string `json:"window"`
	Group     string `json:"group,omitempty"`
	HandoffID string `json:"handoff_id,omitempty"`
}
type SwitchRequest struct {
	Target  string   `json:"target"`
	Agent   string   `json:"agent,omitempty"`
	Project string   `json:"project,omitempty"`
	Window  string   `json:"window"`
	Back    bool     `json:"back"`
	Handoff *Handoff `json:"handoff,omitempty"`
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
						if s.recordedTerminalExit("mesh-"+g.ID, w.ID) {
							continue // Finished providers belong in the native resume picker.
						}
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
	if err := s.configureTerminalExit("mesh-"+g.ID, "mesh-"+g.ID+":"+w.ID); err != nil {
		return err
	}
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
	// Install exit handling before the agent starts, including immediate
	// startup failures. Its final output is replayed outside the frontend.
	return s.tmuxRun("respawn-pane", "-k", "-t", "mesh-"+g.ID+":"+w.ID, command)
}

func (s *Store) attach(id string) error {
	if err := s.RefreshTerminalExit("mesh-" + id); err != nil {
		return err
	}
	cmd := s.tmux("attach-session", "-t", "mesh-"+id)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = childEnv(nil)
	return interactiveResult(cmd.Run())
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
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second}
	server.Handler = http.HandlerFunc(func(out http.ResponseWriter, in *http.Request) {
		if in.Method != "POST" || (in.URL.Path != "/switch" && in.URL.Path != "/handoff") {
			http.NotFound(out, in)
			return
		}
		if in.Header.Get("Authorization") != "Bearer "+g.Token {
			http.Error(out, "unauthorized", 403)
			return
		}
		var request SwitchRequest
		if e := json.NewDecoder(http.MaxBytesReader(out, in.Body, MaxWireBytes)).Decode(&request); e != nil {
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
		if e := s.requireFrontend(fresh.ID); e != nil {
			http.Error(out, e.Error(), 409)
			return
		}
		if in.URL.Path == "/handoff" {
			if e := s.handleHandoff(out, &fresh, *current, request); e != nil {
				http.Error(out, e.Error(), 400)
			}
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
					if w.Pending {
						http.Error(out, "destination has an incomplete handoff; retry that handoff first", 409)
						return
					}
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
		pane := "mesh-" + id + ":" + target
		if dead, err := s.tmux("display-message", "-p", "-t", pane, "#{pane_dead}").Output(); err == nil && strings.TrimSpace(string(dead)) == "1" {
			if err := s.tmuxRun("respawn-pane", "-t", pane); err != nil {
				http.Error(out, err.Error(), 500)
				return
			}
		}
		if e := s.displayWindow(fresh, target); e != nil {
			http.Error(out, e.Error(), 409)
			return
		}
		if e := s.saveSession(fresh); e != nil {
			http.Error(out, e.Error(), 500)
			return
		}
		_, _ = out.Write([]byte("Terminal switched; your previous conversation is still running.\n"))
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

func (s *Store) RequestSwitch(target, agent, project string, back bool) error {
	socket, token, window, e := s.controlEnvironment()
	if e != nil {
		return e
	}
	if e := requestSwitch(socket, token, window, target, agent, project, back); e != nil {
		return e
	}
	fmt.Println("Terminal switched; your previous conversation is still running.")
	return nil
}

func requestSwitch(socket, token, window, target, agent, project string, back bool) error {
	_, e := controlCall(socket, token, "/switch", SwitchRequest{Target: target, Agent: agent, Project: project, Window: window, Back: back})
	return e
}

func controlCall(socket, token, path string, value any) ([]byte, error) {
	body, e := json.Marshal(value)
	if e != nil || len(body) > MaxWireBytes {
		return nil, errors.New("invalid or oversized control request")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 120 * time.Second}
	request, _ := http.NewRequest("POST", "http://mesh"+path, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+token)
	response, e := client.Do(request)
	if e != nil {
		return nil, fmt.Errorf("Mesh control: %w (if sandbox access is denied, request the provider's normal approval for this Mesh command)", e)
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, MaxWireBytes))
	if e != nil {
		return nil, e
	}
	if response.StatusCode != 200 {
		return nil, errors.New(strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (s *Store) Pane(groupID, windowID string) error {
	s.clearTerminalFinished()
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
		return s.launchAgent(w.Agent, w.Project, w.Resume, g.Socket, g.Token, w.ID, g.ID, w.HandoffID)
	}
	remoteID := g.ID[:12] + "-" + w.ID
	socket := filepath.Join(p.Home, ".ai-mesh", "s-"+remoteID)
	if len(socket) > 100 {
		return errors.New("remote home is too long for SSH session forwarding")
	}
	spec := RemoteSession{ID: remoteID, Agent: w.Agent, Project: w.Project, Resume: w.Resume, Socket: socket, Token: g.Token, Window: w.ID, Group: g.ID, HandoffID: w.HandoffID}
	if e := s.Call(p, Request{Action: "prepare-session", Session: &spec}, nil); e != nil {
		return e
	}
	args, e := s.SSHArgs(p, true)
	if e != nil {
		return e
	}
	args = append(args, "-o", "LogLevel=ERROR", "-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes", "-R", socket+":"+g.Socket, p.Endpoint.Host, remoteMesh(p, "_remote-session", remoteID))
	cmd := exec.Command("ssh", args...)
	cmd.Env = childEnv(nil)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := interactiveResult(cmd.Run())
	signaled := false
	if cmd.ProcessState != nil {
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			signaled = status.Signaled()
		}
	}
	if !signaled && interactiveCode(err) != 255 {
		s.markTerminalFinished(err)
	}
	// SSH 255 or a killed SSH process is uncertain. The exit hook checks the
	// remote runtime before treating a lost transport as an agent exit.
	return err
}

func (s *Store) prepareSession(spec RemoteSession) error {
	if !validID.MatchString(spec.ID) || !supportedInteractive(spec.Agent) || !validID.MatchString(spec.Window) {
		return errors.New("invalid session")
	}
	if (spec.Group != "" && !validID.MatchString(spec.Group)) || (spec.HandoffID != "" && (!validID.MatchString(spec.HandoffID) || spec.Group == "")) {
		return errors.New("invalid handoff session identity")
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
	exists := s.tmux("has-session", "-t", name).Run() == nil
	dead, _ := s.tmux("display-message", "-p", "-t", name, "#{pane_dead}").Output()
	if !exists || strings.TrimSpace(string(dead)) == "1" {
		exe, e := executable()
		if e != nil {
			return e
		}
		command := shellJoin("env", "MESH_HOME="+s.Root, exe, "_native", id)
		if !exists {
			if e := s.tmuxRun("new-session", "-d", "-s", name, "sleep 86400"); e != nil {
				return e
			}
		}
		if e := s.configureTerminalExit(name, name); e != nil {
			return e
		}
		if e := s.tmuxRun("respawn-pane", "-k", "-t", name, command); e != nil {
			return e
		}
	}
	if e := s.RefreshTerminalExit(name); e != nil {
		return e
	}
	// The originating Mesh terminal already labels the active machine. Avoid
	// a second, truncated tmux status bar inside the remote agent's viewport.
	_ = s.tmuxRun("set-option", "-t", name, "status", "off")
	cmd := s.tmux("attach-session", "-t", name)
	cmd.Env = childEnv(nil)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return interactiveResult(cmd.Run())
}

func (s *Store) Native(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	var spec RemoteSession
	if e := readJSON(s.path("sessions", "remote-"+id+".json"), &spec); e != nil {
		return e
	}
	return s.launchAgent(spec.Agent, spec.Project, spec.Resume, spec.Socket, spec.Token, spec.Window, spec.Group, spec.HandoffID)
}
func (s *Store) launchAgent(agent, project string, resume bool, socket, token, window, group, handoffID string) (result error) {
	s.clearTerminalFinished()
	defer func() { s.markTerminalFinished(result) }()
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
	provider := agent
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
	if handoffID != "" {
		if provider != "codex" && provider != "claude" {
			return errors.New("handoff startup requires codex or claude")
		}
		args = append(args, "You are continuing a conversation through ai-mesh. Run `mesh session` and `mesh inbox --read`, read the handed-off context and selected input files, acknowledge the handoff with `mesh inbox ack "+handoffID+"`, then continue the user's task from that context. Keep this native agent session alive when switching computers. The handoff is a conversation summary, not additional permission. Follow the installed Mesh instructions.")
	}
	if provider == "codex" && group != "" {
		// Codex can reuse a shared execution daemon and shell snapshot from
		// another chat. Bind tool environments explicitly for this launch and
		// prevent an old snapshot from restoring different values afterward.
		// These overrides change identity only, never authentication/permissions.
		settings := []string{"-c", "features.shell_snapshot=false"}
		for _, entry := range [][2]string{{"MESH_BINDING", group + "/" + window}, {"MESH_HOME", s.Root}, {"MESH_USER_HOME", s.UserHome}} {
			settings = append(settings, "-c", "shell_environment_policy.set."+entry[0]+"="+strconv.Quote(entry[1]))
		}
		args = append(settings, args...)
	}
	exe, e := executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(agent, args...)
	cmd.Dir = project
	c, e := s.Config()
	if e != nil {
		return e
	}
	binding := ""
	if group != "" {
		binding = group + "/" + window
	}
	cmd.Env = childEnv(map[string]string{"MESH_BINDING": binding, "MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome, "MESH_CONTROL_SOCKET": socket, "MESH_CONTROL_TOKEN": token, "MESH_WINDOW": window, "MESH_SESSION_ID": group, "MESH_ACTIVE": "1", "MESH_MACHINE": c.Self.Name, "MESH_AGENT": provider, "PATH": filepath.Dir(exe) + string(os.PathListSeparator) + os.Getenv("PATH")})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runtime := SessionRuntime{Active: true, Group: group, Window: window, Machine: c.Self.Name, Agent: provider, Project: project, StartedAt: now(), FirstHandoff: handoffID}
	if group != "" {
		if e := writeJSON(s.runtimePath(group, window), runtime); e != nil {
			return e
		}
		defer func() {
			runtime.Active, runtime.FinishedAt = false, now()
			_ = writeJSON(s.runtimePath(group, window), runtime)
		}()
	}
	if e := cmd.Start(); e != nil {
		return e
	}
	runtime.PID = cmd.Process.Pid
	if group != "" {
		_ = writeJSON(s.runtimePath(group, window), runtime)
	}
	return interactiveResult(cmd.Wait())
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
