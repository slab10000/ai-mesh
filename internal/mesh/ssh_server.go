package mesh

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// An optional per-account server avoids requiring macOS Remote Login or root.
// It binds only to the explicitly selected local/private IP and accepts Mesh
// keys only. The system SSH service and unrelated authorized keys are untouched.
func sshServerConfig(address string, port int, username, root string) (string, error) {
	ip := net.ParseIP(address)
	tailnet := net.ParseIP("100.64.0.0")
	_, tailRange, _ := net.ParseCIDR(tailnet.String() + "/10")
	if ip == nil || ip.IsUnspecified() || !(ip.IsLoopback() || ip.IsPrivate() || tailRange.Contains(ip)) {
		return "", errors.New("SSH listener requires an explicit local/private IP (for example this computer's Tailscale IP)")
	}
	if port < 1024 || port > 65535 || !validID.MatchString(username) || strings.ContainsAny(root, "\r\n\x00") {
		return "", errors.New("invalid SSH listener account, path, or unprivileged port")
	}
	q := func(p string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"` }
	return fmt.Sprintf("Port %d\nListenAddress %s\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nPermitRootLogin no\nAllowUsers %s\nAllowAgentForwarding no\nX11Forwarding no\nLogLevel VERBOSE\n", port, address,
		q(filepath.Join(root, "hostkey")), q(filepath.Join(root, "pid")), q(filepath.Join(root, "authorized_keys")), username), nil
}

func (s *Store) SSHServer(action, address string, port int) error {
	if runtime.GOOS != "darwin" {
		return errors.New("the optional user SSH listener currently supports macOS; use your normal SSH service on Linux")
	}
	c, err := s.Config()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	label := "dev.ai-mesh.ssh"
	plist := filepath.Join(s.UserHome, "Library", "LaunchAgents", label+".plist")
	if action == "status" {
		cmd := exec.Command("launchctl", "print", domain+"/"+label)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if action == "uninstall" {
		// Publish the access change before stopping the listener.
		err := s.SetIncoming(false)
		_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
		if removeErr := os.Remove(plist); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		return err
	}
	if action != "install" {
		return errors.New("use ssh-server install, status, or uninstall")
	}
	root := s.path("ssh-server")
	config, err := sshServerConfig(address, port, c.Self.Endpoint.User, root)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	key := filepath.Join(root, "hostkey")
	if _, err = os.Stat(key); os.IsNotExist(err) {
		if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			return fmt.Errorf("create SSH host key: %w: %s", err, out)
		}
	} else if err != nil {
		return err
	}
	file := filepath.Join(root, "sshd_config")
	if err = atomicWrite(file, []byte(config), 0600); err != nil {
		return err
	}
	if out, err := exec.Command("/usr/sbin/sshd", "-t", "-f", file).CombinedOutput(); err != nil {
		return fmt.Errorf("validate SSH listener: %w: %s", err, out)
	}
	body := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>
<key>Label</key><string>` + label + `</string>
<key>ProgramArguments</key><array><string>/usr/sbin/sshd</string><string>-D</string><string>-e</string><string>-f</string><string>` + xmlText(file) + `</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer>
<key>StandardErrorPath</key><string>` + xmlText(s.path("logs", "ssh-server.log")) + `</string>
</dict></plist>`
	if err = atomicWrite(plist, []byte(body), 0600); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if err := bootstrapLaunchAgent(domain, plist); err != nil {
		return fmt.Errorf("load SSH listener: %w", err)
	}
	ready := false
	for i := 0; i < 30; i++ {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(address, strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("SSH listener did not start; inspect %s", s.path("logs", "ssh-server.log"))
	}
	// A different process could already own this port. Do not publish a usable
	// endpoint until the listener proves it presents our newly generated key.
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		return errors.New("invalid SSH listener host key")
	}
	expected := strings.Join(fields[:2], " ")
	scan, err := exec.Command("ssh-keyscan", "-T", "2", "-t", "ed25519", "-p", strconv.Itoa(port), address).Output()
	verified := false
	for _, line := range strings.Split(string(scan), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && strings.Join(f[1:3], " ") == expected {
			verified = true
		}
	}
	if err != nil || !verified {
		return errors.New("SSH listener host key check failed; check for an occupied port before enabling incoming access")
	}
	if err = s.updateConfig(func(c *Config) error {
		c.Self.Endpoint.Host, c.Self.Endpoint.Port = address, port
		return nil
	}); err != nil {
		return err
	}
	return s.SetIncoming(true)
}

func (s *Store) localHostKeys() []string {
	keys := hostKeys()
	if b, err := os.ReadFile(s.path("ssh-server", "hostkey.pub")); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 2 {
			keys = append(keys, strings.Join(fields[:2], " "))
		}
	}
	return keys
}
