package mesh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type uninstallOptions struct {
	All, Yes, DryRun, LocalOnly bool
	ExpectID                    string
}

type uninstallEdit struct {
	Path          string
	Before, After []byte
	Mode          os.FileMode
}

type uninstallPlan struct {
	Edits                             []uninstallEdit
	Files, Trees, EmptyDirs, Binaries []string
	Services                          []string
}

func (s *Store) UninstallCLI(args []string) error {
	f := flags("uninstall")
	var o uninstallOptions
	f.BoolVar(&o.All, "all", false, "also uninstall every enrolled computer (upgrade each first)")
	f.BoolVar(&o.Yes, "yes", false, "confirm permanent removal of Mesh data and sessions")
	f.BoolVar(&o.DryRun, "dry-run", false, "preview cleanup without changing files or stopping processes")
	f.BoolVar(&o.LocalOnly, "local-only", false, "skip peer revocation; use when peers are unavailable")
	f.StringVar(&o.ExpectID, "expect-id", "", "require this installation identity (remote uninstall)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || o.All && o.LocalOnly {
		return errors.New("use mesh uninstall [--all | --local-only] [--yes] [--dry-run]")
	}
	return s.uninstall(o, os.Stdin, os.Stdout)
}

func (s *Store) uninstall(o uninstallOptions, input io.Reader, out io.Writer) error {
	plan, err := s.planUninstall()
	if err != nil {
		return err
	}
	var c Config
	err = readJSON(s.path("config.json"), &c)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if o.ExpectID != "" && c.Self.ID != o.ExpectID {
		return errors.New("peer identity changed; refusing to uninstall a different installation")
	}
	if o.All && c.Self.ID == "" {
		return errors.New("--all requires an initialized installation and its peer roster")
	}
	fmt.Fprintf(out, "Remove Mesh on this computer: %s\nStop its service, jobs, and tmux sessions.\n", s.Root)
	for _, e := range plan.Edits {
		fmt.Fprintln(out, "Remove Mesh block:", e.Path)
	}
	for _, paths := range [][]string{plan.Files, plan.Trees, plan.Binaries} {
		for _, p := range paths {
			fmt.Fprintln(out, "Remove:", p)
		}
	}
	fmt.Fprintln(out, "Agent installations, credentials, unrelated SSH settings, and user files outside Mesh state and recorded handoff folders are preserved.")
	var peers []Peer
	for _, p := range c.Peers {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	// Record confirmed remote successes before deleting local credentials. A retry
	// must not require access to a peer which already revoked our key or uninstalled.
	done := map[string]string{}
	if err := readJSON(s.path("uninstall-peers.json"), &done); err != nil && !os.IsNotExist(err) {
		return err
	}
	if done == nil {
		done = map[string]string{}
	}
	for _, p := range peers {
		if o.All {
			fmt.Fprintln(out, "Uninstall enrolled computer:", p.Name)
		} else if !o.LocalOnly {
			fmt.Fprintln(out, "Revoke this computer on peer:", p.Name)
		}
	}
	if !o.DryRun && !o.Yes {
		fmt.Fprint(out, "Permanently delete the listed Mesh data? Type 'uninstall' to continue: ")
		line, _ := bufio.NewReader(input).ReadString('\n')
		if strings.TrimSpace(line) != "uninstall" {
			return errors.New("uninstall cancelled; no changes made")
		}
	}
	if !o.DryRun {
		// An uninstall inside a Mesh pane would kill its own caller mid-cleanup.
		// Check actual ancestry rather than potentially stale shell environment.
		_, inside, err := s.ancestorSession()
		if err != nil {
			return err
		}
		if inside {
			return errors.New("run mesh uninstall from a normal terminal outside Mesh sessions")
		}
	}
	if o.All {
		// Preflight every remaining computer before making any destructive change.
		for _, p := range peers {
			if done[p.ID] == "uninstalled" {
				continue
			}
			if err := s.remoteUninstall(p, true, out); err != nil {
				return fmt.Errorf("preflight %s: %w; upgrade Mesh there or resolve SSH access before retrying", p.Name, err)
			}
		}
	}
	if o.DryRun {
		return nil
	}
	for _, p := range peers {
		action := "revoked"
		if o.All {
			action = "uninstalled"
		}
		if o.LocalOnly || done[p.ID] == action || done[p.ID] == "uninstalled" {
			continue
		}
		if o.All {
			err = s.remoteUninstall(p, false, out)
		} else {
			err = s.Call(p, Request{Action: "revoke", ID: c.Self.ID}, nil)
		}
		if err != nil {
			return fmt.Errorf("%s: %w; local state and executable retained for retry (use --local-only to skip peer cleanup)", p.Name, err)
		}
		done[p.ID] = action
		if err := writeJSON(s.path("uninstall-peers.json"), done); err != nil {
			return err
		}
	}
	if err := s.stopForUninstall(plan); err != nil {
		return err
	}
	// Refresh after workers/services stop: a final write may have added a handoff
	// or edited a shared file since the preview.
	emptyDirs := plan.EmptyDirs
	plan, err = s.planUninstall()
	if err != nil {
		return err
	}
	plan.EmptyDirs = append(plan.EmptyDirs, emptyDirs...)
	for _, edit := range plan.Edits {
		current, err := os.ReadFile(edit.Path)
		if err != nil {
			return err
		}
		if string(current) != string(edit.Before) {
			return fmt.Errorf("%s changed during uninstall; retry", edit.Path)
		}
		if len(strings.TrimSpace(string(edit.After))) == 0 {
			if err := removeFile(edit.Path); err != nil {
				return err
			}
		} else if err := atomicWrite(edit.Path, edit.After, edit.Mode); err != nil {
			return err
		}
	}
	for _, p := range plan.Files {
		if err := removeFile(p); err != nil {
			return err
		}
	}
	for _, p := range plan.Trees {
		if err := ensureNoSymlinks(p, ""); err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	// State and binaries go last so failures leave a usable retry command.
	if err := os.RemoveAll(s.Root); err != nil {
		return err
	}
	for _, p := range plan.Binaries {
		if err := removeFile(p); err != nil {
			return err
		}
	}
	sort.Slice(plan.EmptyDirs, func(i, j int) bool { return len(plan.EmptyDirs[i]) > len(plan.EmptyDirs[j]) })
	for _, p := range plan.EmptyDirs {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	fmt.Fprintln(out, "Mesh uninstalled.")
	return nil
}

func removeFile(p string) error {
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) remoteUninstall(p Peer, preview bool, out io.Writer) error {
	args, err := s.SSHArgs(p, false)
	if err != nil {
		return err
	}
	remoteArgs := []string{"uninstall", "--local-only", "--yes", "--expect-id", p.ID}
	if preview {
		remoteArgs = append(remoteArgs, "--dry-run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(args, p.Endpoint.Host, remoteMesh(p, remoteArgs...))...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (s *Store) planUninstall() (uninstallPlan, error) {
	var p uninstallPlan
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return p, err
	}
	home, err := filepath.Abs(s.UserHome)
	if err != nil {
		return p, err
	}
	// Never recursively remove a home directory, its ancestors, or a symlinked
	// state tree. Custom state directories must have a recognizable Mesh config.
	if root == home || strings.HasPrefix(home, root+string(os.PathSeparator)) || root == string(os.PathSeparator) {
		return p, errors.New("refusing to remove MESH_HOME: it contains the user home")
	}
	if err := ensureNoSymlinks(root, ""); err != nil {
		return p, err
	}
	if filepath.Clean(root) != filepath.Join(home, ".ai-mesh") {
		if _, err := os.Stat(root); err == nil {
			c, err := s.Config()
			if err != nil || !validID.MatchString(c.Self.ID) {
				return p, errors.New("custom MESH_HOME lacks a valid Mesh configuration; refusing recursive removal")
			}
			// Initialization can be pointed at an existing project or shared
			// directory. A config alone is not permission to delete its contents.
			owned := strings.Fields("keys machines jobs receipts sessions logs projects contacts pending runtime handoffs ssh-server config.json config.lock ssh-access.lock known_hosts revoked.json inventory.lock inventory-sync.lock inventory-sync.json integration-paths.lock integration-paths.json daemon.lock daemon-status.json jobs.lock jobs.sock scheduler.lock uninstall-peers.json")
			entries, err := os.ReadDir(root)
			if err != nil {
				return p, err
			}
			for _, entry := range entries {
				found := false
				for _, name := range owned {
					if entry.Name() == name {
						found = true
						break
					}
				}
				if !found {
					return p, fmt.Errorf("custom MESH_HOME contains unrecognized entry %s; refusing recursive removal", entry.Name())
				}
			}
		} else if !os.IsNotExist(err) {
			return p, err
		}
	}
	instructionPaths := map[string]bool{}
	for _, file := range agentPaths(s.UserHome) {
		instructionPaths[file] = true
	}
	for _, dir := range []string{filepath.Join(home, ".codex"), os.Getenv("CODEX_HOME")} {
		if dir != "" {
			instructionPaths[filepath.Join(dir, "AGENTS.md")] = true
			instructionPaths[filepath.Join(dir, "AGENTS.override.md")] = true
		}
	}
	instructionPaths[filepath.Join(home, ".claude", "CLAUDE.md")] = true
	var recorded []string
	if err := readJSON(s.path("integration-paths.json"), &recorded); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	for _, file := range recorded {
		if !filepath.IsAbs(file) {
			return p, errors.New("invalid recorded integration path")
		}
		instructionPaths[file] = true
	}
	var files []string
	for file := range instructionPaths {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		if err := p.cleanSharedFile(file, func(old string) (string, error) { return managedInstructions(old, "", true) }, true); err != nil {
			return p, err
		}
	}
	if err := p.cleanSharedFile(filepath.Join(home, ".ssh", "config"), withoutSSHConfig, true); err != nil {
		return p, err
	}
	if err := p.cleanSharedFile(filepath.Join(home, ".ssh", "authorized_keys"), func(old string) (string, error) {
		var b strings.Builder
		for _, line := range strings.SplitAfter(old, "\n") {
			if !strings.Contains(line, " ai-mesh-managed:") {
				b.WriteString(line)
			}
		}
		return b.String(), nil
	}, false); err != nil {
		return p, err
	}
	for _, service := range []string{filepath.Join(home, "Library", "LaunchAgents", "dev.ai-mesh.agent.plist"), filepath.Join(home, "Library", "LaunchAgents", "dev.ai-mesh.ssh.plist"), filepath.Join(home, ".config", "systemd", "user", "ai-mesh.service")} {
		body, err := os.ReadFile(service)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return p, err
		}
		ownsRoot := strings.Contains(string(body), "<string>"+xmlText(root)+"</string>") || strings.Contains(string(body), "<string>"+xmlText(filepath.Join(root, "ssh-server", "sshd_config"))+"</string>") || strings.Contains(string(body), "Environment="+systemdQuote("MESH_HOME="+root)+"\n")
		if !ownsRoot {
			return p, fmt.Errorf("service %s belongs to another Mesh state directory", service)
		}
		p.Services = append(p.Services, service)
		p.Files = append(p.Files, service)
		p.EmptyDirs = append(p.EmptyDirs, filepath.Dir(service))
	}
	links, err := filepath.Glob(filepath.Join(home, ".config", "systemd", "user", "*.wants", "ai-mesh.service"))
	if err != nil {
		return p, err
	}
	for _, link := range links {
		target, err := os.Readlink(link)
		if err != nil {
			return p, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		if filepath.Clean(target) != filepath.Join(home, ".config", "systemd", "user", "ai-mesh.service") {
			return p, fmt.Errorf("unexpected service link %s", link)
		}
		p.Files = append(p.Files, link)
		p.EmptyDirs = append(p.EmptyDirs, filepath.Dir(link))
	}
	receipts, err := filepath.Glob(s.path("handoffs", "*.json"))
	if err != nil {
		return p, err
	}
	for _, file := range receipts {
		var h HandoffReceipt
		if err := readJSON(file, &h); err != nil {
			return p, err
		}
		if !validID.MatchString(h.ID) || !filepath.IsAbs(h.Project) {
			return p, fmt.Errorf("invalid handoff receipt %s", file)
		}
		dir := filepath.Join(h.Project, ".mesh", "handoffs", h.ID)
		if h.ContextPath != filepath.Join(dir, "CONTEXT.md") || h.InputsPath != filepath.Join(dir, "inputs") {
			return p, fmt.Errorf("invalid handoff paths in %s", file)
		}
		if err := ensureNoSymlinks(dir, ""); err != nil {
			return p, err
		}
		p.Trees = append(p.Trees, dir)
		p.EmptyDirs = append(p.EmptyDirs, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir)))
	}
	sockets := filepath.Join(os.TempDir(), "mesh-"+digest([]byte(s.Root))[:12])
	if err := ensureNoSymlinks(sockets, ""); err != nil {
		return p, err
	}
	p.Trees = append(p.Trees, sockets)
	// The server uses its own tmux socket; never touch the user's default server.
	tmuxTmp := os.Getenv("TMUX_TMPDIR")
	if tmuxTmp == "" {
		tmuxTmp = "/tmp"
	}
	p.Files = append(p.Files, filepath.Join(tmuxTmp, "tmux-"+strconv.Itoa(os.Getuid()), s.tmuxName()))
	dirs := map[string]bool{filepath.Join(home, ".local", "bin"): true}
	if dir := os.Getenv("MESH_INSTALL_DIR"); dir != "" {
		dir, err = filepath.Abs(dir)
		if err != nil {
			return p, err
		}
		dirs[dir] = true
	}
	exe, err := os.Executable()
	if err != nil {
		return p, err
	}
	if filepath.Base(exe) == "mesh" {
		dirs[filepath.Dir(exe)] = true
	}
	names := []string{"mesh", "mesh-darwin-arm64", "mesh-darwin-amd64", "mesh-linux-arm64", "mesh-linux-amd64"}
	for dir := range dirs {
		for _, name := range names {
			file := filepath.Join(dir, name)
			info, err := os.Lstat(file)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return p, err
			}
			if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				return p, fmt.Errorf("refusing to remove non-file executable %s", file)
			}
			p.Binaries = append(p.Binaries, file)
		}
		p.EmptyDirs = append(p.EmptyDirs, dir)
	}
	sort.Strings(p.Binaries)
	return p, nil
}

