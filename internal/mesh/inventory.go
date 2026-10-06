package mesh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Capability struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Note        string `json:"note,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}
type Inventory struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Revision     uint64                `json:"revision"`
	UpdatedAt    string                `json:"updated_at"`
	OS           string                `json:"os"`
	Arch         string                `json:"arch"`
	CPU          string                `json:"cpu,omitempty"`
	Cores        int                   `json:"cores"`
	MemoryBytes  uint64                `json:"memory_bytes,omitempty"`
	GPU          string                `json:"gpu,omitempty"`
	Tools        map[string]string     `json:"tools"`
	Capabilities map[string]Capability `json:"capabilities"`
}

func probe(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}

// SSH and user services often start without the interactive shell's PATH.
// Preserve its ordering and add only conventional, existing tool directories.
func extendToolPath(home string) {
	paths := filepath.SplitList(os.Getenv("PATH"))
	candidates := []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, ".local", "share", "mise", "shims")}
	nodes, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin"))
	sort.Slice(nodes, func(i, j int) bool {
		a, ae := os.Stat(nodes[i])
		b, be := os.Stat(nodes[j])
		return ae == nil && be == nil && a.ModTime().After(b.ModTime())
	})
	candidates = append(candidates, nodes...)
	// Prefer account-managed runtimes before shared fallback installations.
	// A minimal SSH PATH otherwise selected a stale Homebrew Codex even though
	// the account's interactive terminal used its working NVM installation.
	candidates = append(candidates, "/opt/homebrew/bin", "/usr/local/bin")
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, p := range candidates {
		if info, e := os.Stat(p); e == nil && info.IsDir() && !seen[p] {
			paths = append(paths, p)
			seen[p] = true
		}
	}
	_ = os.Setenv("PATH", strings.Join(paths, string(os.PathListSeparator)))
}

func inspectMachine(self Peer) Inventory {
	i := Inventory{ID: self.ID, Name: self.Name, OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU(), Tools: map[string]string{}, Capabilities: map[string]Capability{}}
	if runtime.GOOS == "darwin" {
		i.CPU = probe("sysctl", "-n", "machdep.cpu.brand_string")
		i.MemoryBytes, _ = strconv.ParseUint(probe("sysctl", "-n", "hw.memsize"), 10, 64)
	} else {
		if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "model name") {
					_, i.CPU, _ = strings.Cut(l, ":")
					i.CPU = strings.TrimSpace(i.CPU)
					break
				}
			}
		}
		if b, e := os.ReadFile("/proc/meminfo"); e == nil {
			for _, l := range strings.Split(string(b), "\n") {
				f := strings.Fields(l)
				if len(f) >= 2 && f[0] == "MemTotal:" {
					n, _ := strconv.ParseUint(f[1], 10, 64)
					i.MemoryBytes = n * 1024
				}
			}
		}
	}
	i.GPU = probe("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader")
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, name := range []string{"codex", "claude", "gemini", "opencode", "python3", "node", "docker", "pandoc", "pdflatex", "tmux", "ssh", "tailscale"} {
		if _, e := exec.LookPath(name); e != nil {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			flag := "--version"
			if name == "ssh" || name == "tmux" {
				flag = "-V"
			}
			v := probe(name, flag)
			if v == "" {
				v = "installed (version unknown)"
			}
			v = strings.Split(v, "\n")[0]
			mu.Lock()
			i.Tools[name] = v
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return i
}

func (s *Store) Refresh() (Inventory, error) {
	c, e := s.Config()
	if e != nil {
		return Inventory{}, e
	}
	fresh := inspectMachine(c.Self)
	e = withLock(s.path("inventory.lock"), func() error {
		var old Inventory
		err := readJSON(s.path("machines", c.Self.ID+".json"), &old)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		fresh.Revision = old.Revision + 1
		fresh.UpdatedAt = now()
		if old.Capabilities != nil {
			fresh.Capabilities = old.Capabilities
		}
		return writeJSON(s.path("machines", c.Self.ID+".json"), fresh)
	})
	return fresh, e
}

func (s *Store) Capability(name, environment, note string, remove bool) error {
	if name == "" {
		return fmt.Errorf("capability name is required")
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	return withLock(s.path("inventory.lock"), func() error {
		var i Inventory
		if e := readJSON(s.path("machines", c.Self.ID+".json"), &i); e != nil {
			return e
		}
		if i.Capabilities == nil {
			i.Capabilities = map[string]Capability{}
		}
		key := name
		if environment != "" {
			key += "@" + environment
		}
		if remove {
			delete(i.Capabilities, key)
		} else {
			i.Capabilities[key] = Capability{name, environment, note, now()}
		}
		i.Revision++
		i.UpdatedAt = now()
		return writeJSON(s.path("machines", c.Self.ID+".json"), i)
	})
}

func (s *Store) OwnInventory() (Inventory, error) {
	c, e := s.Config()
	if e != nil {
		return Inventory{}, e
	}
	var i Inventory
	e = readJSON(s.path("machines", c.Self.ID+".json"), &i)
	return i, e
}

func (s *Store) CacheInventory(expectedID string, i Inventory) error {
	if !validID.MatchString(i.ID) || i.ID != expectedID {
		return fmt.Errorf("inventory identity mismatch")
	}
	c, e := s.Config()
	if e != nil {
		return e
	}
	if i.ID == c.Self.ID {
		return fmt.Errorf("cannot overwrite this machine's inventory with a peer copy")
	}
	if _, ok := c.Peers[i.ID]; !ok {
		return fmt.Errorf("inventory from unenrolled machine")
	}
	return withLock(s.path("inventory.lock"), func() error {
		var old Inventory
		err := readJSON(s.path("machines", i.ID+".json"), &old)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if e := writeJSON(s.path("contacts", i.ID+".json"), now()); e != nil {
			return e
		}
		if i.Revision <= old.Revision {
			return nil
		}
		return writeJSON(s.path("machines", i.ID+".json"), i)
	})
}

type MachineView struct {
	Peer          Peer       `json:"machine"`
	Inventory     *Inventory `json:"inventory,omitempty"`
	Reachable     *bool      `json:"reachable,omitempty"`
	Error         string     `json:"error,omitempty"`
	LastContact   string     `json:"last_contact,omitempty"`
	Running       *int       `json:"running,omitempty"`
	MaxJobs       *int       `json:"max_jobs,omitempty"`
	DiskFreeBytes *uint64    `json:"disk_free_bytes,omitempty"`
}

func (s *Store) Machines(check bool) ([]MachineView, error) {
	c, e := s.Config()
	if e != nil {
		return nil, e
	}
	peers := []Peer{c.Self}
	for _, p := range c.Peers {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	views := make([]MachineView, len(peers))
	var wg sync.WaitGroup
	for idx, p := range peers {
		views[idx].Peer = p
		_ = readJSON(s.path("contacts", p.ID+".json"), &views[idx].LastContact)
		var inv Inventory
		if readJSON(s.path("machines", p.ID+".json"), &inv) == nil {
			views[idx].Inventory = &inv
		}
		if check {
			wg.Add(1)
			go func(idx int, p Peer) {
				defer wg.Done()
				var resp Info
				var err error
				if p.ID != c.Self.ID {
					err = s.Call(p, Request{Action: "info"}, &resp)
				} else {
					resp, err = s.Info()
				}
				good := err == nil
				if err != nil {
					views[idx].Error = err.Error()
				} else {
					views[idx].Inventory = &resp.Inventory
					views[idx].Running = &resp.Running
					views[idx].MaxJobs = &resp.MaxJobs
					views[idx].DiskFreeBytes = &resp.DiskFreeBytes
					views[idx].LastContact = now()
					if p.ID != c.Self.ID {
						if e := s.CacheInventory(p.ID, resp.Inventory); e != nil {
							views[idx].Error = e.Error()
						}
					}
				}
				views[idx].Reachable = &good
			}(idx, p)
		}
	}
	wg.Wait()
	return views, nil
}

func (s *Store) Sync() []string {
	c, e := s.Config()
	if e != nil {
		return []string{e.Error()}
	}
	own, e := s.OwnInventory()
	if e != nil {
		return []string{e.Error()}
	}
	var issues []string
	for _, p := range c.Peers {
		if !p.Incoming {
			continue
		}
		var reply Inventory
		if e := s.Call(p, Request{Action: "sync", Inventory: &own}, &reply); e != nil {
			issues = append(issues, p.Name+": "+e.Error())
			continue
		}
		if e := s.CacheInventory(p.ID, reply); e != nil {
			issues = append(issues, p.Name+": "+e.Error())
		}
	}
	return issues
}

func agentPaths(home string) map[string]string {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexFile := filepath.Join(codexHome, "AGENTS.md")
	if b, e := os.ReadFile(filepath.Join(codexHome, "AGENTS.override.md")); e == nil && len(strings.TrimSpace(string(b))) > 0 {
		codexFile = filepath.Join(codexHome, "AGENTS.override.md")
	}
	claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	return map[string]string{"codex": codexFile, "claude": filepath.Join(claudeHome, "CLAUDE.md"), "gemini": filepath.Join(home, ".gemini", "GEMINI.md")}
}
