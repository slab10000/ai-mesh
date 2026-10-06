package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Info struct {
	Protocol      int       `json:"protocol"`
	Version       string    `json:"version"`
	Peer          Peer      `json:"peer"`
	Inventory     Inventory `json:"inventory"`
	Running       int       `json:"running"`
	MaxJobs       int       `json:"max_jobs"`
	DiskFreeBytes uint64    `json:"disk_free_bytes"`
}
type Request struct {
	Action    string         `json:"action"`
	ID        string         `json:"id,omitempty"`
	Task      *Task          `json:"task,omitempty"`
	Inventory *Inventory     `json:"inventory,omitempty"`
	Peers     []Peer         `json:"peers,omitempty"`
	RejoinIDs []string       `json:"rejoin_ids,omitempty"`
	RemoveIDs []string       `json:"remove_ids,omitempty"`
	Offset    int64          `json:"offset,omitempty"`
	Session   *RemoteSession `json:"session,omitempty"`
}
type Response struct {
	Protocol  int             `json:"protocol"`
	MachineID string          `json:"machine_id"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (s *Store) SSHArgs(p Peer, interactive bool) ([]string, error) {
	if e := validateEndpoint(p.Endpoint); e != nil {
		return nil, e
	}
	args := []string{"-p", strconv.Itoa(p.Endpoint.Port), "-l", p.Endpoint.User, "-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ForwardAgent=no", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-i", s.path("keys", "id_ed25519"), "-o", "UserKnownHostsFile=" + quote(s.path("known_hosts")) + " " + quote(filepath.Join(s.UserHome, ".ssh", "known_hosts"))}
	if interactive {
		args = append(args, "-tt")
	}
	return args, nil
}

func remoteMesh(p Peer, args ...string) string {
	// Home is learned over an authenticated SSH connection at enrollment.
	bin := filepath.Join(p.Home, ".local", "bin", "mesh")
	return "exec " + shellJoin(append([]string{bin}, args...)...)
}

func (s *Store) Call(p Peer, r Request, into any) error {
	args, e := s.SSHArgs(p, false)
	if e != nil {
		return e
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > MaxWireBytes {
		return errors.New("request exceeds wire limit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(args, p.Endpoint.Host, remoteMesh(p, "_rpc"))...)
	cmd.Stdin = bytes.NewReader(b)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return e
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, MaxWireBytes+1))
	if len(out) > MaxWireBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return errors.New("peer response exceeds wire limit")
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("SSH to %s: %w: %s", p.Name, waitErr, strings.TrimSpace(stderr.String()))
	}
	var response Response
	if e := json.Unmarshal(out, &response); e != nil {
		return fmt.Errorf("invalid Mesh reply from %s (check remote shell startup output): %w", p.Name, e)
	}
	if response.Protocol != Protocol {
		return errors.New("peer protocol is incompatible")
	}
	if response.MachineID != p.ID {
		return errors.New("peer identity changed; refusing to use a different machine at this address")
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if into != nil {
		return json.Unmarshal(response.Data, into)
	}
	return nil
}

func (s *Store) Info() (Info, error) {
	c, e := s.Config()
	if e != nil {
		return Info{}, e
	}
	i, e := s.OwnInventory()
	if e != nil {
		return Info{}, e
	}
	jobs, e := s.LocalJobs()
	if e != nil {
		return Info{}, e
	}
	n := 0
	for _, j := range jobs {
		if j.State == "running" {
			n++
		}
	}
	var disk syscall.Statfs_t
	_ = syscall.Statfs(s.Root, &disk)
	return Info{Protocol: Protocol, Version: Version, Peer: c.Self, Inventory: i, Running: n, MaxJobs: c.MaxJobs, DiskFreeBytes: disk.Bavail * uint64(disk.Bsize)}, nil
}

func (s *Store) Handle(r Request) (any, error) {
	c, e := s.Config()
	if e != nil {
		return nil, e
	}
	if os.Getenv("SSH_CONNECTION") != "" && !c.Self.Incoming {
		return nil, errors.New("this machine does not accept incoming Mesh requests")
	}
	switch r.Action {
	case "prepare-session":
		if r.Session == nil {
			return nil, errors.New("missing session")
		}
		return nil, s.prepareSession(*r.Session)
	case "info":
		return s.Info()
	case "sync":
		if r.Inventory == nil {
			return nil, errors.New("missing inventory")
		}
		if e := s.CacheInventory(r.Inventory.ID, *r.Inventory); e != nil {
			return nil, e
		}
		return s.OwnInventory()
	case "roster":
		for _, id := range r.RejoinIDs {
			found := false
			for _, p := range r.Peers {
				if p.ID == id {
					found = true
				}
			}
			if !found {
				return nil, errors.New("rejoining peer missing from roster")
			}
			if e := s.unrevoke(id); e != nil {
				return nil, e
			}
		}
		return nil, s.ImportPeers(r.Peers)
	case "peer":
		if len(r.Peers) != 1 {
			return nil, errors.New("expected one existing peer")
		}
		if _, ok := c.Peers[r.Peers[0].ID]; !ok {
			return nil, errors.New("cannot update an unenrolled peer")
		}
		return nil, s.ImportPeers(r.Peers)
	case "revoke":
		return nil, s.RemovePeer(r.ID)
	case "detach":
		for _, id := range r.RemoveIDs {
			if !validID.MatchString(id) || id == c.Self.ID {
				return nil, errors.New("invalid peer removal")
			}
		}
		for _, id := range r.RemoveIDs {
			if e := s.RemovePeer(id); e != nil {
				return nil, e
			}
		}
		return nil, nil
	case "submit":
		if r.Task == nil {
			return nil, errors.New("missing task")
		}
		return s.Accept(*r.Task)
	case "status":
		return s.Job(r.ID)
	case "logs":
		return s.Logs(r.ID, r.Offset)
	case "cancel":
		return nil, s.Cancel(r.ID)
	case "artifacts":
		j, e := s.Job(r.ID)
		if e != nil {
			return nil, e
		}
		if !terminal(j.State) {
			return nil, errors.New("job is still running")
		}
		return outputFiles(s.jobPath(r.ID, "work", "outputs"))
	case "ack":
		return nil, s.editJob(r.ID, func(j *Job) error {
			if !terminal(j.State) {
				return errors.New("job is still running")
			}
			j.Delivery = "delivered"
			return nil
		})
	default:
		return nil, fmt.Errorf("unknown RPC action %q", r.Action)
	}
}

func (s *Store) RPC(in io.Reader, out io.Writer) error {
	b, e := io.ReadAll(io.LimitReader(in, MaxWireBytes+1))
	if e != nil {
		return e
	}
	response := Response{Protocol: Protocol}
	if c, err := s.Config(); err == nil {
		response.MachineID = c.Self.ID
	}
	if len(b) > MaxWireBytes {
		response.Error = "request exceeds wire limit"
	} else {
		var r Request
		if e = json.Unmarshal(b, &r); e == nil {
			var value any
			value, e = s.Handle(r)
			if e == nil {
				response.Data, e = json.Marshal(value)
			}
		}
		if e != nil {
			response.Error = e.Error()
		}
	}
	return json.NewEncoder(out).Encode(response)
}
