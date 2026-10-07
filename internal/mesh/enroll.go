package mesh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func decodePeer(b []byte, p *Peer) error { return json.Unmarshal(b, p) }
func checkResponse(b []byte) error {
	var r Response
	if e := json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Protocol != Protocol {
		return errors.New("incompatible peer protocol")
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

func (s *Store) Init(name, address, username string, port, maxJobs int, incoming bool) (Peer, error) {
	if e := s.ensure(); e != nil {
		return Peer{}, e
	}
	var result Peer
	e := withLock(s.path("config.lock"), func() error {
		if _, err := os.Stat(s.path("config.json")); err == nil {
			c, e := s.Config()
			if e != nil {
				return e
			}
			result = c.Self
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if name == "" {
			name, _ = os.Hostname()
			name = strings.Split(name, ".")[0]
		}
		if !validID.MatchString(name) {
			return errors.New("machine name must contain only letters, numbers, underscores, or hyphens (max 80)")
		}
		if address == "" {
			address, _ = os.Hostname()
		}
		if username == "" {
			u, e := user.Current()
			if e != nil {
				return e
			}
			username = u.Username
		}
		if maxJobs < 1 || maxJobs > 32 {
			return errors.New("max-jobs must be between 1 and 32")
		}
		endpoint := Endpoint{address, username, port}
		if e := validateEndpoint(endpoint); e != nil {
			return e
		}
		id := newID()
		key := s.path("keys", "id_ed25519")
		if _, e := os.Stat(key); os.IsNotExist(e) {
			cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "ai-mesh:"+id, "-f", key)
			if out, e := cmd.CombinedOutput(); e != nil {
				return fmt.Errorf("generate Mesh SSH key: %w: %s", e, out)
			}
		}
		pub, e := os.ReadFile(key + ".pub")
		if e != nil {
			return e
		}
		result = Peer{ID: id, Name: name, Endpoint: endpoint, Home: s.UserHome, Incoming: incoming, PublicKey: strings.TrimSpace(string(pub)), HostKeys: hostKeys()}
		return writeJSON(s.path("config.json"), Config{Protocol, result, map[string]Peer{}, maxJobs})
	})
	if e != nil {
		return Peer{}, e
	}
	if _, e := s.OwnInventory(); e != nil {
		if _, e = s.Refresh(); e != nil {
			return result, e
		}
	}
	return result, s.SSHConfig()
}

func hostKeys() []string {
	var keys []string
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		if b, e := os.ReadFile("/etc/ssh/ssh_host_" + kind + "_key.pub"); e == nil {
			fields := strings.Fields(string(b))
			if len(fields) >= 2 {
				keys = append(keys, strings.Join(fields[:2], " "))
			}
		}
	}
	return keys
}
func publicKey(key string) (string, error) {
	f := strings.Fields(key)
	if len(f) < 2 || f[0] != "ssh-ed25519" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("invalid Mesh public key")
	}
	b, e := base64.StdEncoding.DecodeString(f[1])
	if e != nil || len(b) != 51 || binary.BigEndian.Uint32(b[:4]) != 11 || string(b[4:15]) != "ssh-ed25519" || binary.BigEndian.Uint32(b[15:19]) != 32 {
		return "", errors.New("invalid Ed25519 key")
	}
	return strings.Join(f[:2], " "), nil
}

