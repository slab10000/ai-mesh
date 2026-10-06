package mesh

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type terminalTest struct {
	input io.WriteCloser
	log   string
	done  chan error
}

func startTestTerminal(t *testing.T, env []string, args ...string) *terminalTest {
	t.Helper()
	// A real controlling PTY is necessary: a tmux control-mode client does
	// not exercise terminal restoration or detach-client -E's exec path.
	script := `import os,pty,sys,select,json,fcntl,termios,struct
pid,master=pty.fork()
if pid==0:
 os.environ['TERM']='xterm-256color'
 os.execv(sys.argv[1],sys.argv[1:])
fcntl.ioctl(master,termios.TIOCSWINSZ,struct.pack('HHHH',30,120,0,0))
pending=b''
try:
 while True:
  ready,_,_=select.select([master,0],[],[],.2)
  if master in ready:
   try:data=os.read(master,65536)
   except OSError:break
   if not data:break
   os.write(1,data)
  if 0 in ready:
   data=os.read(0,65536)
   if not data:break
   pending+=data
   while b'\n' in pending:
    line,pending=pending.split(b'\n',1)
    os.write(master,json.loads(line).encode())
finally:
 os.close(master)
_,status=os.waitpid(pid,0)
sys.exit(os.waitstatus_to_exitcode(status))
`
	cmd := exec.Command("python3", append([]string{"-c", script}, args...)...)
	cmd.Env = env
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terminal.ansi")
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	result := &terminalTest{input: input, log: path, done: make(chan error, 1)}
	go func() { result.done <- cmd.Wait(); _ = log.Close() }()
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill() })
	return result
}

