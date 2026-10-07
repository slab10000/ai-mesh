package mesh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ask OpenSSH itself to evaluate precedence, quoting, and Host scopes. -G
// prints the effective configuration without connecting to a server.
func sshSettings(t *testing.T, s *Store, host string) map[string]string {
	t.Helper()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is not installed")
	}
	cmd := exec.Command(ssh, "-G", "-F", filepath.Join(s.UserHome, ".ssh", "config"), host)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("OpenSSH could not read generated configuration: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, _ := strings.Cut(line, " ")
		values[key] = value
	}
	return values
}

func TestSSHConfigPeerLifecycle(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	file := filepath.Join(s.UserHome, ".ssh", "config")
	original := "# User settings\nServerAliveInterval 42\nHost *\n  User fallback\n  Port 2200\nHost unrelated\n  HostName other.test\n"
	if err := atomicWrite(file, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	p := peerOf(remote)
	p.Endpoint = Endpoint{"100.102.191.68", "macuser", 2222}
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	settings := sshSettings(t, s, "macbook")
	for key, want := range map[string]string{
		"hostname": "100.102.191.68", "user": "macuser", "port": "2222",
		"identityfile": s.path("keys", "id_ed25519"), "identitiesonly": "yes",
		"stricthostkeychecking": "true", "forwardagent": "no", "serveraliveinterval": "15",
	} {
		if settings[key] != want {
			t.Errorf("%s = %q, want %q", key, settings[key], want)
		}
	}
	if !strings.Contains(settings["userknownhostsfile"], s.path("known_hosts")) {
		t.Fatal("Mesh host keys not used", settings["userknownhostsfile"])
	}
	unrelated := sshSettings(t, s, "unrelated")
	if unrelated["hostname"] != "other.test" || unrelated["user"] != "fallback" || unrelated["port"] != "2200" || unrelated["serveraliveinterval"] != "42" {
		t.Fatal("user configuration changed for an unrelated host", unrelated)
	}
	first, _ := os.ReadFile(file)
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(file)
	if string(first) != string(second) || !strings.HasSuffix(string(second), original) {
		t.Fatal("alias update was not idempotent or changed user content")
	}
	backup, _ := os.ReadFile(file + ".pre-mesh")
	if string(backup) != original {
		t.Fatal("original SSH configuration was not backed up")
	}
	p.Name, p.Endpoint = "mac-renamed", Endpoint{"100.64.0.8", "newuser", 2223}
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	settings = sshSettings(t, s, p.Name)
	if settings["hostname"] != p.Endpoint.Host || settings["port"] != "2223" || settings["user"] != "newuser" {
		t.Fatal("updated endpoint not reflected in SSH", settings)
	}
	if sshSettings(t, s, "macbook")["port"] != "2200" {
		t.Fatal("old alias survived a rename")
	}
	p.Incoming = false
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(file)
	if string(content) != original {
		t.Fatal("outgoing-only peer kept an SSH alias")
	}
	p.Incoming = true
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePeer(p.ID); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(file)
	if string(content) != original {
		t.Fatal("removing the last peer did not restore original SSH configuration")
	}
}

func TestSSHConfigRepairAndExistingInit(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	if err := s.ImportPeers([]Peer{peerOf(remote)}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.UserHome, ".ssh", "config")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if out, err := cliCmd(t, s, "ssh-config").CombinedOutput(); err != nil {
		t.Fatalf("repair command: %v: %s", err, out)
	}
	if sshSettings(t, s, "macbook")["hostname"] != "macbook.test" {
		t.Fatal("repair did not restore the alias")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init("", "", "", 22, 2, false); err != nil {
		t.Fatal(err)
	}
	if sshSettings(t, s, "macbook")["hostname"] != "macbook.test" {
		t.Fatal("initializing an existing account did not migrate its aliases")
	}
}

func TestSSHConfigKeepsExistingEndpointAlias(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	file := filepath.Join(s.UserHome, ".ssh", "config")
	if err := atomicWrite(file, []byte("Host macbook\n  HostName 100.64.0.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := peerOf(remote)
	p.Endpoint.Host = p.Name
	if err := s.ImportPeers([]Peer{p}); err != nil {
		t.Fatal(err)
	}
	if got := sshSettings(t, s, p.Name)["hostname"]; got != "100.64.0.9" {
		t.Fatalf("enrolling an SSH alias broke its HostName mapping: %s", got)
	}
}

func TestSSHConfigPreservesSymlinkAndIncludes(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	include := filepath.Join(s.UserHome, "defaults")
	if err := atomicWrite(include, []byte("ServerAliveInterval 47\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := "Include " + sshConfigQuote(include) + "\nHost unrelated\n  User alice\n"
	target := filepath.Join(s.UserHome, "dotfiles", "ssh_config")
	if err := atomicWrite(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.UserHome, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportPeers([]Peer{peerOf(remote)}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(file)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("SSH configuration symlink was replaced")
	}
	settings := sshSettings(t, s, "unrelated")
	if settings["serveraliveinterval"] != "47" || settings["user"] != "alice" {
		t.Fatal("original Include lost its scope", settings)
	}
	backup, _ := os.ReadFile(target + ".pre-mesh")
	if string(backup) != original {
		t.Fatal("symlink target not backed up")
	}
}

func TestSSHConfigMalformedBlockIsUntouched(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	if err := s.ImportPeers([]Peer{peerOf(remote)}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(s.UserHome, ".ssh", "config")
	for _, malformed := range []string{
		beginSSHConfig + "\nHost personal\n", endSSHConfig + "\n",
		beginSSHConfig + "\n" + beginSSHConfig + "\n" + endSSHConfig + "\n",
		beginSSHConfig + "\n" + endSSHConfig + "\n" + endSSHConfig + "\n",
	} {
		if err := atomicWrite(file, []byte(malformed), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.SSHConfig(); err == nil {
			t.Fatal("silently accepted malformed managed block")
		}
		content, _ := os.ReadFile(file)
		if string(content) != malformed {
			t.Fatal("malformed configuration was overwritten")
		}
	}
}

func TestSSHConfigQuotesPathsAndRejectsInjection(t *testing.T) {
	s, remote := fixture(t, "linux", false), fixture(t, "macbook", true)
	c, _ := s.Config()
	p := peerOf(remote)
	c.Peers[p.ID] = p
	s.Root = filepath.Join(s.Root, `spaces and "quotes" %h`)
	body, err := s.sshClientConfig(c, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(s.UserHome, ".ssh", "config"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if got := sshSettings(t, s, p.Name)["identityfile"]; got != strings.ReplaceAll(s.path("keys", "id_ed25519"), "%", "%%") {
		t.Fatalf("path was not quoted literally: %q", got)
	}
	p.Endpoint.Host = "host\nProxyCommand malicious"
	c.Peers[p.ID] = p
	if _, err := s.sshClientConfig(c, ""); err == nil {
		t.Fatal("accepted SSH directive injection")
	}
}