func (s *Store) ImportPeers(peers []Peer) error {
	return withLock(s.path("config.lock"), func() error {
		c, e := s.Config()
		if e != nil {
			return e
		}
		blocked := map[string]bool{}
		if err := readJSON(s.path("revoked.json"), &blocked); err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, p := range peers {
			if blocked[p.ID] {
				continue
			}
			if p.ID == c.Self.ID {
				continue
			}
			if !validID.MatchString(p.ID) || !validID.MatchString(p.Name) {
				return errors.New("invalid peer identity")
			}
			if e := validateEndpoint(p.Endpoint); e != nil {
				return e
			}
			if !filepath.IsAbs(p.Home) || strings.ContainsAny(p.Home, "\r\n\x00") {
				return errors.New("invalid peer home")
			}
			if _, e := publicKey(p.PublicKey); e != nil {
				return e
			}
			if old, ok := c.Peers[p.ID]; ok && old.PublicKey != p.PublicKey {
				return fmt.Errorf("peer key changed for %s; remove and enroll it again", p.Name)
			}
			if p.Name == c.Self.Name {
				return errors.New("peer name conflicts with this machine")
			}
			for id, old := range c.Peers {
				if id != p.ID && old.Name == p.Name {
					return fmt.Errorf("duplicate machine name %s", p.Name)
				}
			}
			c.Peers[p.ID] = p
		}
		if e := s.writeAccess(c); e != nil {
			return e
		}
		return writeJSON(s.path("config.json"), c)
	})
}

func (s *Store) writeAccess(c Config) error {
	sshDir := filepath.Join(s.UserHome, ".ssh")
	if e := os.MkdirAll(sshDir, 0700); e != nil {
		return e
	}
	return withLock(s.path("ssh-access.lock"), func() error {
		file := filepath.Join(sshDir, "authorized_keys")
		if info, err := os.Lstat(file); err == nil && info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(file)
			if err != nil {
				return err
			}
			file = resolved
		}
		b, e := os.ReadFile(file)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.Contains(line, " ai-mesh-managed:") {
				continue
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		var known, meshKeys []string
		for _, p := range c.Peers {
			key, e := publicKey(p.PublicKey)
			if e != nil {
				return e
			}
			if c.Self.Incoming {
				lines = append(lines, `no-agent-forwarding,no-X11-forwarding `+key+" ai-mesh-managed:"+p.ID)
				meshKeys = append(meshKeys, `no-agent-forwarding,no-X11-forwarding `+key+" ai-mesh-managed:"+p.ID)
			}
			host := p.Endpoint.Host
			if p.Endpoint.Port != 22 {
				host = "[" + host + "]:" + strconv.Itoa(p.Endpoint.Port)
			}
			for _, key := range p.HostKeys {
				f := strings.Fields(key)
				if len(f) != 2 || strings.ContainsAny(key, "\r\n") {
					return errors.New("invalid host key")
				}
				if _, e := base64.StdEncoding.DecodeString(f[1]); e != nil {
					return e
				}
				known = append(known, host+" "+key)
			}
		}
		if len(lines) > 0 || len(b) > 0 {
			if e := atomicWrite(file, []byte(strings.Join(lines, "\n")+"\n"), 0600); e != nil {
				return e
			}
		}
		if e := atomicWrite(s.path("ssh-server", "authorized_keys"), []byte(strings.Join(meshKeys, "\n")+"\n"), 0600); e != nil {
			return e
		}
		if e := atomicWrite(s.path("known_hosts"), []byte(strings.Join(known, "\n")+"\n"), 0600); e != nil {
			return e
		}
		return s.writeSSHConfig(c)
	})
}

func (s *Store) RemovePeer(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid peer ID")
	}
	err := withLock(s.path("config.lock"), func() error {
		c, e := s.Config()
		if e != nil {
			return e
		}
		delete(c.Peers, id)
		blocked := map[string]bool{}
		if err := readJSON(s.path("revoked.json"), &blocked); err != nil && !os.IsNotExist(err) {
			return err
		}
		blocked[id] = true
		if e := writeJSON(s.path("revoked.json"), blocked); e != nil {
			return e
		}
		if e := s.writeAccess(c); e != nil {
			return e
		}
		return writeJSON(s.path("config.json"), c)
	})
	if err != nil {
		return err
	}
	return s.forgetInventoryDelivery(id)
}

type EnrollOptions struct {
	Endpoint  Endpoint
	Name      string
	Binary    string
	Mutual    bool
	Integrate bool
	Service   bool
}

