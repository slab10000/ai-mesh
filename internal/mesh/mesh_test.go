package mesh

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Child processes exercise the real CLI/worker/controller, without calling any
// installed agent or contacting an SSH server.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		if e := Main(os.Args[1:]); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(t *testing.T, name string, incoming bool) *Store {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(dir, "home")
	s := &Store{Root: filepath.Join(home, ".ai-mesh"), UserHome: home}
	if e := s.ensure(); e != nil {
		t.Fatal(e)
	}
	keyBytes := make([]byte, 51)
	binary.BigEndian.PutUint32(keyBytes[:4], 11)
	copy(keyBytes[4:15], "ssh-ed25519")
	binary.BigEndian.PutUint32(keyBytes[15:19], 32)
	copy(keyBytes[19:], name)
	self := Peer{ID: newID(), Name: name, Endpoint: Endpoint{name + ".test", "tester", 22}, Home: home, Incoming: incoming, PublicKey: "ssh-ed25519 " + base64.StdEncoding.EncodeToString(keyBytes) + " ai-mesh:test"}
	if e := writeJSON(s.path("config.json"), Config{Protocol, self, map[string]Peer{}, 2}); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(s.path("machines", self.ID+".json"), Inventory{ID: self.ID, Name: name, Revision: 1, UpdatedAt: now(), OS: runtime.GOOS, Arch: runtime.GOARCH, Tools: map[string]string{}, Capabilities: map[string]Capability{}}); e != nil {
		t.Fatal(e)
	}
	exe, _ := os.Executable()
	bin := filepath.Join(home, ".local", "bin")
	if e := os.MkdirAll(bin, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(exe, filepath.Join(bin, "mesh")); e != nil {
		t.Fatal(e)
	}
	return s
}
func taskFor(s *Store, cmd ...string) Task {
	c, _ := s.Config()
	id := newID()
	return Task{ID: id, RootID: id, OriginID: c.Self.ID, Agent: "shell", Command: cmd, MaxDepth: 2, MaxChildren: 4}
}
func waitJob(t *testing.T, s *Store, id string) Job {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		j, e := s.Status(id)
		if e != nil {
			t.Fatal(e)
		}
		if terminal(j.State) {
			return j
		}
		time.Sleep(40 * time.Millisecond)
	}
	j, _ := s.Status(id)
	log, _ := os.ReadFile(s.jobPath(id, "worker.log"))
	t.Fatalf("job did not finish: %+v\n%s", j, log)
	return Job{}
}
func peerOf(s *Store) Peer { c, _ := s.Config(); return c.Self }
func cliCmd(t *testing.T, s *Store, args ...string) *exec.Cmd {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, args...)
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome})
	return cmd
}

