package mesh

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeInteractiveProvider(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys, time
root=pathlib.Path(os.environ['MESH_HOME'])
window=os.environ['MESH_WINDOW']
proof=root/('provider-'+window+'.json')
proof.write_text(json.dumps({'pid':os.getpid(),'argv':sys.argv[1:],'machine':os.environ['MESH_MACHINE'],'group':os.environ['MESH_SESSION_ID']}))
while True:
 command=root/('command-'+window+'.sh')
 if command.exists():
  script=command.read_text()
  command.unlink()
  result=subprocess.run(['/bin/sh','-c',script],capture_output=True,text=True)
  (root/('command-'+window+'.json')).write_text(json.dumps({'code':result.returncode,'stdout':result.stdout,'stderr':result.stderr}))
 (root/('heartbeat-'+window)).write_text(str(time.monotonic_ns()))
 time.sleep(.1)
`
	for _, name := range []string{"codex", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testRuntime(t *testing.T, s *Store, group, window string) SessionRuntime {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for {
		var runtime SessionRuntime
		if readJSON(s.runtimePath(group, window), &runtime) == nil && runtime.Active && runtime.PID != 0 {
			return runtime
		}
		if time.Now().After(until) {
			t.Fatalf("native provider did not start: %s/%s", group, window)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// SSH forwards a Unix socket below the remote home, whose path has a small
// OS limit. Real user homes are shorter than Go's descriptive test directories.
func shortSessionFixture(t *testing.T, name string) *Store {
	t.Helper()
	s := fixture(t, name, true)
	dir, err := os.MkdirTemp("/tmp", "mh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	home := filepath.Join(dir, "home")
	if err := os.Rename(s.UserHome, home); err != nil {
		t.Fatal(err)
	}
	s.UserHome, s.Root = home, filepath.Join(home, ".ai-mesh")
	if err := s.updateConfig(func(c *Config) error { c.Self.Home = home; return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHandoffFilesIdempotencyAndScopedInbox(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := fixture(t, "local", true)
	fakeInteractiveProvider(t)
	h := Handoff{ID: newID(), Group: newID(), Window: "wfirst", Source: "other", Agent: "codex", Context: "Decision: use the blue cover.\nNext: finish the report.", Inputs: []File{{Path: "notes.txt", Data: []byte("selected input"), SHA256: digest([]byte("selected input"))}}}
	receipt, err := s.StageHandoff(h)
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(receipt.ContextPath)
	if err != nil || string(text) != h.Context {
		t.Fatal("context was not transferred", err)
	}
	input, err := os.ReadFile(filepath.Join(receipt.InputsPath, "notes.txt"))
	if err != nil || string(input) != "selected input" {
		t.Fatal("selected files were not transferred", err)
	}
	again, err := s.StageHandoff(h)
	if err != nil || again.CreatedAt != receipt.CreatedAt {
		t.Fatal("retry was not idempotent", err)
	}
	changed := h
	changed.Context = "different instructions"
	if _, err := s.StageHandoff(changed); err == nil {
		t.Fatal("same ID accepted a different handoff")
	}
	runtime := SessionRuntime{Active: true, Group: h.Group, Window: h.Window, Machine: "local", Agent: "codex", Project: receipt.Project}
	if err := writeJSON(s.runtimePath(h.Group, h.Window), runtime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_SESSION_ID", h.Group)
	t.Setenv("MESH_WINDOW", h.Window)
	rows, err := s.Inbox(true)
	if err != nil || len(rows) != 1 || rows[0]["context"] != h.Context {
		t.Fatal("inbox lost context", rows, err)
	}
	if err := s.AcknowledgeHandoff(h.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Inbox(true)
	if err != nil || len(rows) != 0 {
		t.Fatal("consumed context appeared again", rows, err)
	}
	if _, err := s.StageHandoff(h); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Inbox(false)
	if len(rows) != 0 {
		t.Fatal("retry reintroduced consumed context")
	}
	runtime.Window = "wsecond"
	if err := writeJSON(s.runtimePath(h.Group, runtime.Window), runtime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_WINDOW", runtime.Window)
	if err := s.AcknowledgeHandoff(h.ID); err == nil {
		t.Fatal("another conversation acknowledged a handoff")
	}
	for _, mutate := range []func(*Handoff){
		func(x *Handoff) { x.Context = "" },
		func(x *Handoff) { x.Group = "../escape" },
		func(x *Handoff) { x.Window = "../escape" },
		func(x *Handoff) { x.Agent = "shell" },
		func(x *Handoff) {
			x.Inputs = []File{{Path: "../escape", Data: []byte("x"), SHA256: digest([]byte("x"))}}
		},
	} {
		bad := h
		bad.ID = newID()
		mutate(&bad)
		if _, err := s.StageHandoff(bad); err == nil {
			t.Fatal("accepted invalid handoff")
		}
	}
	link := filepath.Join(s.UserHome, "linked-project")
	if err := os.Symlink(receipt.Project, link); err != nil {
		t.Fatal(err)
	}
	bad := h
	bad.ID = newID()
	bad.Window = "wnewproject"
	bad.Project = link
	if _, err := s.StageHandoff(bad); err == nil {
		t.Fatal("accepted symlink destination")
	}
}

func TestHandoffAndPlainSwitchPreserveLiveConversations(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	a, b := shortSessionFixture(t, "a"), shortSessionFixture(t, "b")
	inventoryPair(t, a, b)
	fakeSSH(t, a, b)
	fakeInteractiveProvider(t)
	t.Cleanup(func() { _ = a.tmux("kill-server").Run(); _ = b.tmux("kill-server").Run() })
	socketDir, err := os.MkdirTemp("/tmp", "mesh-handoff-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	w := SessionWindow{ID: "wsource", MachineID: peerOf(a).ID, Agent: "codex", Project: a.UserHome}
	g := SessionGroup{ID: newID(), Socket: filepath.Join(socketDir, "c.sock"), Token: newID() + newID(), Windows: []SessionWindow{w}}
	if err := a.saveSession(g); err != nil {
		t.Fatal(err)
	}
	if err := a.createWindow(g, w, true); err != nil {
		t.Fatal(err)
	}
	attachTestFrontend(t, a, g.ID)
	if err := a.ensureController(g); err != nil {
		t.Fatal(err)
	}
	source := testRuntime(t, a, g.ID, w.ID)
	h := &Handoff{ID: newID(), Context: "Carry FORWARD_CONTEXT_42 and continue the same report."}
	request := SwitchRequest{Target: "b", Window: w.ID, Handoff: h}
	// An offline destination must leave the original conversation running.
	if err := os.WriteFile(b.path("offline"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlCall(g.Socket, g.Token, "/handoff", request); err == nil {
		t.Fatal("handoff reported success while destination was offline")
	}
	if testRuntime(t, a, g.ID, w.ID).PID != source.PID {
		t.Fatal("failed handoff interrupted source")
	}
	if err := os.Remove(b.path("offline")); err != nil {
		t.Fatal(err)
	}
	// Lose the reply after files were staged. Retrying must reuse both the
	// receipt and the reserved conversation, without delivering context twice.
	if err := os.WriteFile(b.path("drop-reply"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlCall(g.Socket, g.Token, "/handoff", request); err == nil {
		t.Fatal("lost handoff reply was not reported as uncertain")
	}
	var staged HandoffReceipt
	if err := readJSON(b.path("handoffs", h.ID+".json"), &staged); err != nil {
		t.Fatal("handoff was not staged before the lost reply", err)
	}
	// File transfer can succeed while the interactive SSH connection fails.
	// The source stays visible, and retrying repairs the same destination.
	if err := os.WriteFile(b.path("attach-offline"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := controlCall(g.Socket, g.Token, "/handoff", request); err == nil || !strings.Contains(err.Error(), "context delivered but terminal switch was not confirmed") {
		t.Fatal("failed interactive connection was reported as a completed handoff", err)
	}
	visible, _ := a.frontendWindows(g.ID)
	unchanged, _ := a.session(g.ID)
	if len(visible) != 1 || visible[0] != w.ID || len(unchanged.Back) != 0 {
		t.Fatal("failed display changed the source terminal or its navigation history")
	}
	if err := os.Remove(b.path("attach-offline")); err != nil {
		t.Fatal(err)
	}
	response, err := controlCall(g.Socket, g.Token, "/handoff", request)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Handoff HandoffReceipt `json:"handoff"`
		Reused  bool           `json:"session_reused"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if result.Handoff.Window != staged.Window || result.Handoff.CreatedAt != staged.CreatedAt {
		t.Fatal("uncertain handoff retry changed destination or receipt")
	}
	destination := testRuntime(t, b, g.ID, result.Handoff.Window)
	if destination.Agent != "codex" || destination.FirstHandoff != h.ID {
		t.Fatal("new destination did not inherit handoff", destination)
	}
	var proof struct {
		Args []string `json:"argv"`
	}
	until := time.Now().Add(3 * time.Second)
	for readJSON(b.path("provider-"+destination.Window+".json"), &proof) != nil {
		if time.Now().After(until) {
			t.Fatal("provider proof missing")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(strings.Join(proof.Args, " "), "mesh inbox --read") {
		t.Fatal("new agent lacks handoff startup prompt", proof.Args)
	}
	selectWindow := func(window string) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for {
			out, err := a.tmux("display-message", "-p", "-t", "mesh-"+g.ID, "#{window_name}").Output()
			if err == nil && strings.TrimSpace(string(out)) == window {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("did not select %s: %s %v", window, out, err)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	selectWindow(destination.Window)
	before, _ := os.ReadFile(b.path("handoffs", h.ID+".json"))
	heartbeat, _ := os.ReadFile(b.path("heartbeat-" + destination.Window))
	if err := a.SessionControl("mesh-"+g.ID, destination.Window, "", true); err != nil {
		t.Fatal(err)
	}
	selectWindow(w.ID)
	if err := a.SessionControl("mesh-"+g.ID, w.ID, "b", false); err != nil {
		t.Fatal(err)
	}
	selectWindow(destination.Window)
	if testRuntime(t, a, g.ID, w.ID).PID != source.PID || testRuntime(t, b, g.ID, destination.Window).PID != destination.PID {
		t.Fatal("switch restarted an existing conversation")
	}
	after, _ := os.ReadFile(b.path("handoffs", h.ID+".json"))
	if string(before) != string(after) {
		t.Fatal("plain menu/switch changed the handoff inbox")
	}
	beat, _ := os.ReadFile(b.path("heartbeat-" + destination.Window))
	if string(heartbeat) == string(beat) {
		t.Fatal("background provider stopped running while hidden")
	}
	// An explicit new handoff adds context to the same live destination.
	h2 := &Handoff{ID: newID(), Context: "Additional decision: SECOND_CONTEXT_42."}
	request.Handoff = h2
	response, err = controlCall(g.Socket, g.Token, "/handoff", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Handoff.Window != destination.Window {
		t.Fatal("second handoff replaced the conversation")
	}
	if testRuntime(t, b, g.ID, destination.Window).PID != destination.PID {
		t.Fatal("context delivery restarted provider")
	}
	// Retrying the same handoff neither creates a window nor delivers twice.
	if _, err := controlCall(g.Socket, g.Token, "/handoff", request); err != nil {
		t.Fatal(err)
	}
	fresh, _ := a.session(g.ID)
	if len(fresh.Windows) != 2 {
		t.Fatal("retry duplicated a conversation", fresh.Windows)
	}
	entries, _ := os.ReadDir(b.path("handoffs"))
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != 2 {
		t.Fatal("duplicate context delivery", strconv.Itoa(count))
	}
	if _, err := controlCall(g.Socket, "wrong", "/handoff", request); err == nil {
		t.Fatal("unauthenticated handoff accepted")
	}
}

func TestHandoffUsesExistingProviderWorkspace(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := fixture(t, "local", true)
	fakeInteractiveProvider(t)
	h := Handoff{ID: newID(), Group: newID(), Window: "wmenu", Source: "other", Agent: "codex", Context: "Continue the report."}
	runtime := SessionRuntime{Active: true, Group: h.Group, Window: h.Window, Agent: "codex", Project: s.UserHome, PID: os.Getpid()}
	if err := writeJSON(s.runtimePath(h.Group, h.Window), runtime); err != nil {
		t.Fatal(err)
	}
	// The menu's default project is empty, and older metadata may name a
	// managed directory. Neither should move a running provider's workspace.
	for _, project := range []string{"", s.path("projects", h.Group)} {
		h.ID, h.Project = newID(), project
		r, err := s.StageHandoff(h)
		if err != nil || r.Project != s.UserHome || filepath.Dir(filepath.Dir(filepath.Dir(r.ContextPath))) != filepath.Join(s.UserHome, ".mesh") {
			t.Fatal("inbox does not belong to the live workspace", r, err)
		}
	}
}

func TestHandoffAcknowledgmentRequiresOnlyWorkspaceWrites(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := fixture(t, "local", true)
	fakeInteractiveProvider(t)
	project := filepath.Join(s.UserHome, "report")
	h := Handoff{ID: newID(), Group: newID(), Window: "wproject", Source: "other", Agent: "codex", Project: project, Context: "Continue the report."}
	r, err := s.StageHandoff(h)
	if err != nil {
		t.Fatal(err)
	}
	runtime := SessionRuntime{Active: true, Group: h.Group, Window: h.Window, Agent: "codex", Project: project, PID: os.Getpid()}
	if err := writeJSON(s.runtimePath(h.Group, h.Window), runtime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_SESSION_ID", h.Group)
	t.Setenv("MESH_WINDOW", h.Window)
	before, _ := os.ReadFile(s.path("handoffs", h.ID+".json"))
	// A provider may read global state while writes are restricted to its
	// workspace. No global receipt update or global lock should be needed.
	for _, name := range []string{h.ID + ".json", h.ID + ".lock"} {
		if err := os.Chmod(s.path("handoffs", name), 0400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(s.path("handoffs"), 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(s.path("handoffs"), 0700)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AcknowledgeHandoff(h.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := r.loadReadMarker(); err != nil || r.ReadAt == "" {
		t.Fatal("missing workspace read marker", err)
	}
	rows, err := s.Inbox(true)
	if err != nil || len(rows) != 0 {
		t.Fatal("acknowledged context returned again", err, rows)
	}
	after, _ := os.ReadFile(s.path("handoffs", h.ID+".json"))
	if string(before) != string(after) {
		t.Fatal("acknowledgment changed global state")
	}
}