func (s *Store) Enroll(o EnrollOptions) (Peer, error) {
	c, e := s.Config()
	if e != nil {
		return Peer{}, e
	}
	if e := validateEndpoint(o.Endpoint); e != nil {
		return Peer{}, e
	}
	if !validID.MatchString(o.Name) {
		return Peer{}, errors.New("provide a simple machine name with --name")
	}
	controlDir, e := os.MkdirTemp("", "mesh-ssh-")
	if e != nil {
		return Peer{}, e
	}
	defer os.RemoveAll(controlDir)
	base := []string{"-p", strconv.Itoa(o.Endpoint.Port), "-l", o.Endpoint.User, "-o", "ConnectTimeout=10", "-o", "ForwardAgent=no", "-o", "StrictHostKeyChecking=ask", "-o", "ControlMaster=auto", "-o", "ControlPersist=120", "-o", "ControlPath=" + filepath.Join(controlDir, "s")}
	base = append(base, "-i", s.path("keys", "id_ed25519"), "-o", "UserKnownHostsFile="+quote(s.path("known_hosts"))+" "+quote(filepath.Join(s.UserHome, ".ssh", "known_hosts")))
	defer func() { _ = exec.Command("ssh", append(base, "-O", "exit", o.Endpoint.Host)...).Run() }()
	run := func(command string, input []byte) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ssh", append(base, o.Endpoint.Host, command)...)
		cmd.Stderr = os.Stderr
		if input != nil {
			cmd.Stdin = bytes.NewReader(input)
		}
		return cmd.Output()
	}
	out, e := run(`uname -s; uname -m; printf '%s\n' "$HOME"`, nil)
	if e != nil {
		return Peer{}, fmt.Errorf("SSH authentication failed: %w", e)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(parts) != 3 {
		return Peer{}, errors.New("unexpected SSH discovery reply; remote shell startup must not print text")
	}
	remoteOS := strings.ToLower(parts[0])
	arch := parts[1]
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	if (remoteOS != "linux" && remoteOS != "darwin") || (arch != "amd64" && arch != "arm64") {
		return Peer{}, fmt.Errorf("unsupported destination %s/%s", remoteOS, arch)
	}
	if !filepath.IsAbs(parts[2]) || strings.ContainsAny(parts[2], "\r\x00") {
		return Peer{}, errors.New("invalid remote home")
	}
	bin := o.Binary
	if bin == "" {
		exe, err := executable()
		if err != nil {
			return Peer{}, err
		}
		if remoteOS == runtime.GOOS && arch == runtime.GOARCH {
			bin = exe
		} else {
			for _, candidate := range []string{filepath.Join(filepath.Dir(exe), "mesh-"+remoteOS+"-"+arch), filepath.Join(filepath.Dir(exe), "dist", "mesh-"+remoteOS+"-"+arch), filepath.Join("dist", "mesh-"+remoteOS+"-"+arch)} {
				if _, e := os.Stat(candidate); e == nil {
					bin = candidate
					break
				}
			}
		}
	}
	if bin == "" {
		return Peer{}, fmt.Errorf("need mesh-%s-%s; run scripts/build.sh first or pass --binary", remoteOS, arch)
	}
	payload, e := os.ReadFile(bin)
	if e != nil {
		return Peer{}, e
	}
	tmpName := "mesh-upload-" + newID()
	install := `umask 077; mkdir -p "$HOME/.local/bin" && cat > "$HOME/.local/bin/` + tmpName + `" && chmod 700 "$HOME/.local/bin/` + tmpName + `" && mv "$HOME/.local/bin/` + tmpName + `" "$HOME/.local/bin/mesh"`
	if _, e = run(install, payload); e != nil {
		return Peer{}, fmt.Errorf("upload executable: %w", e)
	}
	p := Peer{Endpoint: o.Endpoint, Home: parts[2]}
	args := []string{"init", "--name", o.Name, "--address", o.Endpoint.Host, "--user", o.Endpoint.User, "--port", strconv.Itoa(o.Endpoint.Port), "--incoming=true"}
	if _, e = run(remoteMesh(p, args...), nil); e != nil {
		return Peer{}, fmt.Errorf("remote initialization: %w", e)
	}
	out, e = run(remoteMesh(p, "_identity"), nil)
	if e != nil {
		return Peer{}, e
	}
	if e := decodePeer(out, &p); e != nil {
		return Peer{}, e
	}
	p.Endpoint = o.Endpoint
	if p.ID == c.Self.ID {
		return Peer{}, errors.New("destination is this Mesh installation")
	}
	// Bootstrap over the already authenticated connection, before switching to Mesh's key.
	roster := []Peer{c.Self}
	if o.Mutual {
		for _, existing := range c.Peers {
			roster = append(roster, existing)
		}
	}
	request := Request{Action: "roster", Peers: roster}
	for _, member := range roster {
		request.RejoinIDs = append(request.RejoinIDs, member.ID)
	}
	body, _ := marshal(request)
	response, e := run(remoteMesh(p, "_rpc"), body)
	if e != nil {
		return Peer{}, e
	}
	if e := checkResponse(response); e != nil {
		return Peer{}, e
	}
	if e := s.unrevoke(p.ID); e != nil {
		return p, e
	}
	if e := s.ImportPeers([]Peer{p}); e != nil {
		return Peer{}, e
	}
	var issues []string
	if o.Integrate {
		if _, e = run(remoteMesh(p, "integrate"), nil); e != nil {
			issues = append(issues, "remote agent integration: "+e.Error())
		}
	}
	if o.Service {
		if _, e = run(remoteMesh(p, "service", "install"), nil); e != nil {
			issues = append(issues, "remote service installation: "+e.Error())
		}
	}
	var info Info
	if e := s.Call(p, Request{Action: "info"}, &info); e != nil {
		return p, fmt.Errorf("enrolled, but key-based connection failed: %w", e)
	}
	if e := s.CacheInventory(p.ID, info.Inventory); e != nil {
		return p, e
	}
	if o.Mutual {
		issues = append(issues, s.reconcilePeers(p.ID)...)
	}
	// Bootstrap the destination's copy immediately, even without a service.
	issues = append(issues, s.Sync()...)
	if len(issues) > 0 {
		return p, fmt.Errorf("enrolled; some configuration needs attention: %s", strings.Join(issues, "; "))
	}
	return p, nil
}

