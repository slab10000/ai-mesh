package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const MaxHandoffContext = 256 << 10

type Handoff struct {
	ID      string `json:"id"`
	Context string `json:"context"`
	Inputs  []File `json:"inputs,omitempty"`
	Group   string `json:"group"`
	Window  string `json:"window"`
	Source  string `json:"source"`
	Agent   string `json:"agent"`
	Project string `json:"project,omitempty"`
}

type HandoffReceipt struct {
	ID          string `json:"id"`
	Group       string `json:"group"`
	Window      string `json:"window"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Agent       string `json:"agent"`
	Project     string `json:"project"`
	ContextPath string `json:"context_path"`
	InputsPath  string `json:"inputs_path"`
	CreatedAt   string `json:"created_at"`
	ReadAt      string `json:"read_at,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

type HandoffRead struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	ReadAt      string `json:"read_at"`
}

func (r *HandoffReceipt) loadReadMarker() error {
	if r.ReadAt != "" { // Compatibility with receipts acknowledged by older builds.
		return nil
	}
	var marker HandoffRead
	err := readJSON(filepath.Join(filepath.Dir(r.ContextPath), "READ.json"), &marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if marker.ID != r.ID || marker.Fingerprint != r.Fingerprint || marker.ReadAt == "" {
		return errors.New("invalid handoff acknowledgment")
	}
	r.ReadAt = marker.ReadAt
	return nil
}

type SessionRuntime struct {
	Active       bool   `json:"active"`
	Group        string `json:"group"`
	Window       string `json:"window"`
	Machine      string `json:"machine"`
	Agent        string `json:"agent"`
	Project      string `json:"project"`
	PID          int    `json:"pid"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	FirstHandoff string `json:"first_handoff,omitempty"`
}

func (s *Store) runtimePath(group, window string) string {
	return s.path("runtime", group, window+".json")
}

// Runtime identity is separate from authentication. It is safe to show to the
// agent/user; the controller token is never returned by these commands.
func (s *Store) CurrentSession() (SessionRuntime, error) {
	group, window := os.Getenv("MESH_SESSION_ID"), os.Getenv("MESH_WINDOW")
	if group == "" || window == "" {
		return SessionRuntime{Active: false}, nil
	}
	if !validID.MatchString(group) || !validID.MatchString(window) {
		return SessionRuntime{}, errors.New("invalid Mesh session identity")
	}
	var runtime SessionRuntime
	if err := readJSON(s.runtimePath(group, window), &runtime); err != nil {
		return runtime, err
	}
	return runtime, nil
}

func (s *Store) StageHandoff(h Handoff) (HandoffReceipt, error) {
	var result HandoffReceipt
	if !validID.MatchString(h.ID) || !validID.MatchString(h.Group) || !validID.MatchString(h.Window) || !validID.MatchString(h.Source) {
		return result, errors.New("invalid handoff identity")
	}
	if h.Agent != "codex" && h.Agent != "claude" {
		return result, errors.New("conversation handoff currently supports codex and claude")
	}
	if strings.TrimSpace(h.Context) == "" || len(h.Context) > MaxHandoffContext {
		return result, errors.New("handoff requires a nonempty context brief of at most 256 KiB")
	}
	if err := validateFiles(h.Inputs); err != nil {
		return result, err
	}
	if len(h.Project) > 4096 || strings.ContainsAny(h.Project, "\x00\r\n") {
		return result, errors.New("invalid project path")
	}
	if _, err := exec.LookPath(h.Agent); err != nil {
		return result, fmt.Errorf("%s is not installed on this computer", h.Agent)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return result, errors.New("tmux is required on the destination")
	}
	c, err := s.Config()
	if err != nil {
		return result, err
	}
	project := h.Project
	// A window initially opened by the computer menu can have an empty saved
	// project (meaning the remote home). The live provider's actual workspace
	// is authoritative; never stage its inbox in a new managed workspace.
	var live SessionRuntime
	if readJSON(s.runtimePath(h.Group, h.Window), &live) == nil && live.Active {
		if live.Agent != h.Agent {
			return result, errors.New("handoff provider differs from the live conversation")
		}
		project = live.Project
	}
	if project == "" {
		project = s.path("projects", h.Group)
	} else if project == "~" {
		project = s.UserHome
	} else if strings.HasPrefix(project, "~/") {
		project = filepath.Join(s.UserHome, project[2:])
	} else if !filepath.IsAbs(project) {
		project = filepath.Join(s.UserHome, project)
	}
	project = filepath.Clean(project)
	// Normalize before hashing so a retry using the resolved project is equal.
	h.Project = project
	b, _ := json.Marshal(h)
	fingerprint := digest(b)
	err = withLock(s.path("handoffs", h.ID+".lock"), func() error {
		file := s.path("handoffs", h.ID+".json")
		if err := readJSON(file, &result); err == nil {
			if result.Fingerprint != fingerprint {
				return errors.New("handoff ID already exists with different context or files")
			}
			return result.loadReadMarker()
		} else if !os.IsNotExist(err) {
			return err
		}
		directory := filepath.Join(".mesh", "handoffs", h.ID)
		if err := ensureNoSymlinks(project, filepath.ToSlash(directory)); err != nil {
			return err
		}
		files := []File{{Path: "CONTEXT.md", Data: []byte(h.Context), SHA256: digest([]byte(h.Context))}}
		for _, input := range h.Inputs {
			input.Path = "inputs/" + input.Path
			files = append(files, input)
		}
		root := filepath.Join(project, directory)
		if err := materialize(root, files); err != nil {
			return err
		}
		if err := ensureNoSymlinks(root, "inputs"); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(root, "inputs"), 0700); err != nil {
			return err
		}
		result = HandoffReceipt{ID: h.ID, Group: h.Group, Window: h.Window, Source: h.Source, Destination: c.Self.Name, Agent: h.Agent, Project: project, ContextPath: filepath.Join(root, "CONTEXT.md"), InputsPath: filepath.Join(root, "inputs"), CreatedAt: now(), Fingerprint: fingerprint}
		return writeJSON(file, result)
	})
	return result, err
}

func (s *Store) Inbox(read bool) ([]map[string]any, error) {
	runtime, err := s.CurrentSession()
	if err != nil {
		return nil, err
	}
	if !runtime.Active {
		return nil, errors.New("inbox requires an agent launched through Mesh")
	}
	entries, err := os.ReadDir(s.path("handoffs"))
	if os.IsNotExist(err) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var receipts []HandoffReceipt
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var receipt HandoffReceipt
		if err := readJSON(s.path("handoffs", entry.Name()), &receipt); err != nil {
			return nil, err
		}
		if receipt.Group == runtime.Group && receipt.Window == runtime.Window {
			if err := receipt.loadReadMarker(); err != nil {
				return nil, err
			}
			if receipt.ReadAt == "" {
				receipts = append(receipts, receipt)
			}
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].CreatedAt < receipts[j].CreatedAt })
	rows := []map[string]any{}
	for _, receipt := range receipts {
		row := map[string]any{"handoff": receipt}
		if read {
			context, err := os.ReadFile(receipt.ContextPath)
			if err != nil {
				return nil, err
			}
			row["context"] = string(context)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Store) AcknowledgeHandoff(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid handoff ID")
	}
	runtime, err := s.CurrentSession()
	if err != nil {
		return err
	}
	var receipt HandoffReceipt
	if err := readJSON(s.path("handoffs", id+".json"), &receipt); err != nil {
		return err
	}
	if !runtime.Active || receipt.Group != runtime.Group || receipt.Window != runtime.Window {
		return errors.New("handoff belongs to another conversation")
	}
	// Consuming project context only needs project write access. Keep the
	// immutable delivery receipt in global state and the read marker beside
	// the brief; do not require an agent to write outside its workspace.
	directory := filepath.Dir(receipt.ContextPath)
	for _, name := range []string{"READ.json", ".read.lock"} {
		if err := ensureNoSymlinks(directory, name); err != nil {
			return err
		}
	}
	return withLock(filepath.Join(directory, ".read.lock"), func() error {
		if err := receipt.loadReadMarker(); err != nil {
			return err
		}
		if receipt.ReadAt != "" {
			return nil
		}
		return writeJSON(filepath.Join(directory, "READ.json"), HandoffRead{ID: receipt.ID, Fingerprint: receipt.Fingerprint, ReadAt: now()})
	})
}

func (s *Store) HandoffCLI(args []string) error {
	if err := need(args, 1, "mesh handoff MACHINE --context FILE [--input PATH] [--project DIR] [--id ID]"); err != nil {
		return err
	}
	target := args[0]
	f := flags("handoff")
	contextFile := f.String("context", "", "conversation brief prepared by the current agent")
	project := f.String("project", "", "destination project (defaults to a managed workspace for a new session)")
	agent := f.String("agent", "", "codex or claude; defaults to current provider")
	id := f.String("id", "", "reuse the same ID after an uncertain handoff")
	var inputs stringsFlag
	f.Var(&inputs, "input", "selected file or directory (repeatable)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *contextFile == "" || len(f.Args()) != 0 {
		return errors.New("handoff requires --context FILE and flags before positional text")
	}
	context, err := os.ReadFile(*contextFile)
	if err != nil {
		return err
	}
	if len(context) > MaxHandoffContext || strings.TrimSpace(string(context)) == "" {
		return errors.New("context brief must contain 1 to 256 KiB")
	}
	files, err := gatherFiles(inputs)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = newID()
	}
	if !validID.MatchString(*id) {
		return errors.New("invalid handoff ID")
	}
	socket, token, window, err := controlEnvironment()
	if err != nil {
		return err
	}
	request := SwitchRequest{Target: target, Agent: *agent, Project: *project, Window: window, Handoff: &Handoff{ID: *id, Context: string(context), Inputs: files}}
	body, err := controlCall(socket, token, "/handoff", request)
	if err != nil {
		return fmt.Errorf("handoff %s was not confirmed: %w; retry the same command with --id %s", *id, err, *id)
	}
	fmt.Print(string(body))
	return nil
}

func (s *Store) handleHandoff(out http.ResponseWriter, group *SessionGroup, current SessionWindow, request SwitchRequest) error {
	if request.Handoff == nil || request.Back {
		return errors.New("handoff requires a context brief and a destination")
	}
	if !validID.MatchString(request.Handoff.ID) || strings.TrimSpace(request.Handoff.Context) == "" || len(request.Handoff.Context) > MaxHandoffContext {
		return errors.New("handoff requires a valid ID and a nonempty context brief of at most 256 KiB")
	}
	if err := validateFiles(request.Handoff.Inputs); err != nil {
		return err
	}
	if len(request.Project) > 4096 || strings.ContainsAny(request.Project, "\x00\r\n") {
		return errors.New("invalid project path")
	}
	c, err := s.Config()
	if err != nil {
		return err
	}
	peer, local, err := c.Resolve(request.Target)
	if err != nil {
		return err
	}
	if !local && !peer.Incoming {
		return errors.New("destination declines incoming access")
	}
	source, _, err := c.Resolve(current.MachineID)
	if err != nil {
		return err
	}
	agent := request.Agent
	if agent == "" {
		agent = current.Agent
	}
	if agent != "codex" && agent != "claude" {
		return errors.New("handoff supports codex and claude")
	}
	project := request.Project
	if project != "" {
		project = resolveProject(peer.Home, project)
	}
	index := -1
	for i, w := range group.Windows {
		if w.MachineID == peer.ID && w.Agent == agent && (project == "" || resolveProject(peer.Home, w.Project) == project) {
			index = i
			break
		}
	}
	if index >= 0 && group.Windows[index].ID == current.ID {
		return errors.New("already in the destination conversation")
	}
	fresh := index < 0
	if fresh {
		window := SessionWindow{ID: "w" + newID()[:8], MachineID: peer.ID, Agent: agent, Project: project, HandoffID: request.Handoff.ID, Pending: true}
		group.Windows = append(group.Windows, window)
		index = len(group.Windows) - 1
		// Reserve the window identity before network I/O, so a lost stage reply
		// can be retried without creating a different destination conversation.
		if err := s.saveSession(*group); err != nil {
			return err
		}
	}
	window := group.Windows[index]
	reused := !fresh && !window.Pending
	if reused {
		window.Project = resolveProject(peer.Home, window.Project)
	}
	if window.Pending && window.HandoffID != request.Handoff.ID {
		// No provider has started in this reserved window. A new handoff can
		// replace an abandoned attempt without destroying a live conversation.
		window.HandoffID = request.Handoff.ID
		group.Windows[index] = window
		if err := s.saveSession(*group); err != nil {
			return err
		}
	}
	handoff := *request.Handoff
	handoff.Group, handoff.Window, handoff.Source, handoff.Agent, handoff.Project = group.ID, window.ID, source.Name, agent, window.Project
	var receipt HandoffReceipt
	if local {
		receipt, err = s.StageHandoff(handoff)
	} else {
		err = s.Call(peer, Request{Action: "handoff", Handoff: &handoff}, &receipt)
	}
	if err != nil {
		return err
	}
	window.Project, window.Pending = receipt.Project, false
	group.Windows[index] = window
	if err := s.saveSession(*group); err != nil {
		return err
	}
	pane := "mesh-" + group.ID + ":" + window.ID
	name, paneErr := s.tmux("display-message", "-p", "-t", pane, "#{window_name}").Output()
	if paneErr != nil || strings.TrimSpace(string(name)) != window.ID {
		if err := s.createWindow(*group, window, false); err != nil {
			return err
		}
	} else if dead, err := s.tmux("display-message", "-p", "-t", pane, "#{pane_dead}").Output(); err == nil && strings.TrimSpace(string(dead)) == "1" {
		if err := s.tmuxRun("respawn-pane", "-t", pane); err != nil {
			return err
		}
	}
	if len(group.Back) == 0 || group.Back[len(group.Back)-1] != current.ID {
		group.Back = append(group.Back, current.ID)
	}
	if err := s.saveSession(*group); err != nil {
		return err
	}
	// Keep old processes running. An existing agent reads the new inbox at
	// its next user turn; never type blindly into a possibly busy agent TUI.
	out.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(out).Encode(map[string]any{"handoff": receipt, "session_reused": reused, "context_delivery": "inbox", "source_session_preserved": true})
	if f, ok := out.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(350 * time.Millisecond)
		if err := s.tmuxRun("select-window", "-t", pane); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	return nil
}

func resolveProject(home, project string) string {
	if project == "" || project == "~" {
		return home
	}
	if strings.HasPrefix(project, "~/") {
		return filepath.Join(home, project[2:])
	}
	if !filepath.IsAbs(project) {
		return filepath.Join(home, project)
	}
	return filepath.Clean(project)
}
