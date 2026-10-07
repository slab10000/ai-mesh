package mesh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func uninstallFixture(t *testing.T) *Store {
	t.Helper()
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "MESH_INSTALL_DIR", "MESH_BINDING", "MESH_SESSION_ID", "MESH_WINDOW"} {
		t.Setenv(key, "")
	}
	return fixture(t, "uninstall", true)
}
func putUninstallFile(t *testing.T, path, body string) {
	t.Helper()
	if err := atomicWrite(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("expected absent %s, got %v", path, err)
		}
	}
}
func assertContents(t *testing.T, path, expected string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != expected {
		t.Fatalf("%s = %q, %v; want %q", path, b, err, expected)
	}
}
func uninstallLocal(t *testing.T, s *Store) {
	t.Helper()
	if err := s.uninstall(uninstallOptions{Yes: true, LocalOnly: true}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallCompleteLocalAndIdempotent(t *testing.T) {
	s := uninstallFixture(t)
	ssh := filepath.Join(s.UserHome, ".ssh", "config")
	instructions := filepath.Join(s.UserHome, ".codex", "AGENTS.md")
	override := filepath.Join(s.UserHome, ".codex", "AGENTS.override.md")
	keys := filepath.Join(s.UserHome, ".ssh", "authorized_keys")
	block := beginInstructions + "\nmesh instructions\n" + endInstructions + "\n"
	putUninstallFile(t, instructions, "my instructions\n"+block+"later changes\n")
	putUninstallFile(t, override, block)
	putUninstallFile(t, ssh, beginSSHConfig+"\nHost peer\n"+endSSHConfig+"\nHost personal\n  HostName example.test\n")
	putUninstallFile(t, keys, "# keep comment\nssh-ed25519 personal\nssh-ed25519 key ai-mesh-managed:peer\n\n")
	putUninstallFile(t, filepath.Join(s.UserHome, ".ssh", "known_hosts"), "keep host\n")
	for _, file := range []string{instructions, override, ssh} {
		putUninstallFile(t, file+".pre-mesh", "backup")
		putUninstallFile(t, file+".mesh.lock", "")
	}
	project := filepath.Join(s.UserHome, "work")
	handoffDir := filepath.Join(project, ".mesh", "handoffs", "handoff1")
	putUninstallFile(t, filepath.Join(handoffDir, "CONTEXT.md"), "brief")
	putUninstallFile(t, filepath.Join(handoffDir, "inputs", "input.txt"), "input")
	h := HandoffReceipt{ID: "handoff1", Project: project, ContextPath: filepath.Join(handoffDir, "CONTEXT.md"), InputsPath: filepath.Join(handoffDir, "inputs")}
	if err := writeJSON(s.path("handoffs", "handoff1.json"), h); err != nil {
		t.Fatal(err)
	}
	putUninstallFile(t, filepath.Join(project, "source.go"), "project source")
	putUninstallFile(t, filepath.Join(project, ".mesh", "user-file"), "personal")
	output := filepath.Join(s.UserHome, "results", "report.pdf")
	putUninstallFile(t, output, "delivered result")
	for _, name := range []string{"mesh-darwin-arm64", "mesh-darwin-amd64", "mesh-linux-arm64", "mesh-linux-amd64"} {
		putUninstallFile(t, filepath.Join(s.UserHome, ".local", "bin", name), "binary")
	}
	putUninstallFile(t, filepath.Join(s.UserHome, ".local", "bin", "other-tool"), "keep")
	socketDir := filepath.Join(os.TempDir(), "mesh-"+digest([]byte(s.Root))[:12])
	putUninstallFile(t, filepath.Join(socketDir, "stale.sock"), "stale")
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	uninstallLocal(t, s)
	assertAbsent(t, s.Root, override, override+".mesh.lock", instructions+".pre-mesh", ssh+".pre-mesh", handoffDir, socketDir, filepath.Join(s.UserHome, ".local", "bin", "mesh"), filepath.Join(s.UserHome, ".local", "bin", "mesh-linux-amd64"))
	assertContents(t, instructions, "my instructions\nlater changes\n")
	assertContents(t, ssh, "Host personal\n  HostName example.test\n")
	assertContents(t, keys, "# keep comment\nssh-ed25519 personal\n\n")
	assertContents(t, filepath.Join(s.UserHome, ".ssh", "known_hosts"), "keep host\n")
	assertContents(t, filepath.Join(project, "source.go"), "project source")
	assertContents(t, filepath.Join(project, ".mesh", "user-file"), "personal")
	assertContents(t, output, "delivered result")
	assertContents(t, filepath.Join(s.UserHome, ".local", "bin", "other-tool"), "keep")
	uninstallLocal(t, s)
	assertAbsent(t, s.Root)
}

func TestUninstallPreviewCancellationAndBadFlagsAreReadOnly(t *testing.T) {
	s := uninstallFixture(t)
	file := filepath.Join(s.UserHome, ".codex", "AGENTS.md")
	body := beginInstructions + "\nbody\n" + endInstructions + "\n"
	putUninstallFile(t, file, body)
	for _, o := range []uninstallOptions{{DryRun: true}, {}} {
		var out bytes.Buffer
		err := s.uninstall(o, strings.NewReader("no\n"), &out)
		if !o.DryRun && (err == nil || !strings.Contains(err.Error(), "cancelled")) {
			t.Fatalf("cancel: %v", err)
		}
		if o.DryRun && err != nil {
			t.Fatal(err)
		}
		assertContents(t, file, body)
		if _, err := s.Config(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UninstallCLI([]string{"--yes", "unexpected"}); err == nil {
		t.Fatal("accepted positional argument")
	}
	if err := s.UninstallCLI([]string{"--all", "--local-only"}); err == nil {
		t.Fatal("accepted conflicting flags")
	}
}

func TestUninstallSymlinksAndHistoricalIntegrationPaths(t *testing.T) {
	s := uninstallFixture(t)
	target := filepath.Join(s.UserHome, "dotfiles", "rules.md")
	link := filepath.Join(s.UserHome, ".codex", "AGENTS.md")
	block := beginInstructions + "\nbody\n" + endInstructions + "\n"
	putUninstallFile(t, target, "keep\n"+block)
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(s.UserHome, "old-provider-home", "AGENTS.md")
	putUninstallFile(t, custom, block)
	for _, p := range []string{target, link, custom} {
		putUninstallFile(t, p+".mesh.lock", "")
		putUninstallFile(t, p+".pre-mesh", "old")
	}
	if err := s.recordIntegrationPaths(link, target, custom); err != nil {
		t.Fatal(err)
	}
	uninstallLocal(t, s)
	assertContents(t, target, "keep\n")
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("lost shared symlink:", err)
	}
	assertAbsent(t, custom, target+".pre-mesh", link+".mesh.lock")
}

func TestUninstallRejectsUnsafeRootsAndHandoffPaths(t *testing.T) {
	for _, kind := range []string{"home", "ancestor", "symlink", "custom", "custom-shared", "handoff", "handoff-symlink", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			s := uninstallFixture(t)
			original := s.Root
			marker := filepath.Join(s.UserHome, "keep")
			putUninstallFile(t, marker, "safe")
			switch kind {
			case "home":
				s.Root = s.UserHome
			case "ancestor":
				s.Root = filepath.Dir(s.UserHome)
			case "symlink":
				s.Root = filepath.Join(s.UserHome, "alias")
				if err := os.Symlink(original, s.Root); err != nil {
					t.Fatal(err)
				}
			case "custom":
				s.Root = filepath.Join(s.UserHome, "work")
				putUninstallFile(t, filepath.Join(s.Root, "keep"), "safe")
			case "custom-shared":
				s.Root = filepath.Join(s.UserHome, "shared")
				if err := os.Rename(original, s.Root); err != nil {
					t.Fatal(err)
				}
				original = s.Root
				putUninstallFile(t, s.path("user-source.go"), "safe")
			case "malformed":
				putUninstallFile(t, filepath.Join(s.UserHome, ".ssh", "config"), beginSSHConfig+"\nmissing end")
			default:
				project := filepath.Join(s.UserHome, "project")
				dir := filepath.Join(project, ".mesh", "handoffs", "h1")
				h := HandoffReceipt{ID: "h1", Project: project, ContextPath: filepath.Join(dir, "CONTEXT.md"), InputsPath: filepath.Join(dir, "inputs")}
				if kind == "handoff" {
					h.ContextPath = marker
				} else {
					if err := os.MkdirAll(project, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(s.UserHome, filepath.Join(project, ".mesh")); err != nil {
						t.Fatal(err)
					}
				}
				if err := writeJSON(filepath.Join(original, "handoffs", "h1.json"), h); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.uninstall(uninstallOptions{Yes: true, LocalOnly: true}, strings.NewReader(""), io.Discard); err == nil {
				t.Fatal("unsafe uninstall succeeded")
			}
			assertContents(t, marker, "safe")
			if _, err := os.Stat(filepath.Join(original, "config.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUninstallLocalRevokesPeerAccess(t *testing.T) {
	a := uninstallFixture(t)
	b := fixture(t, "peer", true)
	if err := a.ImportPeers([]Peer{peerOf(b)}); err != nil {
		t.Fatal(err)
	}
	if err := b.ImportPeers([]Peer{peerOf(a)}); err != nil {
		t.Fatal(err)
	}
	id := peerOf(a).ID
	fakeSSH(t, a, b)
	if err := a.uninstall(uninstallOptions{Yes: true}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, a.Root)
	c, err := b.Config()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Peers[id]; ok {
		t.Fatal("peer still trusts uninstalled machine")
	}
	keys, err := os.ReadFile(filepath.Join(b.UserHome, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(keys), id) {
		t.Fatal("managed SSH key retained")
	}
}

func TestUninstallAllPreflightsAndRemovesPeers(t *testing.T) {
	a := uninstallFixture(t)
	b, c := fixture(t, "peer-b", true), fixture(t, "peer-c", true)
	for _, s := range []*Store{a, b, c} {
		for _, peer := range []*Store{a, b, c} {
			if s != peer {
				if err := s.ImportPeers([]Peer{peerOf(peer)}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	fakeSSH(t, a, b, c)
	putUninstallFile(t, c.path("offline"), "offline")
	if err := a.uninstall(uninstallOptions{All: true, Yes: true}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("offline preflight succeeded")
	}
	for _, s := range []*Store{a, b, c} {
		if _, err := s.Config(); err != nil {
			t.Fatal("preflight mutated state:", err)
		}
	}
	if err := os.Remove(c.path("offline")); err != nil {
		t.Fatal(err)
	}
	if err := a.uninstall(uninstallOptions{All: true, DryRun: true}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{a, b, c} {
		if _, err := s.Config(); err != nil {
			t.Fatal("preview mutated state:", err)
		}
	}
	if err := a.uninstall(uninstallOptions{All: true, Yes: true}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{a, b, c} {
		assertAbsent(t, s.Root, filepath.Join(s.UserHome, ".local", "bin", "mesh"))
	}
}

func TestUninstallAllResumesConfirmedRemoteCleanup(t *testing.T) {
	a := uninstallFixture(t)
	b, c := fixture(t, "first", true), fixture(t, "second", true)
	if peerOf(b).ID > peerOf(c).ID {
		b, c = c, b
	}
	if err := a.ImportPeers([]Peer{peerOf(b), peerOf(c)}); err != nil {
		t.Fatal(err)
	}
	fakeSSH(t, a, b, c)
	underlying, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "fail")
	putUninstallFile(t, marker, "fail after preflight")
	t.Setenv("MESH_UNINSTALL_SSH", underlying)
	t.Setenv("MESH_UNINSTALL_FAIL_HOST", peerOf(c).Endpoint.Host)
	t.Setenv("MESH_UNINSTALL_FAIL_MARKER", marker)
	script := `#!/usr/bin/python3
import os, sys
if sys.argv[-2] == os.environ['MESH_UNINSTALL_FAIL_HOST'] and "'--dry-run'" not in sys.argv[-1] and os.path.exists(os.environ['MESH_UNINSTALL_FAIL_MARKER']):
    sys.exit(255)
os.execv(os.environ['MESH_UNINSTALL_SSH'], [os.environ['MESH_UNINSTALL_SSH']] + sys.argv[1:])
`
	if err := atomicWrite(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := a.uninstall(uninstallOptions{All: true, Yes: true}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("ignored remote removal failure")
	}
	assertAbsent(t, b.Root)
	if _, err := a.Config(); err != nil {
		t.Fatal("lost local retry state:", err)
	}
	if _, err := c.Config(); err != nil {
		t.Fatal("changed failed destination:", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := a.uninstall(uninstallOptions{All: true, Yes: true}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, a.Root, c.Root)
}

func TestUninstallStopsRunningAndQueuedJobsAndManualDaemon(t *testing.T) {
	s := uninstallFixture(t)
	if err := s.updateConfig(func(c *Config) error { c.MaxJobs = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	first, err := s.Accept(taskFor(s, "sh", "-c", "sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Accept(taskFor(s, "sh", "-c", "sleep 60"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := s.Job(first.Task.ID)
		if j.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(s.Root); err == nil {
			_ = s.Cancel(first.Task.ID)
			_ = s.Cancel(second.Task.ID)
		}
	})
	cmd := cliCmd(t, s, "daemon", "--interval", "1h")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	for {
		if _, err := os.Stat(s.path("daemon-status.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	uninstallLocal(t, s)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("daemon: %v: %s", err, log.String())
	}
	assertAbsent(t, s.Root)
	time.Sleep(200 * time.Millisecond)
	assertAbsent(t, s.Root)
}

func TestUninstallNeverSignalsStaleDaemonPID(t *testing.T) {
	s := uninstallFixture(t)
	lock, err := os.OpenFile(s.path("daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(s.path("daemon-status.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := s.stopUninstallDaemon(); err == nil {
		t.Fatal("accepted unrelated PID")
	}
}

func TestUninstallStopsOnlyItsTmuxServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	s := uninstallFixture(t)
	other := fixture(t, "other", true)
	for _, store := range []*Store{s, other} {
		if err := store.tmuxRun("new-session", "-d", "-s", "test", "sleep 60"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.tmux("kill-server").Run() })
	}
	uninstallLocal(t, s)
	if s.tmux("has-session", "-t", "test").Run() == nil {
		t.Fatal("Mesh session still running")
	}
	if other.tmux("has-session", "-t", "test").Run() != nil {
		t.Fatal("unrelated tmux server stopped")
	}
}

func TestUninstallServiceFailureRetainsRetryState(t *testing.T) {
	s := uninstallFixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "commands")
	t.Setenv("MESH_UNINSTALL_TEST_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	service := filepath.Join(s.UserHome, ".config", "systemd", "user", "ai-mesh.service")
	tool := "systemctl"
	if runtime.GOOS == "darwin" {
		service = filepath.Join(s.UserHome, "Library", "LaunchAgents", "dev.ai-mesh.agent.plist")
		tool = "launchctl"
	}
	body, err := serviceDefinition(runtime.GOOS, "/mesh", s.Root, s.UserHome, "/bin")
	if err != nil {
		t.Fatal(err)
	}
	putUninstallFile(t, service, body)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MESH_UNINSTALL_TEST_LOG\"\n"
	if runtime.GOOS == "darwin" {
		script += "[ \"$1\" = print ] && exit 0\n"
	}
	putUninstallFile(t, filepath.Join(bin, tool), script+"exit 1\n")
	if err := os.Chmod(filepath.Join(bin, tool), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.uninstall(uninstallOptions{Yes: true, LocalOnly: true}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("ignored service failure")
	}
	if _, err := s.Config(); err != nil {
		t.Fatal(err)
	}
	assertContents(t, service, body)
	putUninstallFile(t, filepath.Join(bin, tool), script+"exit 0\n")
	if err := os.Chmod(filepath.Join(bin, tool), 0700); err != nil {
		t.Fatal(err)
	}
	uninstallLocal(t, s)
	assertAbsent(t, service, s.Root)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), map[bool]string{true: "bootout", false: "disable --now"}[runtime.GOOS == "darwin"]) {
		t.Fatalf("missing stop command: %s", b)
	}
}

func TestUninstallRemoteIdentityMismatch(t *testing.T) {
	s := uninstallFixture(t)
	err := s.uninstall(uninstallOptions{Yes: true, LocalOnly: true, ExpectID: "different"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("identity check: %v", err)
	}
	if _, err := s.Config(); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallUninitializedAndCustomInstallDirectory(t *testing.T) {
	s := uninstallFixture(t)
	if err := os.RemoveAll(s.Root); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(s.UserHome, "custom bin")
	t.Setenv("MESH_INSTALL_DIR", custom)
	putUninstallFile(t, filepath.Join(custom, "mesh"), "mesh executable")
	putUninstallFile(t, filepath.Join(custom, "mesh-linux-amd64"), "cross executable")
	putUninstallFile(t, filepath.Join(custom, "mesh-personal"), "personal")
	uninstallLocal(t, s)
	assertAbsent(t, s.Root, filepath.Join(custom, "mesh"), filepath.Join(custom, "mesh-linux-amd64"))
	assertContents(t, filepath.Join(custom, "mesh-personal"), "personal")
}

func ExampleStore_UninstallCLI() {
	fmt.Println("mesh uninstall --dry-run")
	fmt.Println("mesh uninstall --yes")
	fmt.Println("mesh uninstall --all --yes")
	// Output:
	// mesh uninstall --dry-run
	// mesh uninstall --yes
	// mesh uninstall --all --yes
}

func TestUninstallRunningExecutableInCustomDirectory(t *testing.T) {
	s := uninstallFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(s.UserHome, "custom bin", "mesh")
	if err := atomicWrite(installed, body, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(installed, "uninstall", "--yes", "--local-only")
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v: %s", err, out)
	}
	assertAbsent(t, installed, s.Root)
	if _, err := os.Stat(exe); err != nil {
		t.Fatal("removed original executable:", err)
	}
}

func TestUninstallRejectsServiceForAnotherStateRoot(t *testing.T) {
	s := uninstallFixture(t)
	body, err := serviceDefinition("darwin", "/mesh", s.Root+"-other", s.UserHome, "/bin")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.UserHome, "Library", "LaunchAgents", "dev.ai-mesh.agent.plist")
	putUninstallFile(t, file, body)
	if _, err := s.planUninstall(); err == nil {
		t.Fatal("accepted another installation's service")
	}
	assertContents(t, file, body)
}

func TestUninstallStopsNativeConversationAndController(t *testing.T) {
	for _, controller := range []bool{false, true} {
		t.Run(fmt.Sprint(controller), func(t *testing.T) {
			if _, err := exec.LookPath("tmux"); err != nil {
				t.Skip("tmux unavailable")
			}
			s := uninstallFixture(t)
			fakeInteractiveProvider(t)
			t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
			sockets := filepath.Join(os.TempDir(), "mesh-"+digest([]byte(s.Root))[:12])
			if err := os.MkdirAll(sockets, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(sockets) })
			w := SessionWindow{ID: "wfirst", MachineID: peerOf(s).ID, Agent: "codex", Project: s.UserHome}
			g := SessionGroup{ID: newID(), Socket: filepath.Join(sockets, "s.sock"), Token: newID() + newID(), Windows: []SessionWindow{w}}
			if err := s.saveSession(g); err != nil {
				t.Fatal(err)
			}
			if err := s.createWindow(g, w, true); err != nil {
				t.Fatal(err)
			}
			if controller {
				if err := s.ensureController(g); err != nil {
					t.Fatal(err)
				}
			}
			state := testRuntime(t, s, g.ID, w.ID)
			uninstallLocal(t, s)
			if syscall.Kill(state.PID, 0) == nil {
				t.Fatal("provider survived uninstall")
			}
			assertAbsent(t, s.Root, sockets)
			time.Sleep(100 * time.Millisecond)
			assertAbsent(t, s.Root)

		})
	}
}