func fakeSSH(t *testing.T, stores ...*Store) {
	t.Helper()
	dir := t.TempDir()
	mapping := map[string]map[string]string{}
	for _, s := range stores {
		p := peerOf(s)
		mapping[p.Endpoint.Host] = map[string]string{"root": s.Root, "home": s.UserHome}
	}
	b, _ := json.Marshal(mapping)
	file := filepath.Join(dir, "peers.json")
	if e := os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	script := `#!/usr/bin/python3
import json, os, subprocess, sys
mapping = json.load(open(os.environ['MESH_TEST_PEERS']))
if '-O' in sys.argv:
    sys.exit(0)
host, command = sys.argv[-2:]
payload = None
if command.startswith('exec ') and '_rpc' in command:
    payload = sys.stdin.buffer.read()
    if os.environ.get('MESH_TEST_SSH_LOG'):
        with open(os.environ['MESH_TEST_SSH_LOG'], 'a') as log:
            log.write(json.dumps({'host': host, 'action': json.loads(payload)['action']}) + '\n')
if host not in mapping:
    sys.stderr.write('fixture endpoint is offline\n')
    sys.exit(255)
peer = mapping[host]
if os.path.exists(os.path.join(peer['root'], 'offline')):
    sys.exit(255)
if command.startswith('uname -s; uname -m;'):
    import platform
    print(platform.system())
    print(platform.machine())
    print(peer['home'])
    sys.exit(0)
if command.startswith('umask 077; mkdir -p'):
    import tempfile
    folder = os.path.join(peer['home'], '.local', 'bin')
    os.makedirs(folder, exist_ok=True)
    fd, temp = tempfile.mkstemp(dir=folder)
    with os.fdopen(fd, 'wb') as out:
        out.write(sys.stdin.buffer.read())
    os.chmod(temp, 0o700)
    os.replace(temp, os.path.join(folder, 'mesh'))
    sys.exit(0)
env = dict(os.environ)
env['MESH_HOME'] = peer['root']
env['MESH_USER_HOME'] = peer['home']
env['SSH_CONNECTION'] = '127.0.0.1 40000 127.0.0.1 22'
env.pop('MESH_JOB_ID', None)
result = subprocess.run(command, shell=True, env=env, input=payload, stdout=subprocess.PIPE)
drop = os.path.join(peer['root'], 'drop-reply')
if os.path.exists(drop):
    os.unlink(drop)
    sys.exit(255)
sys.stdout.buffer.write(result.stdout)
sys.exit(result.returncode)
`
	if e := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MESH_TEST_PEERS", file)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestShellQuoting(t *testing.T) {
	for _, value := range []string{"plain", "with spaces", "a'b", "$(touch nope); `false`\nnext"} {
		cmd := exec.Command("sh", "-c", "printf '%s' "+quote(value))
		out, e := cmd.Output()
		if e != nil || string(out) != value {
			t.Fatalf("quote %q: %s %v", value, out, e)
		}
	}
}

func TestFileTransferBoundaries(t *testing.T) {
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("content")
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", "a\\b", "a/./b", ".", "a\nfile"} {
		if e := materialize(root, []File{{Path: name, Data: data, SHA256: digest(data)}}); e == nil {
			t.Errorf("accepted unsafe path %q", name)
		}
	}
	if e := materialize(root, []File{{Path: "bad", Data: data, SHA256: "wrong"}}); e == nil {
		t.Fatal("accepted corrupt bytes")
	}
	files := []File{{Path: "nested/report.pdf", Data: data, SHA256: digest(data)}}
	if e := materialize(root, files); e != nil {
		t.Fatal(e)
	}
	if e := materialize(root, files); e != nil {
		t.Fatal("matching redelivery failed", e)
	}
	files[0].Data = []byte("new")
	files[0].SHA256 = digest(files[0].Data)
	if e := materialize(root, files); e == nil {
		t.Fatal("overwrote existing result")
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if e := materialize(root, []File{{Path: "link/escape", Data: data, SHA256: digest(data)}}); e == nil {
		t.Fatal("followed output symlink")
	}
	if _, e := gatherFiles([]string{filepath.Join(root, "link")}); e == nil {
		t.Fatal("transferred input symlink")
	}
	if e := validateFiles([]File{{"dir", data, digest(data), false}, {"dir/file", data, digest(data), false}}); e == nil {
		t.Fatal("accepted file/directory collision")
	}
}

func TestConcurrentInventoryAndStaleCopies(t *testing.T) {
	a, b := fixture(t, "a", true), fixture(t, "b", true)
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if e := b.Capability(fmt.Sprintf("tool-%d", n), "env", "verified", false); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	inv, e := b.OwnInventory()
	if e != nil {
		t.Fatal(e)
	}
	if len(inv.Capabilities) != 20 || inv.Revision != 21 {
		t.Fatalf("lost update: %+v", inv)
	}
	if e := a.CacheInventory(inv.ID, inv); e != nil {
		t.Fatal(e)
	}
	older := inv
	older.Revision = 1
	older.Capabilities = nil
	if e := a.CacheInventory(inv.ID, older); e != nil {
		t.Fatal(e)
	}
	var saved Inventory
	_ = readJSON(a.path("machines", inv.ID+".json"), &saved)
	if len(saved.Capabilities) != 20 {
		t.Fatal("stale copy overwrote latest")
	}
	own, _ := a.OwnInventory()
	if e := a.CacheInventory(own.ID, own); e == nil {
		t.Fatal("accepted peer replacement for own inventory")
	}
	if e := a.CacheInventory(peerOf(a).ID, inv); e == nil {
		t.Fatal("accepted identity mismatch")
	}
}

