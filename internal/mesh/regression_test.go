package mesh

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerJobsUseServiceAndLocalJobsPreserveCaller(t *testing.T) {
	s := fixture(t, "local", true)
	short, e := os.MkdirTemp("/tmp", "mesh-dispatch-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	newRoot := filepath.Join(short, "state")
	if e = os.Rename(s.Root, newRoot); e != nil {
		t.Fatal(e)
	}
	s.Root = newRoot
	t.Setenv("MESH_DISPATCH_TEST", "service")
	listener, e := s.jobListener()
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	info, e := os.Stat(s.path("jobs.sock"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", info, e)
	}
	task := taskFor(s, "sh", "-c", `printf '%s' "$MESH_DISPATCH_TEST" > outputs/owner.txt`)
	request, e := json.Marshal(Request{Action: "submit", Task: &task})
	if e != nil {
		t.Fatal(e)
	}
	cmd := cliCmd(t, s, "_rpc")
	cmd.Stdin = strings.NewReader(string(request))
	cmd.Env = append(cmd.Env, "MESH_DISPATCH_TEST=submitting-process")
	output, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("submit: %s %v", output, e)
	}
	var response Response
	if e = json.Unmarshal(output, &response); e != nil {
		t.Fatal(e)
	}
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	j := waitJob(t, s, task.ID)
	if j.State != "completed" {
		t.Fatal(j)
	}
	got, e := os.ReadFile(s.jobPath(task.ID, "work", "outputs", "owner.txt"))
	if e != nil || string(got) != "service" {
		t.Fatalf("worker inherited submitting SSH environment: %s %v", got, e)
	}
	cmd = cliCmd(t, s, "run", "--on", "local", "--agent", "shell", "--output", filepath.Join(s.UserHome, "result"), "--", "sh", "-c", `printf '%s' "$MESH_DISPATCH_TEST" > outputs/owner.txt`)
	cmd.Env = append(cmd.Env, "MESH_DISPATCH_TEST=submitting-process")
	output, e = cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("local submit: %s %v", output, e)
	}
	var receipt Receipt
	if e = json.Unmarshal(output, &receipt); e != nil {
		t.Fatal(e)
	}
	if j = waitJob(t, s, receipt.ID); j.State != "completed" {
		t.Fatal(j)
	}
	got, e = os.ReadFile(s.jobPath(receipt.ID, "work", "outputs", "owner.txt"))
	if e != nil || string(got) != "submitting-process" {
		t.Fatalf("local job lost caller environment: %s %v", got, e)
	}
	_ = listener.Close()
	if s.wakeJob(receipt.ID) {
		t.Fatal("closed daemon accepted notification")
	}
}

