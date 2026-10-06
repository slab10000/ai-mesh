package mesh

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real tmux control client exercises attachment and active-window state
// without requiring the test runner to own a terminal.
func attachTestFrontend(t *testing.T, s *Store, group string) func() {
	t.Helper()
	cmd := s.tmux("-C", "attach-session", "-t", "mesh-"+group)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Env = childEnv(map[string]string{"TERM": "xterm-256color"})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	closeClient := func() {
		if !closed {
			closed = true
			_ = in.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(closeClient)
	until := time.Now().Add(3 * time.Second)
	for s.requireFrontend(group) != nil {
		if time.Now().After(until) {
			t.Fatal("test frontend did not attach")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return closeClient
}

func TestStaleEnvironmentRoutesToCallingConversation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	fakeInteractiveProvider(t)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	group := func() SessionGroup {
		id := newID()
		w := SessionWindow{ID: "w" + newID()[:8], MachineID: peerOf(s).ID, Agent: "codex", Project: s.UserHome}
		g := SessionGroup{ID: id, Socket: filepath.Join(filepath.Dir(s.UserHome), id[:8]+".sock"), Token: newID() + newID(), Windows: []SessionWindow{w}}
		if err := s.saveSession(g); err != nil {
			t.Fatal(err)
		}
		if err := s.createWindow(g, w, true); err != nil {
			t.Fatal(err)
		}
		attachTestFrontend(t, s, g.ID)
		if err := s.ensureController(g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	old, actual := group(), group()
	w := actual.Windows[0]
	before := testRuntime(t, s, actual.ID, w.ID)
	oldBefore := testRuntime(t, s, old.ID, old.Windows[0].ID)
	exe, _ := os.Executable()
	brief := filepath.Join(s.UserHome, "brief.md")
	if err := os.WriteFile(brief, []byte("Continue the right conversation, including ROUTING_106."), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate a provider's shell snapshot restoring an older (still live,
	// even attached) session and its credentials. Run from the real provider
	// process, so its ancestor identity is authoritative.
	stale := shellJoin("env", "MESH_BINDING=", "MESH_SESSION_ID="+old.ID, "MESH_WINDOW="+old.Windows[0].ID, "MESH_CONTROL_SOCKET="+old.Socket, "MESH_CONTROL_TOKEN="+old.Token, exe)
	project := filepath.Join(s.UserHome, "destination")
	script := stale + " session && " + stale + " handoff local --context " + quote(brief) + " --project " + quote(project)
	if err := os.WriteFile(s.path("command-"+w.ID+".sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Code           int
		Stdout, Stderr string
	}
	until := time.Now().Add(10 * time.Second)
	for readJSON(s.path("command-"+w.ID+".json"), &result) != nil {
		if time.Now().After(until) {
			t.Fatal("provider did not finish handoff")
		}
		time.Sleep(40 * time.Millisecond)
	}
	if result.Code != 0 {
		t.Fatalf("handoff failed: %s", result.Stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	var current SessionRuntime
	var response struct {
		Handoff          HandoffReceipt
		TerminalSwitched bool `json:"terminal_switched"`
	}
	if err := decoder.Decode(&current); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if current.Group != actual.ID || current.Window != w.ID || response.Handoff.Group != actual.ID || !response.TerminalSwitched {
		t.Fatal("stale environment sent context or navigation to another conversation")
	}
	visible, _ := s.frontendWindows(actual.ID)
	oldVisible, _ := s.frontendWindows(old.ID)
	if len(visible) != 1 || visible[0] != response.Handoff.Window || len(oldVisible) != 1 || oldVisible[0] != old.Windows[0].ID {
		t.Fatal("wrong frontend changed")
	}
	if testRuntime(t, s, actual.ID, w.ID).PID != before.PID || testRuntime(t, s, old.ID, old.Windows[0].ID).PID != oldBefore.PID {
		t.Fatal("source or unrelated provider restarted")
	}
	unchanged, _ := s.session(old.ID)
	if len(unchanged.Windows) != 1 {
		t.Fatal("created a destination in the unrelated session")
	}
}

func TestDetachedFrontendCannotReportSuccessfulHandoff(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	fakeInteractiveProvider(t)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	w := SessionWindow{ID: "wsource", MachineID: peerOf(s).ID, Agent: "codex", Project: s.UserHome}
	g := SessionGroup{ID: newID(), Token: newID() + newID(), Socket: filepath.Join(filepath.Dir(s.UserHome), "control.sock"), Windows: []SessionWindow{w}}
	if err := s.saveSession(g); err != nil {
		t.Fatal(err)
	}
	if err := s.createWindow(g, w, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureController(g); err != nil {
		t.Fatal(err)
	}
	h := &Handoff{ID: newID(), Context: "Do not claim an invisible switch."}
	_, err := controlCall(g.Socket, g.Token, "/handoff", SwitchRequest{Window: w.ID, Target: "local", Project: filepath.Join(s.UserHome, "other"), Handoff: h})
	if err == nil || !strings.Contains(err.Error(), "no attached terminal") {
		t.Fatal("detached handoff did not explain failure", err)
	}
	if _, err := os.Stat(s.path("handoffs", h.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("invisible handoff transferred context")
	}
	if err := requestSwitch(g.Socket, g.Token, w.ID, "local", "codex", filepath.Join(s.UserHome, "other"), false); err == nil {
		t.Fatal("detached switch reported success")
	}
}

func TestRestrictedProcessLookupDoesNotTrustStaleIdentity(t *testing.T) {
	s := fixture(t, "local", true)
	runtime := SessionRuntime{Active: true, Group: newID(), Window: "wstale", PID: 12345}
	if err := writeJSON(s.runtimePath(runtime.Group, runtime.Window), runtime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_SESSION_ID", runtime.Group)
	t.Setenv("MESH_WINDOW", runtime.Window)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := s.CurrentSession(); err == nil || !strings.Contains(err.Error(), "normal approval") {
		t.Fatal("unverifiable snapshot identity was presented as the current conversation", err)
	}
	if _, _, _, err := s.controlEnvironment(); err == nil || !strings.Contains(err.Error(), "normal approval") {
		t.Fatal("controller routing trusted an unverifiable snapshot", err)
	}
}

func TestConversationBindingOverridesSharedExecutor(t *testing.T) {
	s := fixture(t, "local", true)
	// The shared worker has no ancestry relationship to the actual provider.
	// Even ps is unavailable; the per-conversation binding still routes it.
	g := SessionGroup{ID: newID(), Token: newID() + newID(), Socket: "/example/current.sock", Windows: []SessionWindow{{ID: "wcurrent"}}}
	if err := s.saveSession(g); err != nil {
		t.Fatal(err)
	}
	runtime := SessionRuntime{Active: true, Group: g.ID, Window: "wcurrent", PID: 12345}
	if err := writeJSON(s.runtimePath(g.ID, runtime.Window), runtime); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_BINDING", g.ID+"/wcurrent")
	t.Setenv("MESH_SESSION_ID", "stalegroup")
	t.Setenv("MESH_WINDOW", "wstale")
	t.Setenv("MESH_CONTROL_SOCKET", "/wrong.sock")
	t.Setenv("MESH_CONTROL_TOKEN", "wrong")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	current, err := s.CurrentSession()
	if err != nil || current.Group != g.ID || current.Window != "wcurrent" {
		t.Fatal("lost explicit conversation binding", err)
	}
	socket, token, window, err := s.controlEnvironment()
	if err != nil || socket != g.Socket || token != g.Token || window != "wcurrent" {
		t.Fatal("used shared worker credentials", err)
	}
	// The destination resolves the same binding to its SSH-forwarded socket.
	if err := os.Remove(s.path("sessions", g.ID+".json")); err != nil {
		t.Fatal(err)
	}
	remote := RemoteSession{ID: g.ID[:12] + "-wcurrent", Group: g.ID, Window: "wcurrent", Socket: "/example/forwarded.sock", Token: g.Token}
	if err := writeJSON(s.path("sessions", "remote-"+remote.ID+".json"), remote); err != nil {
		t.Fatal(err)
	}
	socket, _, window, err = s.controlEnvironment()
	if err != nil || socket != remote.Socket || window != "wcurrent" {
		t.Fatal("lost remote binding", err)
	}
	for _, invalid := range []string{"../escape/wcurrent", "unknown/wcurrent", g.ID + "/other"} {
		t.Setenv("MESH_BINDING", invalid)
		if _, _, _, err := s.controlEnvironment(); err == nil {
			t.Fatal("invalid binding silently fell back to stale identity")
		}
	}
}