func TestAccessAndRevocation(t *testing.T) {
	a, b, c := fixture(t, "a", true), fixture(t, "b", true), fixture(t, "c", false)
	file := filepath.Join(a.UserHome, ".ssh", "authorized_keys")
	if e := atomicWrite(file, []byte("existing-user-key\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	content, _ := os.ReadFile(file)
	if !strings.Contains(string(content), "existing-user-key") || strings.Count(string(content), "ai-mesh-managed:") != 1 {
		t.Fatalf("bad key update: %s", content)
	}
	if e := c.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	content, _ = os.ReadFile(filepath.Join(c.UserHome, ".ssh", "authorized_keys"))
	if strings.Contains(string(content), "ai-mesh-managed:") {
		t.Fatal("incoming opt-out ignored")
	}
	if e := a.RemovePeer(peerOf(b).ID); e != nil {
		t.Fatal(e)
	}
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	config, _ := a.Config()
	if len(config.Peers) != 0 {
		t.Fatal("stale roster restored revoked device")
	}
	content, _ = os.ReadFile(file)
	if strings.Contains(string(content), "ai-mesh-managed:") || !strings.Contains(string(content), "existing-user-key") {
		t.Fatalf("bad revocation: %s", content)
	}
}

func TestInstructionsPreserveUserContent(t *testing.T) {
	original := "# My instructions\nKeep this.\n"
	first, e := managedInstructions(original, "mesh instructions", false)
	if e != nil {
		t.Fatal(e)
	}
	second, e := managedInstructions(first, "mesh instructions", false)
	if e != nil || second != first {
		t.Fatal("integration not idempotent")
	}
	removed, e := managedInstructions(second, "", true)
	if e != nil || removed != original {
		t.Fatal("did not restore user text")
	}
	if _, e := managedInstructions(original+beginInstructions, "new", false); e == nil {
		t.Fatal("silently repaired malformed block")
	}
}

func TestAccessPreservesAuthorizedKeysSymlink(t *testing.T) {
	a, b := fixture(t, "a", true), fixture(t, "b", true)
	target := filepath.Join(a.UserHome, "dotfiles", "authorized_keys")
	if e := atomicWrite(target, []byte("existing-user-key\n"), 0600); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(a.UserHome, ".ssh", "authorized_keys")
	if e := os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, file); e != nil {
		t.Fatal(e)
	}
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	info, e := os.Lstat(file)
	if e != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced user's symlink")
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "existing-user-key") || !strings.Contains(string(data), "ai-mesh-managed:") {
		t.Fatalf("lost keys: %s", data)
	}
}

func TestDiscoveryParsers(t *testing.T) {
	peers, e := parseTailscale([]byte(`{"Self":{"HostName":"self"},"Peer":{"abc":{"HostName":"home","DNSName":"home.example.ts.net.","TailscaleIPs":["100.64.0.1"]}}}`))
	if e != nil || len(peers) != 1 || peers[0].Host != "home.example.ts.net" {
		t.Fatalf("bad discovery: %+v %v", peers, e)
	}
	ssh := parseSSHConfig([]byte("Host lab other\n HostName 10.0.0.2\n User alice\n Port 2200\nHost *\n User nobody\n"))
	if len(ssh) != 2 || ssh[0].Host != "lab" || ssh[0].Port != 2200 || ssh[0].User != "alice" {
		t.Fatalf("bad SSH config discovery: %+v", ssh)
	}
}

