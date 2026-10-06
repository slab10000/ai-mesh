package mesh

import (
	"fmt"
	"net"
	"os"
	"time"
)

// This is a private local Unix datagram socket, never a network listener. The
// durable queued job is the source of truth; a lost wakeup is recovered on the
// service's next tick. Worker locks make repeated notifications idempotent.
func (s *Store) wakeJob(id string) bool {
	conn, err := net.DialTimeout("unixgram", s.path("jobs.sock"), 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = conn.Write([]byte(id))
	return err == nil
}

func (s *Store) jobListener() (*net.UnixConn, error) {
	path := s.path("jobs.sock")
	// The daemon holds daemon.lock before replacing any stale socket.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("job notification path is not a socket: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		conn.Close()
		os.Remove(path)
		return nil, err
	}
	go func() {
		b := make([]byte, 128)
		for {
			n, _, err := conn.ReadFromUnix(b)
			if err != nil {
				return
			}
			id := string(b[:n])
			if !validID.MatchString(id) {
				continue
			}
			job, err := s.Job(id)
			if err != nil || job.State != "queued" {
				continue
			}
			if err := s.spawnWorker(id); err != nil {
				fmt.Fprintln(os.Stderr, "start queued job:", err)
			}
		}
	}()
	return conn, nil
}