func (s *Store) ReconcilePeers() []string {
	return s.reconcilePeers("")
}

func (s *Store) unrevoke(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid peer ID")
	}
	return withLock(s.path("config.lock"), func() error {
		blocked := map[string]bool{}
		if e := readJSON(s.path("revoked.json"), &blocked); e != nil && !os.IsNotExist(e) {
			return e
		}
		delete(blocked, id)
		return writeJSON(s.path("revoked.json"), blocked)
	})
}

func (s *Store) reconcilePeers(rejoinID string) []string {
	c, e := s.Config()
	if e != nil {
		return []string{e.Error()}
	}
	var issues []string
	for _, p := range c.Peers {
		if !p.Incoming {
			continue
		}
		if e := s.queueChange(p, "roster", rejoinID); e != nil {
			issues = append(issues, p.Name+": "+e.Error())
		}
	}
	return append(issues, s.PendingChanges()...)
}

// Changing this account's access publishes only its own metadata. It does not
// turn previously separate pairs of computers into a mutually authorized mesh.
func (s *Store) SetIncoming(value bool) error {
	if e := s.updateConfig(func(c *Config) error {
		c.Self.Incoming = value
		c.Self.HostKeys = s.localHostKeys()
		return s.writeAccess(*c)
	}); e != nil {
		return e
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	for _, p := range c.Peers {
		if p.Incoming {
			if e := s.queueChange(p, "peer", ""); e != nil {
				return e
			}
		}
	}
	if issues := s.PendingChanges(); len(issues) > 0 {
		return fmt.Errorf("local access updated; peer notifications queued: %s", strings.Join(issues, "; "))
	}
	return nil
}