func TestLocalTaskLifecycleAndIdempotency(t *testing.T) {
	s := fixture(t, "local", false)
	marker := filepath.Join(s.UserHome, "runs")
	task := taskFor(s, "sh", "-c", "printf x >> "+quote(marker)+`; cat inputs/input.txt > "$MESH_OUTPUT_DIR/report.txt"; printf 'done\n'`)
	b := []byte("hello")
	task.Inputs = []File{{Path: "input.txt", Data: b, SHA256: digest(b)}}
	r, e := s.Submit("local", task, filepath.Join(s.UserHome, "results"))
	if e != nil {
		t.Fatal(e)
	}
	j := waitJob(t, s, r.ID)
	if j.State != "completed" || j.Delivery != "pending" {
		t.Fatalf("bad result %+v", j)
	}
	if _, e := s.Accept(task); e != nil {
		t.Fatal(e)
	}
	runs, _ := os.ReadFile(marker)
	if string(runs) != "x" {
		t.Fatalf("ran more than once: %s", runs)
	}
	changed := task
	changed.Prompt = "changed"
	if _, e := s.Accept(changed); e == nil {
		t.Fatal("accepted different task with same ID")
	}
	if _, e := s.Collect(r.ID, ""); e != nil {
		t.Fatal(e)
	}
	out, _ := os.ReadFile(filepath.Join(r.Output, "report.txt"))
	if string(out) != "hello" {
		t.Fatalf("wrong result: %s", out)
	}
	j, _ = s.Status(r.ID)
	if j.Delivery != "delivered" {
		t.Fatal("not acknowledged")
	}
	if _, e := s.Collect(r.ID, ""); e != nil {
		t.Fatal("redelivery failed", e)
	}
}

