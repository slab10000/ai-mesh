package mesh

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Task struct {
	ID              string   `json:"id"`
	Agent           string   `json:"agent"`
	Prompt          string   `json:"prompt,omitempty"`
	Command         []string `json:"command,omitempty"`
	Inputs          []File   `json:"inputs,omitempty"`
	OriginID        string   `json:"origin_id"`
	ParentID        string   `json:"parent_id,omitempty"`
	RootID          string   `json:"root_id"`
	Depth           int      `json:"depth"`
	MaxDepth        int      `json:"max_depth"`
	MaxChildren     int      `json:"max_children"`
	Allowed         []string `json:"allowed,omitempty"`
	RequiredOutputs []string `json:"required_outputs,omitempty"`
}
type Job struct {
	Task        Task     `json:"task"`
	Fingerprint string   `json:"fingerprint"`
	State       string   `json:"state"`
	Delivery    string   `json:"delivery"`
	CreatedAt   string   `json:"created_at"`
	StartedAt   string   `json:"started_at,omitempty"`
	FinishedAt  string   `json:"finished_at,omitempty"`
	Error       string   `json:"error,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
	ExitCode    int      `json:"exit_code"`
	PID         int      `json:"pid,omitempty"`
	Children    []string `json:"children,omitempty"`
}
type Receipt struct {
	ID        string `json:"id"`
	TargetID  string `json:"target_id"`
	Output    string `json:"output"`
	Delivery  string `json:"delivery"`
	LastState string `json:"last_state"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at"`
}
type LogChunk struct {
	Text   string `json:"text"`
	Offset int64  `json:"offset"`
	State  string `json:"state"`
}

func terminal(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "needs_attention"
}
func (s *Store) jobPath(id string, parts ...string) string {
	return s.path(append([]string{"jobs", id}, parts...)...)
}
func (s *Store) Job(id string) (Job, error) {
	var j Job
	if !validID.MatchString(id) {
		return j, errors.New("invalid job ID")
	}
	e := readJSON(s.jobPath(id, "job.json"), &j)
	return j, e
}
func (s *Store) editJob(id string, fn func(*Job) error) error {
	return withLock(s.jobPath(id, "state.lock"), func() error {
		j, e := s.Job(id)
		if e != nil {
			return e
		}
		if e := fn(&j); e != nil {
			return e
		}
		return writeJSON(s.jobPath(id, "job.json"), j)
	})
}

func validateTask(t Task, selfID string) error {
	for _, name := range t.RequiredOutputs {
		if !safeRelative(name) {
			return fmt.Errorf("unsafe expected output %q", name)
		}
	}
	if !validID.MatchString(t.ID) || !validID.MatchString(t.OriginID) || !validID.MatchString(t.RootID) {
		return errors.New("invalid task identity")
	}
	if t.ParentID != "" && !validID.MatchString(t.ParentID) {
		return errors.New("invalid parent ID")
	}
	if t.Depth < 0 || t.MaxDepth < 0 || t.MaxDepth > 8 || t.Depth > t.MaxDepth || t.MaxChildren < 0 || t.MaxChildren > 32 {
		return errors.New("invalid delegation limits")
	}
	if len(t.Allowed) > 0 {
		ok := false
		for _, id := range t.Allowed {
			if id == selfID {
				ok = true
			}
		}
		if !ok {
			return errors.New("task placement policy excludes this machine")
		}
	}
	if t.Agent == "shell" {
		if len(t.Command) == 0 {
			return errors.New("shell task requires a command after --")
		}
	} else if t.Agent != "codex" && t.Agent != "claude" {
		return errors.New("background tasks support codex, claude, or shell")
	}
	if t.Agent != "shell" && strings.TrimSpace(t.Prompt) == "" {
		return errors.New("agent task requires a prompt")
	}
	return validateFiles(t.Inputs)
}

func (s *Store) Accept(t Task) (Job, error) {
	return s.accept(t, false)
}

