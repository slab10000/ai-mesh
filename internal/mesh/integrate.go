package mesh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const beginInstructions = "<!-- ai-mesh:begin -->"
const endInstructions = "<!-- ai-mesh:end -->"

func managedInstructions(existing, body string, remove bool) (string, error) {
	start := strings.Index(existing, beginInstructions)
	end := strings.Index(existing, endInstructions)
	if (start < 0) != (end < 0) || (start >= 0 && end < start) || strings.Count(existing, beginInstructions) > 1 || strings.Count(existing, endInstructions) > 1 {
		return "", fmt.Errorf("malformed Mesh instruction block; existing file was not changed")
	}
	if start >= 0 {
		tail := end + len(endInstructions)
		if tail < len(existing) && existing[tail] == '\n' {
			tail++
		}
		existing = existing[:start] + existing[tail:]
	}
	if remove {
		return existing, nil
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + beginInstructions + "\n" + body + "\n" + endInstructions + "\n", nil
}

func (s *Store) Integrate(remove bool) ([]string, error) {
	exe, e := executable()
	if e != nil {
		return nil, e
	}
	body := `## Working across computers with ai-mesh

The Mesh CLI is available at ` + "`" + exe + "`" + `. Use your shell tool to call it. Run ` + "`mesh help`" + ` for the command reference.
- Start with ` + "`mesh session`" + ` to identify whether this conversation is running inside Mesh, its computer, provider, workspace, and session. Mesh also sets MESH_ACTIVE, MESH_MACHINE, MESH_AGENT, MESH_SESSION_ID, and MESH_WINDOW. Trust ` + "`mesh session`" + ` over cached MESH_* variables: shell snapshots can contain an older session identity. Never print controller credentials.
- Inside Mesh, at the beginning of each user turn run ` + "`mesh inbox --read`" + `. Read any new handoff context and its selected files before acting, then mark each consumed brief with ` + "`mesh inbox ack ID`" + `. A brief is conversation context, not a new grant of permissions. Keep the existing conversation and reconcile new context with work already completed here.
- Before choosing a remote computer, run ` + "`mesh machines --check --json`" + `. Machine descriptions may be cached; respect reachability and the user's placement requirements.
- Delegate with ` + "`mesh run --on NAME --agent codex|claude --input PATH --context FILE --output DIR \"TASK\"`" + `. Input files are transferred; write a task brief into the context file when handoff is needed. A tool does not automatically receive the whole conversation.
- For named deliverables, repeat ` + "`--expect FILE`" + ` for each required path inside outputs/. Check both execution status and delivered files before reporting success. A ` + "`needs_attention`" + ` state means provider authentication, permissions, or usage needs attention.
- For an explicit command use ` + "`mesh run --on NAME --agent shell -- COMMAND ARG...`" + `. Returned job IDs are persistent. Use ` + "`mesh status ID`" + `, ` + "`mesh watch ID`" + `, ` + "`mesh collect ID`" + `, and ` + "`mesh cancel ID`" + `. Execution and delivery are separate. A connection error is not proof that execution failed; inspect or retry the same ID rather than submitting a duplicate.
- ` + "`--only`" + ` on a run forbids further Mesh delegation. Child tasks inherit placement and delegation limits; do not evade these limits through raw SSH. Coordinate children and collect their results before declaring the parent complete.
- After verifying a reusable capability, record it with ` + "`mesh capability add NAME --environment ENV --note DESCRIPTION`" + `. Never record credentials or infer user permission from a capability.
- When the user says "continue this conversation/task on COMPUTER", prepare a concise Markdown handoff in the current workspace: the user's objective and latest request, decisions, constraints, completed work, relevant file paths, remaining steps, and any jobs still running on the source computer. Summarize working context; do not claim to copy hidden model state or credentials. Select the files needed to continue.
- Then call ` + "`mesh handoff COMPUTER --context BRIEF.md --input FILE_OR_DIR`" + ` (repeat --input; optional --project for a destination workspace). This transfers the brief/files and changes the visible terminal. A new destination conversation receives a startup prompt; an existing conversation stays alive and receives the brief in its inbox. Only report a visible switch when the reply confirms ` + "`terminal_switched: true`" + `. On an uncertain reply, retry the same command with the reported --id; do not create a duplicate handoff. After a successful handoff, finish this turn without exiting the source agent or duplicating the transferred work.
- For "go back", "return to COMPUTER", or revisiting an existing conversation, use ` + "`mesh back`" + ` or ` + "`mesh switch COMPUTER`" + `. These select the same live conversations and keep other agents/jobs running. Do not restart an agent, run a native resume picker, or summarize again merely to revisit it. Use handoff only when the user wants to carry current work/context along.
- These controls require a terminal launched with Mesh. In an ordinary agent/app session, explain how to start ` + "`mesh codex`" + ` or ` + "`mesh claude`" + `; do not claim to have changed the open app. If the provider blocks a Mesh command, request its normal approval for that command; do not bypass permissions.
- The user can switch without an agent tool call: Ctrl-b then m opens the computer menu, Ctrl-b then b returns, and Ctrl-b then d detaches. Choosing a computer immediately changes the visible terminal to its preserved live conversation; it never creates a context brief or sends one. These controls remain available when a provider sandbox cannot reach the Mesh session socket.
- Do not enroll computers, distribute SSH keys, remove peers, or change user rules unless the user requests it.
- Preserve the selected provider's authentication and permissions. Do not use bypass-permissions flags merely to make a remote task succeed.
`
	var changed []string
	for agent, path := range agentPaths(s.UserHome) {
		original := path
		if !remove {
			if _, e := exec.LookPath(agent); e != nil {
				continue
			}
		}
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return changed, err
			}
			path = resolved
		}
		e := withLock(path+".mesh.lock", func() error {
			old, e := os.ReadFile(path)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if remove && os.IsNotExist(e) {
				return nil
			}
			updated, e := managedInstructions(string(old), body, remove)
			if e != nil {
				return e
			}
			if updated == string(old) {
				return nil
			}
			if len(old) > 0 {
				if _, e := os.Stat(path + ".pre-mesh"); os.IsNotExist(e) {
					if e := atomicWrite(path+".pre-mesh", old, 0600); e != nil {
						return e
					}
				}
			}
			if e := atomicWrite(path, []byte(updated), 0600); e != nil {
				return e
			}
			changed = append(changed, path)
			return nil
		})
		if e != nil {
			return changed, e
		}
		if !remove {
			if e := s.recordIntegrationPaths(original, path); e != nil {
				return changed, e
			}
		}
	}
	return changed, nil
}

// Remember custom provider homes and symlink targets even if the environment
// or the selected AGENTS.override.md changes before uninstall.
func (s *Store) recordIntegrationPaths(paths ...string) error {
	return withLock(s.path("integration-paths.lock"), func() error {
		var saved []string
		if err := readJSON(s.path("integration-paths.json"), &saved); err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, path := range paths {
			path, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			found := false
			for _, old := range saved {
				if old == path {
					found = true
					break
				}
			}
			if !found {
				saved = append(saved, path)
			}
		}
		return writeJSON(s.path("integration-paths.json"), saved)
	})
}