func (p *uninstallPlan) cleanSharedFile(file string, clean func(string) (string, error), sidecars bool) error {
	original := file
	// Integrate and SSHConfig edit symlink targets, so uninstall does the same.
	if info, err := os.Lstat(file); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(file)
		if err != nil {
			return err
		}
		file = resolved
	}
	if sidecars {
		p.Files = append(p.Files, file+".pre-mesh", file+".mesh.lock")
		if file != original {
			p.Files = append(p.Files, original+".pre-mesh", original+".mesh.lock")
		}
	}
	old, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	updated, err := clean(string(old))
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if updated == string(old) {
		return nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	already := false
	for _, edit := range p.Edits {
		if edit.Path == file {
			already = true
			break
		}
	}
	if !already {
		p.Edits = append(p.Edits, uninstallEdit{file, old, []byte(updated), info.Mode().Perm()})
	}
	if strings.TrimSpace(updated) == "" && original != file {
		p.Files = append(p.Files, original)
	}
	// Empty provider directories created solely for the integration can go too.
	p.EmptyDirs = append(p.EmptyDirs, filepath.Dir(original))
	return nil
}

func (s *Store) stopForUninstall(p uninstallPlan) error {
	for _, file := range p.Services {
		if strings.HasSuffix(file, ".plist") && runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("launchctl"); err != nil {
				return err
			}
			label := strings.TrimSuffix(filepath.Base(file), ".plist")
			target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
			if err := exec.Command("launchctl", "bootout", target).Run(); err != nil {
				status, statusErr := exec.Command("launchctl", "print", target).CombinedOutput()
				if statusErr == nil || !strings.Contains(string(status), "Could not find service") && !strings.Contains(string(status), "Could not find domain") {
					return fmt.Errorf("could not stop %s: %w", label, err)
				}
			}
		} else if strings.HasSuffix(file, ".service") && runtime.GOOS == "linux" {
			if out, err := exec.Command("systemctl", "--user", "disable", "--now", "ai-mesh.service").CombinedOutput(); err != nil {
				return fmt.Errorf("stop service: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
	}
	if err := s.stopUninstallDaemon(); err != nil {
		return err
	}
	jobs, err := s.LocalJobs()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, j := range jobs {
		if !terminal(j.State) {
			if err := s.Cancel(j.Task.ID); err != nil {
				return err
			}
		}
	}
	for _, j := range jobs {
		if err := waitUnlocked(s.jobPath(j.Task.ID, "worker.lock"), 8*time.Second); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		if out, err := s.tmux("kill-server").CombinedOutput(); err != nil && !strings.Contains(string(out), "no server running") && !strings.Contains(string(out), "No such file or directory") {
			return fmt.Errorf("stop Mesh sessions: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	locks, err := filepath.Glob(s.path("sessions", "*.lock"))
	if err != nil {
		return err
	}
	for _, file := range locks {
		if err := waitUnlocked(file, 18*time.Second); err != nil {
			return err
		}
	}
	// Remove definitions before reload; the manager must not retain a stale unit.
	for _, file := range p.Services {
		if err := removeFile(file); err != nil {
			return err
		}
	}
	if runtime.GOOS == "linux" && len(p.Services) > 0 {
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("reload user services: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func waitUnlocked(path string, timeout time.Duration) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Mesh process still owns %s; stop it and retry uninstall", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Store) stopUninstallDaemon() error {
	if waitUnlocked(s.path("daemon.lock"), 0) == nil {
		return nil
	}
	var status struct {
		PID int `json:"pid"`
	}
	if err := readJSON(s.path("daemon-status.json"), &status); err != nil {
		return fmt.Errorf("stop the running Mesh daemon and retry: %w", err)
	}
	// A stale status file must never be enough to signal an unrelated process.
	exe, err := executable()
	if err != nil {
		return err
	}
	b, err := exec.Command("ps", "-p", strconv.Itoa(status.PID), "-o", "command=").Output()
	command := strings.TrimSpace(string(b))
	program, args, found := strings.Cut(command, " daemon")
	resolved, resolveErr := filepath.EvalSymlinks(program)
	if err != nil || resolveErr != nil || status.PID <= 1 || !found || resolved != exe || args != "" && !strings.HasPrefix(args, " ") {
		return errors.New("cannot verify running Mesh daemon identity; stop it and retry uninstall")
	}
	if err := syscall.Kill(status.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return waitUnlocked(s.path("daemon.lock"), 10*time.Second)
}