func (p *terminalTest) send(t *testing.T, value string) {
	t.Helper()
	data, _ := json.Marshal(value)
	if _, err := p.input.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (p *terminalTest) text() string {
	data, _ := os.ReadFile(p.log)
	return string(data)
}

func (p *terminalTest) waitText(t *testing.T, text string) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for !strings.Contains(p.text(), text) {
		if time.Now().After(until) {
			t.Fatalf("missing %q in terminal: %q", text, p.text())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func exitProvider(t *testing.T, code int, immediate bool) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,tty,termios,pathlib,json
pathlib.Path(os.environ['MESH_HOME'],'exit-argv-'+os.environ['MESH_SESSION_ID']+'.json').write_text(json.dumps(sys.argv[1:]))
provider=pathlib.Path(sys.argv[0]).name
resume={'codex':'codex resume CODEX_SESSION_106','claude':'claude --resume CLAUDE_SESSION_106','gemini':'gemini --resume GEMINI_SESSION_106'}[provider]
if %t:
 print('Provider authentication required',flush=True)
 sys.exit(%d)
old=termios.tcgetattr(0)
tty.setraw(0)
try:
 os.write(1,b'\x1b[2J\x1b[HAGENT_READY\r\n')
 interrupts=0
 while True:
  key=os.read(0,1)
  if not key:break
  if key==b'\x03':
   interrupts+=1
   if interrupts==1:
    os.write(1,b'TURN_INTERRUPTED\r\n')
   else:
    os.write(1,('\x1b[2J\x1b[HTo resume this conversation:\r\n  '+resume+'\r\n').encode())
    break
finally:
 termios.tcsetattr(0,termios.TCSANOW,old)
sys.exit(%d)
`, immediate, code, code)
	script = strings.ReplaceAll(strings.ReplaceAll(script, "if true:", "if True:"), "if false:", "if False:")
	for _, provider := range []string{"codex", "claude", "gemini"} {
		if err := os.WriteFile(filepath.Join(dir, provider), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAgentExitRestoresTerminalAndNativeResumeMessage(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, remote := range []bool{false, true} {
		for _, provider := range []string{"codex", "claude", "gemini"} {
			t.Run(fmt.Sprintf("%s/remote=%v", provider, remote), func(t *testing.T) {
				a, b := shortSessionFixture(t, "a"), shortSessionFixture(t, "b")
				inventoryPair(t, a, b)
				fakeSSH(t, a, b)
				exitProvider(t, 0, false)
				t.Cleanup(func() { _ = a.tmux("kill-server").Run(); _ = b.tmux("kill-server").Run() })
				target := "local"
				destination := a
				if remote {
					target = "b"
					destination = b
				}
				exe, _ := os.Executable()
				terminal := startTestTerminal(t, childEnv(map[string]string{"MESH_HOME": a.Root, "MESH_USER_HOME": a.UserHome}), exe, provider, target, "--project", destination.UserHome)
				terminal.waitText(t, "AGENT_READY")
				group := onlyTestGroup(t, a)
				window := group.Windows[0]
				runtime := testRuntime(t, destination, group.ID, window.ID)
				// Interrupting a turn must not exit Mesh or restart the provider.
				terminal.send(t, "\x03")
				terminal.waitText(t, "TURN_INTERRUPTED")
				select {
				case err := <-terminal.done:
					t.Fatal("interrupt closed the frontend", err)
				default:
				}
				if testRuntime(t, destination, group.ID, window.ID).PID != runtime.PID {
					t.Fatal("interrupt restarted provider")
				}
				terminal.send(t, "\x03")
				select {
				case err := <-terminal.done:
					if err != nil {
						t.Fatalf("Mesh did not exit cleanly: %v\n%q", err, terminal.text())
					}
				case <-time.After(10 * time.Second):
					for _, store := range []*Store{a, b} {
						data, _ := store.tmux("list-panes", "-a", "-F", "#{session_name} #{pane_id} dead=#{pane_dead} marker=#{@mesh-finished} pid=#{pane_pid}").CombinedOutput()
						t.Logf("store %s panes: %s", store.Root, data)
						data, _ = store.tmux("list-clients", "-F", "#{session_name} #{client_name} #{pane_id}").CombinedOutput()
						t.Logf("clients: %s", data)
					}
					data, _ := exec.Command("ps", "-axo", "pid,ppid,command").Output()
					for _, line := range strings.Split(string(data), "\n") {
						if strings.Contains(line, group.ID[:12]) || strings.Contains(line, a.Root) || strings.Contains(line, b.Root) {
							t.Log(line)
						}
					}
					t.Fatal("stuck after provider exit")
				}
				output := terminal.text()
				index := strings.LastIndex(output, "\x1b[?1049l")
				if index < 0 {
					t.Fatalf("outer terminal was not restored: %q", output)
				}
				tail := output[index:]
				resume := map[string]string{"codex": "codex resume CODEX_SESSION_106", "claude": "claude --resume CLAUDE_SESSION_106", "gemini": "gemini --resume GEMINI_SESSION_106"}[provider]
				if strings.Count(tail, resume) != 1 || strings.Contains(tail, "Pane is dead") || strings.Contains(tail, "mesh:") || strings.Contains(tail, "[detached") {
					t.Fatalf("incorrect final output: %q", tail)
				}
				var ended SessionRuntime
				if readJSON(destination.runtimePath(group.ID, window.ID), &ended) != nil || ended.Active || ended.FinishedAt == "" {
					t.Fatal("provider exit not recorded")
				}
			})
		}
	}
}

func TestImmediateAgentExitAndNonzeroStatus(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, remote := range []bool{false, true} {
		for _, code := range []int{1, 130} {
			t.Run(fmt.Sprintf("code=%d/remote=%v", code, remote), func(t *testing.T) {
				a, b := shortSessionFixture(t, "a"), shortSessionFixture(t, "b")
				inventoryPair(t, a, b)
				fakeSSH(t, a, b)
				exitProvider(t, code, true)
				t.Cleanup(func() { _ = a.tmux("kill-server").Run(); _ = b.tmux("kill-server").Run() })
				target, destination := "local", a
				if remote {
					target, destination = "b", b
				}
				exe, _ := os.Executable()
				p := startTestTerminal(t, childEnv(map[string]string{"MESH_HOME": a.Root, "MESH_USER_HOME": a.UserHome}), exe, "claude", target, "--project", destination.UserHome)
				select {
				case err := <-p.done:
					exit, ok := err.(*exec.ExitError)
					if !ok || exit.ExitCode() != code {
						t.Fatalf("lost provider exit status: %v; output=%q", err, p.text())
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("immediate provider failure hung: %q", p.text())
				}
				if !strings.Contains(p.text(), "Provider authentication required") || strings.Contains(p.text(), "mesh:") {
					t.Fatalf("lost provider diagnostic: %q", p.text())
				}
			})
		}
	}
}

func onlyTestGroup(t *testing.T, s *Store) SessionGroup {
	t.Helper()
	entries, _ := os.ReadDir(s.path("sessions"))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			g, err := s.session(strings.TrimSuffix(entry.Name(), ".json"))
			if err == nil && len(g.Windows) > 0 {
				return g
			}
		}
	}
	t.Fatal("missing session group")
	return SessionGroup{}
}

func waitTestPaneExit(t *testing.T, s *Store, pane string) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for {
		data, _ := s.tmux("display-message", "-p", "-t", pane, "#{pane_dead}").Output()
		if strings.TrimSpace(string(data)) == "1" {
			return
		}
		if time.Now().After(until) {
			t.Fatal("pane did not exit", pane)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestHiddenExitAndManualDetachPreserveLiveAgent(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	exitProvider(t, 0, false)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	exe, _ := os.Executable()
	env := childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	p := startTestTerminal(t, env, exe, "codex", "--project", s.UserHome)
	p.waitText(t, "AGENT_READY")
	group := onlyTestGroup(t, s)
	first := group.Windows[0]
	c, _ := s.Config()
	second := SessionWindow{ID: "wsecond", MachineID: c.Self.ID, Agent: "claude", Project: s.UserHome}
	group.Windows = append(group.Windows, second)
	if err := s.saveSession(group); err != nil {
		t.Fatal(err)
	}
	if err := s.createWindow(group, second, false); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(t, s, group.ID, second.ID)
	session := "mesh-" + group.ID
	if err := s.tmuxRun("select-window", "-t", session+":"+second.ID); err != nil {
		t.Fatal(err)
	}
	// End the hidden agent. Its hook must leave the foreground client alone.
	if err := s.tmuxRun("send-keys", "-t", session+":"+first.ID, "C-c", "C-c"); err != nil {
		t.Fatal(err)
	}
	waitTestPaneExit(t, s, session+":"+first.ID)
	if err := s.TerminalExited(session); err != nil {
		t.Fatal(err)
	}
	clients, _ := s.frontendWindows(group.ID)
	if len(clients) != 1 || clients[0] != second.ID {
		t.Fatalf("hidden exit detached the frontend: %v", clients)
	}
	if testRuntime(t, s, group.ID, second.ID).PID != runtime.PID {
		t.Fatal("live agent was replaced")
	}
	// Ctrl-b d is still a detach, not an agent exit.
	p.send(t, "\x02d")
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("manual detach hung")
	}
	if testRuntime(t, s, group.ID, second.ID).PID != runtime.PID {
		t.Fatal("detach stopped live agent")
	}
	if strings.Contains(p.text(), "CODEX_SESSION_106") {
		t.Fatal("hidden agent's resume hint leaked into foreground")
	}
	// Reattach the exact live conversation, then quit it normally.
	p = startTestTerminal(t, env, exe, "claude", "--resume", "--project", s.UserHome)
	p.waitText(t, "AGENT_READY")
	if testRuntime(t, s, group.ID, second.ID).PID != runtime.PID {
		t.Fatal("resume restarted live agent")
	}
	p.send(t, "\x03\x03")
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("reattached agent exit hung")
	}
	p.waitText(t, "claude --resume CLAUDE_SESSION_106")
}

func TestUpgradeReleasesAlreadyFinishedPane(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	exitProvider(t, 0, false)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	exe, _ := os.Executable()
	p := startTestTerminal(t, childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome}), exe, "codex", "--project", s.UserHome)
	p.waitText(t, "AGENT_READY")
	group := onlyTestGroup(t, s)
	session := "mesh-" + group.ID
	for _, hook := range []string{"pane-died", "client-attached", "after-select-window"} {
		if err := s.tmuxRun("set-hook", "-u", "-t", session, hook); err != nil {
			t.Fatal(err)
		}
	}
	p.send(t, "\x03\x03")
	waitTestPaneExit(t, s, session)
	if err := s.tmuxRun("set-option", "-p", "-u", "-t", session, "@mesh-finished"); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshTerminalExit(session); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("upgrade left the old client stuck")
	}
	if !strings.Contains(p.text()[strings.LastIndex(p.text(), "\x1b[?1049l"):], "codex resume CODEX_SESSION_106") {
		t.Fatal("upgrade lost old resume message")
	}
}

func TestResumeAfterExitUsesNativePicker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	exitProvider(t, 0, false)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	exe, _ := os.Executable()
	env := childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	first := startTestTerminal(t, env, exe, "codex", "--project", s.UserHome)
	first.waitText(t, "AGENT_READY")
	old := onlyTestGroup(t, s)
	first.send(t, "\x03\x03")
	select {
	case err := <-first.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("first exit hung")
	}
	next := startTestTerminal(t, env, exe, "codex", "--project", s.UserHome, "--resume")
	next.waitText(t, "AGENT_READY")
	entries, _ := filepath.Glob(s.path("exit-argv-*.json"))
	if len(entries) != 2 {
		t.Fatal("resume reused the completed pane instead of starting native resume")
	}
	for _, file := range entries {
		if strings.Contains(file, old.ID) {
			continue
		}
		var args []string
		if err := readJSON(file, &args); err != nil {
			t.Fatal(err)
		}
		if len(args) == 0 || args[len(args)-1] != "resume" {
			t.Fatalf("native resume flag missing: %v", args)
		}
	}
	next.send(t, "\x03\x03")
	select {
	case err := <-next.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("resumed exit hung")
	}
}

func TestExitWithMultipleAttachedSessionGroups(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := shortSessionFixture(t, "local")
	exitProvider(t, 0, false)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	exe, _ := os.Executable()
	env := childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	first := startTestTerminal(t, env, exe, "codex", "--project", s.UserHome)
	first.waitText(t, "AGENT_READY")
	firstGroup := onlyTestGroup(t, s)
	second := startTestTerminal(t, env, exe, "claude", "--project", s.UserHome)
	second.waitText(t, "AGENT_READY")
	// The more recently active tmux session must not redirect exit detection.
	first.send(t, "\x03\x03")
	select {
	case err := <-first.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("another attached session prevented the first terminal exiting")
	}
	first.waitText(t, "codex resume CODEX_SESSION_106")
	clients, err := s.tmux("list-clients", "-F", "#{session_name}").Output()
	if err != nil || len(strings.Fields(string(clients))) != 1 || strings.Contains(string(clients), firstGroup.ID) {
		t.Fatalf("wrong client detached: %s %v", clients, err)
	}
	select {
	case err := <-second.done:
		t.Fatal("other conversation exited", err)
	default:
	}
	second.send(t, "\x03\x03")
	select {
	case err := <-second.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("second exit hung")
	}
	second.waitText(t, "claude --resume CLAUDE_SESSION_106")
}

func TestKilledSSHIsNotAProviderExit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	a, b := shortSessionFixture(t, "a"), shortSessionFixture(t, "b")
	inventoryPair(t, a, b)
	fakeSSH(t, a, b)
	exitProvider(t, 0, false)
	t.Cleanup(func() { _ = a.tmux("kill-server").Run(); _ = b.tmux("kill-server").Run() })
	exe, _ := os.Executable()
	p := startTestTerminal(t, childEnv(map[string]string{"MESH_HOME": a.Root, "MESH_USER_HOME": a.UserHome}), exe, "codex", "b", "--project", b.UserHome)
	p.waitText(t, "AGENT_READY")
	group := onlyTestGroup(t, a)
	window := group.Windows[0]
	runtime := testRuntime(t, b, group.ID, window.ID)
	if err := os.WriteFile(b.path("disconnect"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitTestPaneExit(t, a, "mesh-"+group.ID)
	if err := a.TerminalExited("mesh-" + group.ID); err != nil {
		t.Fatal(err)
	}
	clients, _ := a.frontendWindows(group.ID)
	if len(clients) != 1 {
		t.Fatal("killed SSH client was mistaken for an agent exit")
	}
	if testRuntime(t, b, group.ID, window.ID).PID != runtime.PID {
		t.Fatal("transport loss stopped provider")
	}
	if err := os.Remove(b.path("disconnect")); err != nil {
		t.Fatal(err)
	}
	if err := a.SessionControl(group.ID, window.ID, "b", false); err != nil {
		t.Fatal(err)
	}
	if testRuntime(t, b, group.ID, window.ID).PID != runtime.PID {
		t.Fatal("reconnection restarted provider")
	}
	p.send(t, "\x03\x03")
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("reconnected provider exit hung")
	}
}