func TestSSHFindsUserAgentAndPreservesExplicitPath(t *testing.T) {
	home := t.TempDir()
	nvm := filepath.Join(home, ".nvm", "versions", "node", "v24.0.0", "bin")
	if e := os.MkdirAll(nvm, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(nvm, "codex"), []byte("#!/bin/sh\necho user-agent\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	extendToolPath(home)
	got, e := exec.LookPath("codex")
	if e != nil || got != filepath.Join(nvm, "codex") {
		t.Fatalf("minimal SSH environment chose %s: %v", got, e)
	}
	explicit := t.TempDir()
	if e := os.WriteFile(filepath.Join(explicit, "codex"), []byte("#!/bin/sh\necho explicit-agent\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", explicit+":/usr/bin:/bin")
	extendToolPath(home)
	got, e = exec.LookPath("codex")
	if e != nil || got != filepath.Join(explicit, "codex") {
		t.Fatalf("overrode user PATH: %s %v", got, e)
	}
}

func TestProviderFailuresFromRealEventShapes(t *testing.T) {
	cases := []struct{ name, events, code string }{
		{"expired Claude OAuth", `{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}`, "authentication_required"},
		{"Claude permission denial", `{"type":"result","is_error":false,"result":"done","permission_denials":[{"tool_name":"Bash"}]}`, "permission_required"},
		{"Codex usage limit", `{"type":"turn.failed","error":{"message":"You have hit your usage limit"}}`, "usage_limit"},
		{"Codex failure", `{"type":"turn.failed","error":{"message":"Connection failed"}}`, "agent_failed"},
		{"recovered transient error", "{\"type\":\"error\",\"message\":\"reconnecting\"}\n{\"type\":\"turn.completed\"}", ""},
		{"ordinary discussion of errors", `{"type":"assistant","message":{"content":[{"text":"OAuth and permission errors are described in this report"}]}}`, ""},
		{"normal Claude completion", `{"type":"result","is_error":false,"result":"Authentication report generated"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.log")
			if e := os.WriteFile(path, []byte(tc.events+"\n"), 0600); e != nil {
				t.Fatal(e)
			}
			message, code := providerFailure(path)
			if code != tc.code || (message == "") != (code == "") {
				t.Fatalf("got %q %q, want %q", message, code, tc.code)
			}
		})
	}
}

func TestRequiredOutputsAndAgentAttention(t *testing.T) {
	s := fixture(t, "local", false)
	task := taskFor(s, "sh", "-c", "true")
	task.RequiredOutputs = []string{"report.pdf"}
	r, e := s.Submit("local", task, filepath.Join(s.UserHome, "missing-result"))
	if e != nil {
		t.Fatal(e)
	}
	j := waitJob(t, s, r.ID)
	if j.State != "failed" || !strings.Contains(j.Error, "report.pdf") {
		t.Fatalf("missing deliverable accepted: %+v", j)
	}
	task = taskFor(s, "sh", "-c", `printf test > outputs/report.pdf`)
	task.RequiredOutputs = []string{"report.pdf"}
	r, e = s.Submit("local", task, filepath.Join(s.UserHome, "present-result"))
	if e != nil {
		t.Fatal(e)
	}
	if j = waitJob(t, s, r.ID); j.State != "completed" {
		t.Fatal(j)
	}
	task = taskFor(s, "true")
	task.RequiredOutputs = []string{"../escape"}
	if _, e = s.Submit("local", task, ""); e == nil {
		t.Fatal("unsafe output path accepted")
	}
	dir := t.TempDir()
	if e = os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"result\":\"OAuth session expired\"}'\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	task = taskFor(s)
	task.Agent = "claude"
	task.Prompt = "test"
	r, e = s.Submit("local", task, filepath.Join(s.UserHome, "auth-result"))
	if e != nil {
		t.Fatal(e)
	}
	j = waitJob(t, s, r.ID)
	if j.State != "needs_attention" || j.ErrorCode != "authentication_required" || !terminal(j.State) {
		t.Fatalf("provider error hidden: %+v", j)
	}
}

func TestDiscoveryExcludesTailscaleIngress(t *testing.T) {
	peers, e := parseTailscale([]byte(`{"Peer":{"a":{"HostName":"green-lighthouse","TailscaleIPs":["100.64.0.1"]},"b":{"HostName":"funnel-ingress-node","Tags":["tag:ingress"],"TailscaleIPs":["100.64.0.2"]}}}`))
	if e != nil || len(peers) != 1 || peers[0].Name != "green-lighthouse" {
		t.Fatalf("bad discovered computers: %+v %v", peers, e)
	}
}

func TestUserSSHServerConfiguration(t *testing.T) {
	config, e := sshServerConfig("100.102.191.68", 2222, "alice", "/Users/alice/Mesh Folder/ssh-server")
	if e != nil {
		t.Fatal(e)
	}
	for _, required := range []string{"ListenAddress 100.102.191.68", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "PermitRootLogin no", "AllowUsers alice", `AuthorizedKeysFile "/Users/alice/Mesh Folder/ssh-server/authorized_keys"`} {
		if !strings.Contains(config, required) {
			t.Fatalf("missing %s", required)
		}
	}
	for _, address := range []string{"0.0.0.0", "::", "8.8.8.8", "example.com", "127.0.0.1\nPasswordAuthentication yes"} {
		if _, e := sshServerConfig(address, 2222, "alice", "/tmp/mesh"); e == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	if _, e := sshServerConfig("127.0.0.1", 22, "alice", "/tmp/mesh"); e == nil {
		t.Fatal("privileged port accepted")
	}
	s := fixture(t, "local", true)
	other := fixture(t, "other", true)
	if e = s.ImportPeers([]Peer{peerOf(other)}); e != nil {
		t.Fatal(e)
	}
	keys, e := os.ReadFile(s.path("ssh-server", "authorized_keys"))
	if e != nil || !strings.Contains(string(keys), peerOf(other).ID) {
		t.Fatalf("missing managed key: %s %v", keys, e)
	}
	_ = s.SetIncoming(false) // This fixture peer is deliberately not reachable.
	keys, _ = os.ReadFile(s.path("ssh-server", "authorized_keys"))
	if strings.TrimSpace(string(keys)) != "" {
		t.Fatal("listener still authorizes keys after incoming disabled")
	}
}