func TestRemoteRoundTripAndReconnect(t *testing.T) {
	a, b := fixture(t, "a", true), fixture(t, "b", true)
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	if e := b.ImportPeers([]Peer{peerOf(a)}); e != nil {
		t.Fatal(e)
	}
	fakeSSH(t, a, b)
	if issues := a.Sync(); len(issues) > 0 {
		t.Fatal(issues)
	}
	task := taskFor(a, "sh", "-c", `sleep 0.2; printf 'remote artifact' > "$MESH_OUTPUT_DIR/result.txt"`)
	if e := os.WriteFile(b.path("drop-reply"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	r, e := a.Submit("b", task, filepath.Join(a.UserHome, "received"))
	if e == nil {
		t.Fatal("expected simulated lost response")
	}
	if e := a.Retry(r.ID); e != nil {
		t.Fatal(e)
	}
	j := waitJob(t, a, r.ID)
	if j.State != "completed" {
		t.Fatal(j)
	}
	if e := os.WriteFile(b.path("offline"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := a.Collect(r.ID, ""); e == nil {
		t.Fatal("collected from offline destination")
	}
	receipt, _ := a.Receipt(r.ID)
	if receipt.Delivery != "pending" {
		t.Fatal("premature delivery")
	}
	_ = os.Remove(b.path("offline"))
	if issues := a.Tick(); len(issues) > 0 {
		t.Fatal(issues)
	}
	receipt, _ = a.Receipt(r.ID)
	if receipt.Delivery != "delivered" {
		t.Fatal("daemon did not collect after reconnect")
	}
	remote, _ := b.Job(r.ID)
	if remote.Delivery != "delivered" {
		t.Fatal("destination not acknowledged")
	}
	data, _ := os.ReadFile(filepath.Join(receipt.Output, "result.txt"))
	if string(data) != "remote artifact" {
		t.Fatalf("wrong artifact %s", data)
	}
}

func TestWorkerSurvivesSubmittingCLIExit(t *testing.T) {
	s := fixture(t, "origin", false)
	cmd := cliCmd(t, s, "run", "--agent", "shell", "--", "sh", "-c", `sleep 0.3; printf survived > "$MESH_OUTPUT_DIR/alive.txt"`)
	out, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	var r Receipt
	if e := json.Unmarshal(out, &r); e != nil {
		t.Fatalf("%s %v", out, e)
	}
	j := waitJob(t, s, r.ID)
	if j.State != "completed" {
		t.Fatal(j)
	}
}

func TestTaskFailureAndCancellation(t *testing.T) {
	s := fixture(t, "local", false)
	r, e := s.Submit("local", taskFor(s, "sh", "-c", "exit 7"), "")
	if e != nil {
		t.Fatal(e)
	}
	j := waitJob(t, s, r.ID)
	if j.State != "failed" || j.ExitCode != 7 {
		t.Fatal(j)
	}
	r, e = s.Submit("local", taskFor(s, "sh", "-c", "sleep 30"), "")
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, _ = s.Status(r.ID)
		if j.State == "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e := s.CancelTask(r.ID); e != nil {
		t.Fatal(e)
	}
	j = waitJob(t, s, r.ID)
	if j.State != "cancelled" {
		t.Fatal(j)
	}
}

func TestPlacementAndDelegationLimits(t *testing.T) {
	s := fixture(t, "parent", false)
	parent := taskFor(s, "true")
	parent.MaxDepth = 0
	if e := os.MkdirAll(s.jobPath(parent.ID), 0700); e != nil {
		t.Fatal(e)
	}
	if e := writeJSON(s.jobPath(parent.ID, "job.json"), Job{Task: parent, State: "running"}); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MESH_JOB_ID", parent.ID)
	child := taskFor(s, "true")
	if _, e := s.Submit("local", child, ""); e == nil {
		t.Fatal("bypassed delegation depth")
	}
	parent.MaxDepth = 2
	parent.MaxChildren = 0
	_ = writeJSON(s.jobPath(parent.ID, "job.json"), Job{Task: parent, State: "running"})
	if _, e := s.Submit("local", child, ""); e == nil {
		t.Fatal("bypassed child count")
	}
	child.Allowed = []string{"another-machine"}
	if e := validateTask(child, peerOf(s).ID); e == nil {
		t.Fatal("bypassed placement")
	}
}

func TestAgentAdaptersDoNotBypassPermissions(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		cmd, e := agentCommand(Task{Agent: agent, Prompt: "Make a PDF"}, "/tmp/work")
		if e != nil {
			t.Fatal(e)
		}
		joined := strings.Join(cmd.Args, " ")
		if strings.Contains(joined, "dangerously") || strings.Contains(joined, "bypass") {
			t.Fatal(joined)
		}
		if cmd.Stdin == nil {
			t.Fatal("prompt missing")
		}
		if agent == "codex" && !strings.Contains(joined, "workspace-write") {
			t.Fatal(joined)
		}
		if agent == "claude" && !strings.Contains(joined, "stream-json") {
			t.Fatal(joined)
		}
	}
}

func TestServiceDefinitions(t *testing.T) {
	x, e := serviceDefinition("darwin", "/a path/mesh", "/data/a&b", "/user/home", "/some/path")
	if e != nil {
		t.Fatal(e)
	}
	decoder := xml.NewDecoder(strings.NewReader(x))
	for {
		_, e := decoder.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	x, e = serviceDefinition("linux", "/a path/mesh", "/data/100%", "/user/home", "/some/path")
	if e != nil || !strings.Contains(x, `ExecStart="/a path/mesh" daemon`) || !strings.Contains(x, "100%%") {
		t.Fatalf("%s %v", x, e)
	}
}

func TestRPCRejectsIncomingOptOut(t *testing.T) {
	s := fixture(t, "local", false)
	t.Setenv("SSH_CONNECTION", "test")
	var out bytes.Buffer
	if e := s.RPC(strings.NewReader(`{"action":"info"}`), &out); e != nil {
		t.Fatal(e)
	}
	var reply Response
	_ = json.Unmarshal(out.Bytes(), &reply)
	if !strings.Contains(reply.Error, "does not accept") {
		t.Fatal(out.String())
	}
}

func TestPendingMembershipAndRevocation(t *testing.T) {
	a, b, c := fixture(t, "a", true), fixture(t, "b", true), fixture(t, "c", true)
	for _, pair := range [][2]*Store{{a, b}, {a, c}, {b, a}, {c, a}} {
		if e := pair[0].ImportPeers([]Peer{peerOf(pair[1])}); e != nil {
			t.Fatal(e)
		}
	}
	fakeSSH(t, a, b, c)
	_ = os.WriteFile(b.path("offline"), nil, 0600)
	if issues := a.ReconcilePeers(); len(issues) == 0 {
		t.Fatal("expected queued enrollment")
	}
	_ = os.Remove(b.path("offline"))
	if issues := a.PendingChanges(); len(issues) > 0 {
		t.Fatal(issues)
	}
	config, _ := b.Config()
	if _, ok := config.Peers[peerOf(c).ID]; !ok {
		t.Fatal("offline member did not catch up")
	}
	_ = os.WriteFile(b.path("offline"), nil, 0600)
	if e := a.RemoveFromMesh("c"); e == nil {
		t.Fatal("expected queued revocation")
	}
	_ = os.Remove(b.path("offline"))
	if issues := a.PendingChanges(); len(issues) > 0 {
		t.Fatal(issues)
	}
	config, _ = b.Config()
	if _, ok := config.Peers[peerOf(c).ID]; ok {
		t.Fatal("revocation not applied")
	}
}

func TestSessionSwitchAndReturn(t *testing.T) {
	if _, e := exec.LookPath("tmux"); e != nil {
		t.Skip("tmux unavailable")
	}
	s := fixture(t, "local", false)
	t.Cleanup(func() { _ = s.tmux("kill-server").Run() })
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if e := os.WriteFile(fake, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	socketDir, e := os.MkdirTemp("/tmp", "mesh-controller-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	w := SessionWindow{ID: "wfirst", MachineID: peerOf(s).ID, Agent: "codex", Project: s.UserHome}
	g := SessionGroup{ID: newID(), Socket: filepath.Join(socketDir, "c.sock"), Token: newID() + newID(), Windows: []SessionWindow{w}}
	if e := s.saveSession(g); e != nil {
		t.Fatal(e)
	}
	if e := s.createWindow(g, w, true); e != nil {
		t.Fatal(e)
	}
	if e := s.ensureController(g); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MESH_CONTROL_SOCKET", g.Socket)
	t.Setenv("MESH_CONTROL_TOKEN", g.Token)
	t.Setenv("MESH_WINDOW", w.ID)
	project := filepath.Join(s.UserHome, "another-project")
	_ = os.MkdirAll(project, 0700)
	if e := RequestSwitch("local", "codex", project, false); e != nil {
		t.Fatal(e)
	}
	time.Sleep(500 * time.Millisecond)
	updated, e := s.session(g.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(updated.Windows) != 2 || len(updated.Back) != 1 {
		t.Fatalf("switch lost state: %+v", updated)
	}
	t.Setenv("MESH_WINDOW", updated.Windows[1].ID)
	if e := RequestSwitch("", "", "", true); e != nil {
		t.Fatal(e)
	}
	time.Sleep(500 * time.Millisecond)
	selected, e := s.tmux("display-message", "-p", "-t", "mesh-"+g.ID, "#{window_name}").Output()
	if e != nil || strings.TrimSpace(string(selected)) != w.ID {
		t.Fatalf("return failed: %s %v", selected, e)
	}
	// A broken SSH connection leaves an exited pane. Choosing the same
	// computer must revive that pane rather than showing a dead terminal.
	pidText, e := s.tmux("display-message", "-p", "-t", "mesh-"+g.ID+":"+updated.Windows[1].ID, "#{pane_pid}").Output()
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if e != nil {
		t.Fatal(e)
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		dead, _ := s.tmux("display-message", "-p", "-t", "mesh-"+g.ID+":"+updated.Windows[1].ID, "#{pane_dead}").Output()
		if strings.TrimSpace(string(dead)) == "1" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e := RequestSwitch("local", "codex", project, false); e != nil {
		t.Fatal(e)
	}
	time.Sleep(500 * time.Millisecond)
	dead, _ := s.tmux("display-message", "-p", "-t", "mesh-"+g.ID+":"+updated.Windows[1].ID, "#{pane_dead}").Output()
	if strings.TrimSpace(string(dead)) != "0" {
		t.Fatal("switch left an exited pane instead of reconnecting")
	}
	t.Setenv("MESH_CONTROL_TOKEN", "wrong")
	if e := RequestSwitch("local", "", "", false); e == nil {
		t.Fatal("unauthenticated control accepted")
	}
	// The controller notices the removed group and exits on its next idle check.
	_ = s.tmux("kill-server").Run()
}

func TestEnrollmentUsesOnlySelectedAccounts(t *testing.T) {
	a, b, c := fixture(t, "a", false), fixture(t, "b", true), fixture(t, "c", true)
	if e := a.ImportPeers([]Peer{peerOf(c)}); e != nil {
		t.Fatal(e)
	}
	if e := c.ImportPeers([]Peer{peerOf(a)}); e != nil {
		t.Fatal(e)
	}
	fakeSSH(t, a, b, c)
	p := peerOf(b)
	exe, _ := os.Executable()
	got, e := a.Enroll(EnrollOptions{Endpoint: p.Endpoint, Name: p.Name, Binary: exe, Mutual: true})
	if e != nil {
		t.Fatal(e)
	}
	if got.ID != p.ID {
		t.Fatal("identity changed")
	}
	for _, s := range []*Store{a, b, c} {
		config, e := s.Config()
		if e != nil || len(config.Peers) != 2 {
			t.Fatalf("incomplete roster: %+v %v", config, e)
		}
	}
	keys, _ := os.ReadFile(filepath.Join(a.UserHome, ".ssh", "authorized_keys"))
	if strings.Contains(string(keys), "ai-mesh-managed:") {
		t.Fatal("enrollment changed incoming opt-out")
	}
	var cached Inventory
	if e := readJSON(a.path("machines", p.ID+".json"), &cached); e != nil {
		t.Fatal(e)
	}
}

func TestExplicitReenrollmentRestoresMutualAccess(t *testing.T) {
	a, b, c := fixture(t, "a", true), fixture(t, "b", true), fixture(t, "c", true)
	for _, s := range []*Store{a, b, c} {
		if e := s.ImportPeers([]Peer{peerOf(a), peerOf(b), peerOf(c)}); e != nil {
			t.Fatal(e)
		}
	}
	fakeSSH(t, a, b, c)
	if e := a.RemoveFromMesh("b"); e != nil {
		t.Fatal(e)
	}
	config, _ := b.Config()
	if len(config.Peers) != 0 {
		t.Fatal("removed computer retained mesh keys")
	}
	p := peerOf(b)
	exe, _ := os.Executable()
	if _, e := a.Enroll(EnrollOptions{Endpoint: p.Endpoint, Name: p.Name, Binary: exe, Mutual: true}); e != nil {
		t.Fatal(e)
	}
	for _, s := range []*Store{a, b, c} {
		config, e := s.Config()
		if e != nil || len(config.Peers) != 2 {
			t.Fatalf("explicit rejoin incomplete: %+v %v", config, e)
		}
	}
}

func TestRemoteParentDelegatesAndCollectsChild(t *testing.T) {
	a, b, c := fixture(t, "a", false), fixture(t, "b", true), fixture(t, "c", true)
	if e := a.ImportPeers([]Peer{peerOf(b)}); e != nil {
		t.Fatal(e)
	}
	if e := b.ImportPeers([]Peer{peerOf(c)}); e != nil {
		t.Fatal(e)
	}
	fakeSSH(t, a, b, c)
	childScript := `printf 'computed on child' > "$MESH_OUTPUT_DIR/child.txt"`
	script := shellJoin(filepath.Join(b.UserHome, ".local/bin/mesh"), "run", "--on", "c", "--agent", "shell", "--wait", "--output", b.path("child-result"), "--", "sh", "-c", childScript) + ` && cat ` + quote(b.path("child-result", "child.txt")) + ` > "$MESH_OUTPUT_DIR/final.txt"`
	task := taskFor(a, "sh", "-c", script)
	task.MaxDepth = 1
	task.Allowed = []string{peerOf(b).ID, peerOf(c).ID}
	r, e := a.Submit("b", task, filepath.Join(a.UserHome, "results"))
	if e != nil {
		t.Fatal(e)
	}
	j := waitJob(t, a, r.ID)
	if j.State != "completed" || len(j.Children) != 1 {
		logs, _ := a.TaskLogs(r.ID, 0)
		t.Fatalf("%+v\n%s", j, logs.Text)
	}
	child, e := c.Job(j.Children[0])
	if e != nil || child.Task.RootID != r.ID || child.Task.ParentID != r.ID || child.Task.Depth != 1 || child.Task.MaxDepth != 1 || len(child.Task.Allowed) != 2 {
		t.Fatalf("lost inherited policy: %+v %v", child, e)
	}
	if _, e := a.Collect(r.ID, ""); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(filepath.Join(r.Output, "final.txt"))
	if string(data) != "computed on child" {
		t.Fatalf("missing nested result: %s", data)
	}
}

func TestConcurrentCollectorsDoNotLoseDelivery(t *testing.T) {
	s := fixture(t, "local", false)
	r, e := s.Submit("local", taskFor(s, "sh", "-c", `printf result > "$MESH_OUTPUT_DIR/result.txt"`), filepath.Join(s.UserHome, "result"))
	if e != nil {
		t.Fatal(e)
	}
	waitJob(t, s, r.ID)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Collect(r.ID, ""); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	r, e = s.Receipt(r.ID)
	if e != nil || r.Delivery != "delivered" {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestSchedulerQueuesAndRecoversInterruptedJob(t *testing.T) {
	s := fixture(t, "local", false)
	if e := s.updateConfig(func(c *Config) error { c.MaxJobs = 1; return nil }); e != nil {
		t.Fatal(e)
	}
	gate := filepath.Join(s.UserHome, "release")
	first, e := s.Submit("local", taskFor(s, "sh", "-c", "while [ ! -f "+quote(gate)+" ]; do sleep 0.05; done"), "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0600) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := s.Job(first.ID)
		if j.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first job did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second, e := s.Submit("local", taskFor(s, "true"), "")
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(150 * time.Millisecond)
	j, _ := s.Job(second.ID)
	if j.State != "queued" {
		t.Fatal("exceeded concurrency limit", j.State)
	}
	if e := os.WriteFile(gate, nil, 0600); e != nil {
		t.Fatal(e)
	}
	waitJob(t, s, first.ID)
	waitJob(t, s, second.ID)
	orphan := taskFor(s, "false")
	if e := writeJSON(s.jobPath(orphan.ID, "job.json"), Job{Task: orphan, State: "running", PID: 99999999}); e != nil {
		t.Fatal(e)
	}
	if issues := s.RecoverJobs(); len(issues) > 0 {
		t.Fatal(issues)
	}
	j, _ = s.Job(orphan.ID)
	if j.State != "failed" || !strings.Contains(j.Error, "not automatically rerun") {
		t.Fatalf("unsafe recovery: %+v", j)
	}
}

func TestIncomingChangeDoesNotGrantFullMesh(t *testing.T) {
	a, b, c := fixture(t, "a", true), fixture(t, "b", true), fixture(t, "c", true)
	for _, pair := range [][2]*Store{{a, b}, {a, c}, {b, a}, {c, a}} {
		if e := pair[0].ImportPeers([]Peer{peerOf(pair[1])}); e != nil {
			t.Fatal(e)
		}
	}
	fakeSSH(t, a, b, c)
	if e := a.SetIncoming(false); e != nil {
		t.Fatal(e)
	}
	config, e := b.Config()
	if e != nil {
		t.Fatal(e)
	}
	if len(config.Peers) != 1 || config.Peers[peerOf(a).ID].Incoming {
		t.Fatalf("access change broadened topology or was lost: %+v", config.Peers)
	}
}

func TestFakeAgentEndToEnd(t *testing.T) {
	s := fixture(t, "local", false)
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, agent := range []string{"codex", "claude"} {
		script := "#!/bin/sh\ncat > received-prompt.txt\ncp inputs/brief.txt outputs/report.txt\nprintf '{\"type\":\"fixture.complete\"}\\n'\n"
		if e := os.WriteFile(filepath.Join(dir, agent), []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
		task := taskFor(s)
		task.Agent = agent
		task.Prompt = "Use the brief to produce a report."
		data := []byte("file context")
		task.Inputs = []File{{"brief.txt", data, digest(data), false}}
		r, e := s.Submit("local", task, filepath.Join(s.UserHome, agent+"-result"))
		if e != nil {
			t.Fatal(e)
		}
		j := waitJob(t, s, r.ID)
		if j.State != "completed" {
			t.Fatal(j)
		}
		prompt, _ := os.ReadFile(s.jobPath(r.ID, "work", "received-prompt.txt"))
		if !strings.Contains(string(prompt), task.Prompt) || !strings.Contains(string(prompt), "./outputs") {
			t.Fatalf("wrong prompt %s", prompt)
		}
		if _, e := s.Collect(r.ID, ""); e != nil {
			t.Fatal(e)
		}
	}
}
