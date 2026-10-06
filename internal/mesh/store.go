package mesh

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const Version = "0.1.0"
const Protocol = 1

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

type Endpoint struct {
	Host string `json:"host"`
	User string `json:"user"`
	Port int    `json:"port"`
}

type Peer struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Endpoint  Endpoint `json:"endpoint"`
	Home      string   `json:"home"`
	Incoming  bool     `json:"incoming"`
	PublicKey string   `json:"public_key"`
	HostKeys  []string `json:"host_keys,omitempty"`
}

type Config struct {
	Protocol int             `json:"protocol"`
	Self     Peer            `json:"self"`
	Peers    map[string]Peer `json:"peers"`
	MaxJobs  int             `json:"max_jobs"`
}

type Store struct {
	Root     string
	UserHome string
}

func OpenStore() (*Store, error) {
	home := os.Getenv("MESH_USER_HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	root := os.Getenv("MESH_HOME")
	if root == "" {
		root = filepath.Join(home, ".ai-mesh")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{Root: root, UserHome: home}, nil
}

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func (s *Store) ensure() error {
	for _, dir := range []string{"", "keys", "machines", "jobs", "receipts", "sessions", "logs", "projects"} {
		if err := os.MkdirAll(s.path(dir), 0700); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func readJSON(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mesh-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600)
}

func withLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *Store) Config() (Config, error) {
	var c Config
	if err := readJSON(s.path("config.json"), &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, errors.New("not initialized; run mesh setup or mesh init")
		}
		return c, err
	}
	if c.Protocol != Protocol {
		return c, fmt.Errorf("unsupported configuration protocol %d", c.Protocol)
	}
	if c.Peers == nil {
		c.Peers = map[string]Peer{}
	}
	return c, nil
}

func (s *Store) updateConfig(fn func(*Config) error) error {
	return withLock(s.path("config.lock"), func() error {
		c, err := s.Config()
		if err != nil {
			return err
		}
		if err := fn(&c); err != nil {
			return err
		}
		return writeJSON(s.path("config.json"), c)
	})
}

func (c Config) Resolve(name string) (Peer, bool, error) {
	name = strings.TrimPrefix(name, "@")
	if name == "" || name == "local" || name == c.Self.ID || name == c.Self.Name {
		return c.Self, true, nil
	}
	var found []Peer
	for _, p := range c.Peers {
		if p.ID == name || p.Name == name {
			found = append(found, p)
		}
	}
	if len(found) == 1 {
		return found[0], false, nil
	}
	if len(found) > 1 {
		return Peer{}, false, fmt.Errorf("ambiguous machine %q; use its ID", name)
	}
	return Peer{}, false, fmt.Errorf("unknown machine %q; use mesh machines or mesh enroll", name)
}

func validateEndpoint(e Endpoint) error {
	if e.Host == "" || strings.HasPrefix(e.Host, "-") || strings.ContainsAny(e.Host, " \t\r\n/@\\'\";`$") {
		return errors.New("invalid SSH host; provide a hostname, address, or SSH alias")
	}
	if e.User == "" || strings.HasPrefix(e.User, "-") || strings.ContainsAny(e.User, " \t\r\n/@\\'\";`$") {
		return errors.New("invalid SSH username")
	}
	if e.Port < 1 || e.Port > 65535 {
		return errors.New("SSH port must be between 1 and 65535")
	}
	return nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func shellJoin(args ...string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quote(a)
	}
	return strings.Join(out, " ")
}

func executable() (string, error) {
	p, e := os.Executable()
	if e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(p)
}

func childEnv(extra map[string]string) []string {
	out := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := extra[k]; ok {
			continue
		}
		if k == "TMUX" || k == "TMUX_PANE" || k == "MESH_CONTROL_SOCKET" || k == "MESH_CONTROL_TOKEN" || k == "MESH_WINDOW" || k == "MESH_SESSION_ID" || k == "MESH_BINDING" || k == "MESH_ACTIVE" || k == "MESH_MACHINE" || k == "MESH_AGENT" {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}