func (s *Store) accept(t Task, fromPeer bool) (Job, error) {
	c, e := s.Config()
	if e != nil {
		return Job{}, e
	}
	if e := validateTask(t, c.Self.ID); e != nil {
		return Job{}, e
	}
	b, _ := json.Marshal(t)
	fingerprint := digest(b)
	var result Job
	e = withLock(s.path("jobs.lock"), func() error {
		old, err := s.Job(t.ID)
		if err == nil {
			if old.Fingerprint != fingerprint {
				return errors.New("job ID already exists with different input")
			}
			result = old
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		for _, d := range []string{"work/inputs", "work/outputs"} {
			if e := os.MkdirAll(s.jobPath(t.ID, d), 0700); e != nil {
				return e
			}
		}
		if e := materialize(s.jobPath(t.ID, "work", "inputs"), t.Inputs); e != nil {
			return e
		}
		t.Inputs = nil
		result = Job{Task: t, Fingerprint: fingerprint, State: "queued", Delivery: "pending", CreatedAt: now(), ExitCode: -1}
		return writeJSON(s.jobPath(t.ID, "job.json"), result)
	})
	if e != nil {
		return result, e
	}
	// Peer requests use the installed service's environment when available.
	// Local CLI submissions retain their caller's environment and permissions.
	if result.State == "queued" && (!fromPeer || !s.wakeJob(t.ID)) {
		if e := s.spawnWorker(t.ID); e != nil {
			return result, e
		}
	}
	return result, nil
}

func (s *Store) spawnWorker(id string) error {
	exe, e := executable()
	if e != nil {
		return e
	}
	log, e := os.OpenFile(s.jobPath(id, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	cmd := exec.Command(exe, "_worker", id)
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome, "MESH_JOB_ID": ""})
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e := cmd.Start(); e != nil {
		return e
	}
	return cmd.Process.Release()
}

func agentCommand(t Task, work string) (*exec.Cmd, error) {
	prompt := t.Prompt + "\n\nMesh task: inputs are in ./inputs. Write all deliverable files in ./outputs. Report errors honestly. Use mesh jobs/status/watch/collect to manage children. The task's placement and delegation limits must be respected."
	if len(t.RequiredOutputs) > 0 {
		prompt += "\nRequired files inside outputs/: " + strings.Join(t.RequiredOutputs, ", ")
	}
	switch t.Agent {
	case "shell":
		if len(t.Command) == 0 {
			return nil, errors.New("empty command")
		}
		return exec.Command(t.Command[0], t.Command[1:]...), nil
	case "codex":
		cmd := exec.Command("codex", "exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "--color", "never", "-")
		cmd.Stdin = strings.NewReader(prompt)
		return cmd, nil
	case "claude":
		cmd := exec.Command("claude", "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits")
		cmd.Stdin = strings.NewReader(prompt)
		return cmd, nil
	}
	return nil, errors.New("unsupported agent")
}

func (s *Store) Worker(id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid job ID")
	}
	f, e := os.OpenFile(s.jobPath(id, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		if e == syscall.EWOULDBLOCK {
			return nil
		}
		return e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	c, e := s.Config()
	if e != nil {
		return e
	}
	j, e := s.Job(id)
	if e != nil {
		return e
	}
	if j.State != "queued" {
		return nil
	}
	// A worker owns its job lock while waiting for a node-local execution slot.
	for {
		started := false
		e = withLock(s.path("scheduler.lock"), func() error {
			jobs, err := s.LocalJobs()
			if err != nil {
				return err
			}
			active := 0
			for _, other := range jobs {
				if other.State == "running" {
					active++
				}
			}
			return s.editJob(id, func(current *Job) error {
				if current.State != "queued" {
					return nil
				}
				if active < c.MaxJobs {
					current.State = "running"
					current.StartedAt = now()
					current.PID = os.Getpid()
					started = true
				}
				return nil
			})
		})
		if e != nil {
			return e
		}
		j, e = s.Job(id)
		if e != nil {
			return e
		}
		if terminal(j.State) {
			return nil
		}
		if started {
			break
		}
		time.Sleep(time.Second)
	}
	work := s.jobPath(id, "work")
	cmd, e := agentCommand(j.Task, work)
	if e != nil {
		return s.finishJob(id, "failed", -1, e.Error())
	}
	log, e := os.OpenFile(s.jobPath(id, "events.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return s.finishJob(id, "failed", -1, e.Error())
	}
	defer log.Close()
	cmd.Dir = work
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	exe, _ := executable()
	cmd.Env = childEnv(map[string]string{"MESH_HOME": s.Root, "MESH_USER_HOME": s.UserHome, "MESH_JOB_ID": id, "MESH_OUTPUT_DIR": filepath.Join(work, "outputs"), "MESH_INPUT_DIR": filepath.Join(work, "inputs"), "PATH": filepath.Dir(exe) + string(os.PathListSeparator) + os.Getenv("PATH")})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	if e := cmd.Start(); e != nil {
		return s.finishJob(id, "failed", -1, e.Error())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cancelled := false
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var runErr error
	running := true
	for running {
		select {
		case runErr = <-done:
			running = false
		case <-sig:
			cancelled = true
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		case <-ticker.C:
			if _, err := os.Stat(s.jobPath(id, "cancel")); err == nil {
				cancelled = true
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			}
		}
		if cancelled && running {
			select {
			case runErr = <-done:
				running = false
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				runErr = <-done
				running = false
			}
		}
	}
	// Terminate leftover children: long-lived work must be another Mesh job.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if cancelled {
		return s.finishJob(id, "cancelled", -1, "cancelled")
	}
	message, errorCode := "", ""
	if j.Task.Agent != "shell" {
		message, errorCode = providerFailure(s.jobPath(id, "events.log"))
	}
	if message != "" {
		state := "failed"
		if errorCode == "authentication_required" || errorCode == "permission_required" || errorCode == "usage_limit" {
			state = "needs_attention"
		}
		code := 0
		if runErr != nil {
			code = -1
			var x *exec.ExitError
			if errors.As(runErr, &x) {
				code = x.ExitCode()
			}
		}
		return s.editJob(id, func(job *Job) error {
			job.State, job.Error, job.ErrorCode, job.ExitCode, job.FinishedAt, job.PID = state, message, errorCode, code, now(), 0
			return nil
		})
	}
	if runErr != nil {
		code := -1
		var x *exec.ExitError
		if errors.As(runErr, &x) {
			code = x.ExitCode()
		}
		return s.finishJob(id, "failed", code, runErr.Error())
	}
	files, e := outputFiles(filepath.Join(work, "outputs"))
	if e != nil {
		return s.finishJob(id, "failed", -1, "invalid output bundle: "+e.Error())
	}
	for _, expected := range j.Task.RequiredOutputs {
		found := false
		for _, file := range files {
			if file.Path == expected {
				found = true
			}
		}
		if !found {
			return s.finishJob(id, "failed", 0, "required output was not produced: "+expected)
		}
	}
	return s.finishJob(id, "completed", 0, "")
}

// Read provider-declared failures rather than making the user decode a raw
// JSON stream. In particular, a Claude result may have subtype=success while
// is_error=true. Successful assistant prose is never treated as an error.
func providerFailure(path string) (string, string) {
	f, e := os.Open(path)
	if e != nil {
		return "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	message := ""
	for scanner.Scan() {
		var event struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			PermissionDenials []json.RawMessage `json:"permission_denials"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event.Type {
		case "turn.completed":
			message = ""
		case "turn.failed":
			message = event.Error.Message
		case "error":
			message = event.Message
		case "result":
			if event.IsError {
				message = event.Result
			} else {
				message = ""
			}
			if len(event.PermissionDenials) > 0 {
				message = "Agent tools were denied permission. Use an interactive Mesh session to approve the required actions, or configure the provider's normal permissions for this task."
			}
		}
	}
	if message == "" {
		return "", ""
	}
	if len(message) > 8192 {
		message = message[:8192]
	}
	lower := strings.ToLower(message)
	code := "agent_failed"
	switch {
	case strings.Contains(lower, "authenticat"), strings.Contains(lower, "oauth"), strings.Contains(lower, "unauthorized"):
		code = "authentication_required"
	case strings.Contains(lower, "permission"), strings.Contains(lower, "denied"), strings.Contains(lower, "sandbox"):
		code = "permission_required"
	case strings.Contains(lower, "usage limit"), strings.Contains(lower, "rate limit"), strings.Contains(lower, "quota"):
		code = "usage_limit"
	}
	return message, code
}

func (s *Store) finishJob(id, state string, code int, message string) error {
	return s.editJob(id, func(j *Job) error {
		j.State = state
		j.ExitCode = code
		j.Error = message
		j.FinishedAt = now()
		j.PID = 0
		return nil
	})
}
func (s *Store) Cancel(id string) error {
	return s.editJob(id, func(j *Job) error {
		if terminal(j.State) {
			return nil
		}
		if j.State == "queued" {
			j.State = "cancelled"
			j.FinishedAt = now()
			return nil
		}
		return atomicWrite(s.jobPath(id, "cancel"), []byte("cancel\n"), 0600)
	})
}
func (s *Store) LocalJobs() ([]Job, error) {
	entries, e := os.ReadDir(s.path("jobs"))
	if e != nil {
		return nil, e
	}
	out := []Job{}
	for _, x := range entries {
		if !x.IsDir() {
			continue
		}
		j, e := s.Job(x.Name())
		if os.IsNotExist(e) {
			// A rejected or interrupted submission may have created its workspace
			// before committing job.json. It is not a scheduled job.
			continue
		}
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, nil
}
func (s *Store) Logs(id string, offset int64) (LogChunk, error) {
	j, e := s.Job(id)
	if e != nil {
		return LogChunk{}, e
	}
	if offset < 0 {
		return LogChunk{}, errors.New("negative log offset")
	}
	chunk := LogChunk{Offset: offset, State: j.State}
	f, e := os.Open(s.jobPath(id, "events.log"))
	if os.IsNotExist(e) {
		return chunk, nil
	}
	if e != nil {
		return chunk, e
	}
	defer f.Close()
	if _, e = f.Seek(offset, io.SeekStart); e != nil {
		return chunk, e
	}
	b, e := io.ReadAll(io.LimitReader(f, 64<<10))
	chunk.Text = string(b)
	chunk.Offset += int64(len(b))
	return chunk, e
}

func (s *Store) Submit(target string, t Task, output string) (Receipt, error) {
	c, e := s.Config()
	if e != nil {
		return Receipt{}, e
	}
	peer, local, e := c.Resolve(target)
	if e != nil {
		return Receipt{}, e
	}
	if !local && !peer.Incoming {
		return Receipt{}, errors.New("destination does not accept incoming Mesh tasks")
	}
	if t.ID == "" {
		t.ID = newID()
	}
	t.OriginID = c.Self.ID
	t.RootID = t.ID
	parentID := os.Getenv("MESH_JOB_ID")
	if parentID != "" {
		parent, e := s.Job(parentID)
		if e != nil {
			return Receipt{}, fmt.Errorf("load parent policy: %w", e)
		}
		t.ParentID = parentID
		t.RootID = parent.Task.RootID
		t.Depth = parent.Task.Depth + 1
		if t.MaxDepth > parent.Task.MaxDepth {
			t.MaxDepth = parent.Task.MaxDepth
		}
		if t.MaxChildren > parent.Task.MaxChildren {
			t.MaxChildren = parent.Task.MaxChildren
		}
		if len(parent.Task.Allowed) > 0 {
			if len(t.Allowed) == 0 {
				t.Allowed = parent.Task.Allowed
			} else {
				for _, id := range t.Allowed {
					found := false
					for _, allowed := range parent.Task.Allowed {
						if id == allowed {
							found = true
						}
					}
					if !found {
						return Receipt{}, errors.New("child cannot broaden parent placement")
					}
				}
			}
		}
	}
	if e := validateTask(t, peer.ID); e != nil {
		return Receipt{}, e
	}
	if output == "" {
		output = filepath.Join("results", t.ID)
	}
	output, e = filepath.Abs(output)
	if e != nil {
		return Receipt{}, e
	}
	r := Receipt{ID: t.ID, TargetID: peer.ID, Output: output, Delivery: "pending", LastState: "submitting", CreatedAt: now()}
	if parentID != "" {
		e = s.editJob(parentID, func(p *Job) error {
			if p.State != "running" {
				return errors.New("parent is not running")
			}
			if len(p.Children) >= p.Task.MaxChildren {
				return errors.New("parent child-task limit reached")
			}
			p.Children = append(p.Children, t.ID)
			return nil
		})
		if e != nil {
			return r, e
		}
	}
	if e := writeJSON(s.path("receipts", t.ID+".request.json"), t); e != nil {
		return r, e
	}
	if e := writeJSON(s.path("receipts", t.ID+".json"), r); e != nil {
		return r, e
	}
	var j Job
	if local {
		j, e = s.Accept(t)
	} else {
		e = s.Call(peer, Request{Action: "submit", Task: &t}, &j)
	}
	if e != nil {
		r.LastError = e.Error()
	} else {
		r.LastState = j.State
	}
	_ = writeJSON(s.path("receipts", t.ID+".json"), r)
	if e != nil {
		return r, fmt.Errorf("submission %s saved; retry with mesh retry %s: %w", t.ID, t.ID, e)
	}
	return r, nil
}

func (s *Store) Receipt(id string) (Receipt, error) {
	var r Receipt
	if !validID.MatchString(id) {
		return r, errors.New("invalid job ID")
	}
	e := readJSON(s.path("receipts", id+".json"), &r)
	return r, e
}
func (s *Store) targetForJob(id string) (Peer, bool, error) {
	c, e := s.Config()
	if e != nil {
		return Peer{}, false, e
	}
	r, e := s.Receipt(id)
	if os.IsNotExist(e) {
		if _, e = s.Job(id); e == nil {
			return c.Self, true, nil
		}
	}
	if e != nil {
		return Peer{}, false, e
	}
	return c.Resolve(r.TargetID)
}
func (s *Store) Status(id string) (Job, error) {
	p, local, e := s.targetForJob(id)
	if e != nil {
		return Job{}, e
	}
	if local {
		return s.Job(id)
	}
	var j Job
	e = s.Call(p, Request{Action: "status", ID: id}, &j)
	return j, e
}
func (s *Store) TaskLogs(id string, offset int64) (LogChunk, error) {
	p, local, e := s.targetForJob(id)
	if e != nil {
		return LogChunk{}, e
	}
	if local {
		return s.Logs(id, offset)
	}
	var l LogChunk
	e = s.Call(p, Request{Action: "logs", ID: id, Offset: offset}, &l)
	return l, e
}
func (s *Store) CancelTask(id string) error {
	p, local, e := s.targetForJob(id)
	if e != nil {
		return e
	}
	if local {
		return s.Cancel(id)
	}
	return s.Call(p, Request{Action: "cancel", ID: id}, nil)
}
func (s *Store) Retry(id string) error {
	r, e := s.Receipt(id)
	if e != nil {
		return e
	}
	var t Task
	if e := readJSON(s.path("receipts", id+".request.json"), &t); e != nil {
		return e
	}
	p, local, e := s.targetForJob(id)
	if e != nil {
		return e
	}
	var j Job
	if local {
		j, e = s.Accept(t)
	} else {
		e = s.Call(p, Request{Action: "submit", Task: &t}, &j)
	}
	if e != nil {
		return e
	}
	r.LastState = j.State
	r.LastError = ""
	return writeJSON(s.path("receipts", id+".json"), r)
}

func (s *Store) Collect(id, output string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid job ID")
	}
	var result string
	e := withLock(s.path("receipts", id+".lock"), func() error {
		var err error
		result, err = s.collect(id, output)
		return err
	})
	return result, e
}

func (s *Store) collect(id, output string) (string, error) {
	r, e := s.Receipt(id)
	if e != nil {
		return "", e
	}
	if output != "" {
		r.Output, e = filepath.Abs(output)
		if e != nil {
			return "", e
		}
		r.Delivery = "pending"
		if e := writeJSON(s.path("receipts", id+".json"), r); e != nil {
			return "", e
		}
	}
	j, e := s.Status(id)
	if e != nil {
		return "", e
	}
	if !terminal(j.State) {
		return "", fmt.Errorf("job is %s", j.State)
	}
	p, local, e := s.targetForJob(id)
	if e != nil {
		return "", e
	}
	var files []File
	if local {
		files, e = outputFiles(s.jobPath(id, "work", "outputs"))
	} else {
		e = s.Call(p, Request{Action: "artifacts", ID: id}, &files)
	}
	if e != nil {
		return "", e
	}
	if e := materialize(r.Output, files); e != nil {
		return "", e
	}
	if e := os.MkdirAll(r.Output, 0700); e != nil {
		return "", e
	}
	// Acknowledge only after every checksum was verified and every file written.
	if local {
		e = s.editJob(id, func(j *Job) error { j.Delivery = "delivered"; return nil })
	} else {
		e = s.Call(p, Request{Action: "ack", ID: id}, nil)
	}
	if e != nil {
		return r.Output, fmt.Errorf("files saved, delivery acknowledgment pending: %w", e)
	}
	r.Delivery = "delivered"
	r.LastState = j.State
	r.LastError = ""
	if e := writeJSON(s.path("receipts", id+".json"), r); e != nil {
		return r.Output, e
	}
	return r.Output, nil
}

func (s *Store) Wait(id string, stream bool) error {
	var offset int64
	for {
		if stream {
			l, e := s.TaskLogs(id, offset)
			if e != nil {
				return e
			}
			fmt.Print(l.Text)
			offset = l.Offset
			if len(l.Text) > 0 {
				continue
			}
		}
		j, e := s.Status(id)
		if e != nil {
			return e
		}
		if terminal(j.State) {
			if stream {
				l, e := s.TaskLogs(id, offset)
				if e != nil {
					return e
				}
				fmt.Print(l.Text)
				if len(l.Text) > 0 {
					offset = l.Offset
					continue
				}
			}
			if j.State != "completed" {
				return fmt.Errorf("job %s: %s (exit %s)", j.State, j.Error, strconv.Itoa(j.ExitCode))
			}
			return nil
		}
		time.Sleep(time.Second)
	}
}
