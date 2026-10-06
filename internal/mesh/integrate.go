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
- Before choosing a remote computer, run ` + "`mesh machines --check --json`" + `. Machine descriptions may be cached; respect reachability and the user's placement requirements.
- Delegate with ` + "`mesh run --on NAME --agent codex|claude --input PATH --context FILE --output DIR \"TASK\"`" + `. Input files are transferred; write a task brief into the context file when handoff is needed. A tool does not automatically receive the whole conversation.
- For an explicit command use ` + "`mesh run --on NAME --agent shell -- COMMAND ARG...`" + `. Returned job IDs are persistent. Use ` + "`mesh status ID`" + `, ` + "`mesh watch ID`" + `, ` + "`mesh collect ID`" + `, and ` + "`mesh cancel ID`" + `. Execution and delivery are separate. A connection error is not proof that execution failed; inspect or retry the same ID rather than submitting a duplicate.
- ` + "`--only`" + ` on a run forbids further Mesh delegation. Child tasks inherit placement and delegation limits; do not evade these limits through raw SSH. Coordinate children and collect their results before declaring the parent complete.
- After verifying a reusable capability, record it with ` + "`mesh capability add NAME --environment ENV --note DESCRIPTION`" + `. Never record credentials or infer user permission from a capability.
- When launched through Mesh, a user request to switch computers should invoke ` + "`mesh switch NAME`" + `; returning uses ` + "`mesh back`" + `. These preserve the current session. Switching is not conversation migration. A context handoff requires an explicit brief or selected files.
- Do not enroll computers, distribute SSH keys, remove peers, or change user rules unless the user requests it.
- Preserve the selected provider's authentication and permissions. Do not use bypass-permissions flags merely to make a remote task succeed.
`
	var changed []string
	for agent, path := range agentPaths(s.UserHome) {
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
	}
	return changed, nil
}
