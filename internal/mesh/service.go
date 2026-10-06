package mesh

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, "\n", `\n`).Replace(s) + `"`
}

// launchd may finish removing an old job shortly after bootout returns.
// Retry that upgrade race briefly; preserve the real diagnostic on failure.
func bootstrapLaunchAgent(domain, file string) error {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		out, err := exec.Command("launchctl", "bootstrap", domain, file).CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("launchd configuration saved but could not load in %s: %w", domain, last)
}

func serviceDefinition(platform, exe, root, home, pathEnv string) (string, error) {
	switch platform {
	case "darwin":
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.ai-mesh.agent</string>
<key>ProgramArguments</key><array><string>` + xmlText(exe) + `</string><string>daemon</string></array>
<key>EnvironmentVariables</key><dict><key>MESH_HOME</key><string>` + xmlText(root) + `</string><key>MESH_USER_HOME</key><string>` + xmlText(home) + `</string><key>PATH</key><string>` + xmlText(pathEnv) + `</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>` + xmlText(filepath.Join(root, "logs", "daemon.log")) + `</string>
<key>StandardErrorPath</key><string>` + xmlText(filepath.Join(root, "logs", "daemon.log")) + `</string>
</dict></plist>
`, nil
	case "linux":
		return "[Unit]\nDescription=ai-mesh inventory and result delivery\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + systemdQuote(exe) + " daemon\nEnvironment=" + systemdQuote("MESH_HOME="+root) + "\nEnvironment=" + systemdQuote("MESH_USER_HOME="+home) + "\nEnvironment=" + systemdQuote("PATH="+pathEnv) + "\nRestart=on-failure\nRestartSec=5\nUMask=0077\n\n[Install]\nWantedBy=default.target\n", nil
	}
	return "", errors.New("services are supported on macOS and Linux")
}

func (s *Store) Service(action string) error {
	if _, e := s.Config(); e != nil {
		return e
	}
	exe, e := executable()
	if e != nil {
		return e
	}
	var file string
	if runtime.GOOS == "darwin" {
		file = filepath.Join(s.UserHome, "Library", "LaunchAgents", "dev.ai-mesh.agent.plist")
	} else {
		file = filepath.Join(s.UserHome, ".config", "systemd", "user", "ai-mesh.service")
	}
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if action == "install" {
		body, e := serviceDefinition(runtime.GOOS, exe, s.Root, s.UserHome, os.Getenv("PATH"))
		if e != nil {
			return e
		}
		if e := atomicWrite(file, []byte(body), 0600); e != nil {
			return e
		}
		if runtime.GOOS == "darwin" {
			domain := "gui/" + strconv.Itoa(os.Getuid())
			_ = exec.Command("launchctl", "bootout", domain+"/dev.ai-mesh.agent").Run()
			if e := bootstrapLaunchAgent(domain, file); e != nil {
				return e
			}
		} else {
			if e := run("systemctl", "--user", "daemon-reload"); e != nil {
				return fmt.Errorf("unit saved; no active systemd user manager (run mesh daemon manually): %w", e)
			}
			if e := run("systemctl", "--user", "enable", "ai-mesh.service"); e != nil {
				return e
			}
			// Enrollment may have atomically replaced the executable. Restart an
			// existing service too, so it actually uses the installed version.
			return run("systemctl", "--user", "restart", "ai-mesh.service")
		}
		return nil
	}
	if action == "uninstall" {
		if runtime.GOOS == "darwin" {
			_ = run("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/dev.ai-mesh.agent")
		} else {
			_ = run("systemctl", "--user", "disable", "--now", "ai-mesh.service")
		}
		if e := os.Remove(file); e != nil && !os.IsNotExist(e) {
			return e
		}
		return nil
	}
	if action == "status" {
		if runtime.GOOS == "darwin" {
			return run("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/dev.ai-mesh.agent")
		}
		return run("systemctl", "--user", "status", "ai-mesh.service")
	}
	return errors.New("use service install, status, or uninstall")
}

func (s *Store) RecoverJobs() []string {
	jobs, e := s.LocalJobs()
	if e != nil {
		return []string{e.Error()}
	}
	var issues []string
	for _, j := range jobs {
		if j.State == "queued" {
			if e := s.spawnWorker(j.Task.ID); e != nil {
				issues = append(issues, e.Error())
			}
			continue
		}
		if j.State != "running" {
			continue
		}
		f, e := os.OpenFile(s.jobPath(j.Task.ID, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			issues = append(issues, e.Error())
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			if e := s.editJob(j.Task.ID, func(current *Job) error {
				if current.State == "running" {
					current.State = "failed"
					current.ExitCode = -1
					current.Error = "worker exited unexpectedly; task was not automatically rerun"
					current.FinishedAt = now()
					current.PID = 0
				}
				return nil
			}); e != nil {
				issues = append(issues, e.Error())
			}
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		_ = f.Close()
	}
	return issues
}

func (s *Store) Tick() []string {
	issues := s.RecoverJobs()
	issues = append(issues, s.PendingChanges()...)
	issues = append(issues, s.Sync()...)
	entries, e := os.ReadDir(s.path("receipts"))
	if e != nil {
		return append(issues, e.Error())
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".request.json") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		r, e := s.Receipt(id)
		if e != nil {
			issues = append(issues, e.Error())
			continue
		}
		if r.Delivery == "delivered" {
			continue
		}
		j, e := s.Status(id)
		statusErr := e
		if e := withLock(s.path("receipts", id+".lock"), func() error {
			fresh, err := s.Receipt(id)
			if err != nil {
				return err
			}
			if fresh.Delivery == "delivered" {
				return nil
			}
			fresh.LastError = ""
			if statusErr != nil {
				fresh.LastError = statusErr.Error()
			} else {
				fresh.LastState = j.State
			}
			return writeJSON(s.path("receipts", id+".json"), fresh)
		}); e != nil {
			issues = append(issues, e.Error())
		}
		if statusErr != nil {
			continue
		}
		if terminal(j.State) {
			if _, e := s.Collect(id, ""); e != nil {
				issues = append(issues, id+": "+e.Error())
			}
		}
	}
	return issues
}

func (s *Store) Daemon(interval time.Duration, once bool) error {
	if interval < time.Second {
		return errors.New("daemon interval must be at least one second")
	}
	if _, e := s.Config(); e != nil {
		return e
	}
	f, e := os.OpenFile(s.path("daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("another Mesh daemon is already running")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var changes <-chan struct{}
	if !once {
		var closeWatcher func()
		changes, closeWatcher, e = s.inventoryChanges(ctx)
		if e != nil {
			return fmt.Errorf("watch machine description: %w", e)
		}
		defer closeWatcher()
		listener, err := s.jobListener()
		if err != nil {
			// An unusually long custom MESH_HOME may exceed the platform's Unix
			// socket limit. Keep periodic recovery and direct CLI dispatch usable.
			fmt.Fprintln(os.Stderr, "job notifications unavailable; using periodic recovery:", err)
		} else {
			defer os.Remove(s.path("jobs.sock"))
			defer listener.Close()
		}
	}
	tick := func() {
		issues := s.Tick()
		for _, issue := range issues {
			fmt.Fprintln(os.Stderr, now(), issue)
		}
		_ = writeJSON(s.path("daemon-status.json"), map[string]any{"pid": os.Getpid(), "last_tick": now(), "issues": issues})
	}
	tick()
	if once {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			for _, issue := range s.Sync() {
				fmt.Fprintln(os.Stderr, now(), "inventory update:", issue)
			}
		case <-ticker.C:
			tick() // Job delivery and retries of unacknowledged inventory only.
		}
	}
}
