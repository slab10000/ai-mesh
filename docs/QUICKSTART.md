# ai-mesh: short usage guide

Your Mac (`macbook`) and `blas@green-lighthouse` are enrolled and can both submit work to each other. Both have the Mesh background service. Codex has been tested successfully in both directions. No Claude installation or login is needed for these workflows.

## Switch computers in your terminal

```sh
export PATH="$HOME/.local/bin:$PATH"
mesh codex
```

Inside that terminal:

| Keys | Result |
| --- | --- |
| Ctrl-b, release, then **m** | Show computers; choose one by number |
| Ctrl-b, release, then **b** | Return to your previous session |
| Ctrl-b, release, then **d** | Detach while keeping sessions alive |

The status bar names the computer and agent you are using. Choosing a computer immediately shows its live agent chat in the **same terminal**, with no SSH commands or second terminal to open. Once visited, Mac → server → Mac → server returns to the same conversations and running processes. The menu never summarizes or sends context. The first visit starts a conversation if this Mesh group does not already have one there.

These work too:

```sh
mesh codex green-lighthouse --project /home/blas/ai-mesh-e2e/project
mesh codex --resume
mesh shell                   # Plain terminal switching without an AI provider
mesh connect                 # Initial computer/agent picker
mesh sessions                # Saved groups
```

An agent can run `mesh switch green-lighthouse` or `mesh back`. Its normal permissions still apply: remote Codex required permission to access the control socket during our test. The keyboard controls work directly through Mesh. After an SSH drop, select the destination again to reconnect its preserved remote session. After detaching, use the same agent/project with `--resume`.

## Continue current work on another computer

Context transfer is a separate action. Inside `mesh codex`, say:

> Continue this task on green-lighthouse, taking the relevant context and files with you.

Installed agent instructions explain how to check reachable computers, write a concise brief of the objective, decisions, constraints, completed work, and next steps, then run:

```sh
mesh handoff green-lighthouse --context BRIEF.md --input report-notes.txt
```

The brief and selected files arrive in `.mesh/handoffs/ID/` inside the destination workspace. An existing conversation keeps its workspace; a new one defaults to `~/.ai-mesh/projects/SESSION_ID`. `--project DIR` chooses a different workspace; `--input` can be repeated. Existing project files are not replaced.

Mesh changes the visible terminal automatically and leaves the source agent alive. A new destination agent starts with a prompt to read its inbox and continue. An already-running destination keeps its conversation and reads new context **at its next user turn**; Mesh does not interrupt it or type into a busy prompt. Use the menu or `mesh back` to revisit either conversation without another transfer.

A successful handoff reports `terminal_switched: true`: Mesh has verified the destination agent, SSH attachment, and visible terminal window. A transfer without a working terminal switch is reported as unconfirmed, with the handoff ID for retry. Cached `MESH_*` shell variables can be stale; `mesh session` resolves the explicit conversation binding, with native process ancestry as a fallback.

Agents inspect their identity with `mesh session`, read pending context with `mesh inbox --read`, then acknowledge it with `mesh inbox ack ID`. Acknowledgment writes `READ.json` beside the brief, requiring only normal workspace write access. An uncertain handoff reports an ID: retry the same command with `--id ID` to avoid duplication. Handoffs copy explicit summaries and selected files, not hidden model state or provider credentials. Files produced during an interactive handoff stay in its workspace; use `mesh run` below when automatic return delivery is needed.

After upgrading, run `mesh integrate` on both computers and start a new Mesh terminal to load the new controller and instructions. Already-running conversations remain preserved, but older controllers do not acquire new commands in place. This feature controls the native CLI terminal; it does not replace a Codex desktop chat. Normal provider trust and approval prompts remain enabled. Process preservation requires the agent and its host to remain running; a reboot or explicitly exiting the agent is different from switching or detaching.

## Send a task and receive files here

From this repository on the Mac:

```sh
mesh machines --check
mesh run --on green-lighthouse --agent codex \
  --input examples/pdf-instructions.md \
  --context examples/handoff.md \
  --output ./results/pdf \
  --expect remote-report.pdf --expect generated-on.txt \
  --only --wait \
  'Read inputs/pdf-instructions.md and produce all requested outputs.'
```

Mesh copies the selected input and brief, starts Codex on the server using its existing login, streams its output, and retrieves the files. `--only` prevents further delegation. `--expect` prevents a missing deliverable from being reported as a completed task. The verified example PDF and its generator are in [evidence](evidence/).

Omit `--wait` to get a persistent job ID immediately:

```sh
mesh status JOB_ID
mesh watch JOB_ID
mesh collect JOB_ID
mesh cancel JOB_ID
```

Execution and delivery are separate. A job can be `completed` while delivery is `pending`. The Mac service retrieves its outputs when communication resumes. `mesh collect` requests delivery immediately. After an uncertain submission, use `mesh retry JOB_ID`; it reuses the identity and does not execute a duplicate task.

For a command without an AI provider:

```sh
mesh run --on green-lighthouse --agent shell --output ./results/host --wait -- \
  sh -c 'hostname > outputs/host.txt'
```

## How the pieces fit

Each account has its own Mesh identity, SSH key, job records, and machine description under `~/.ai-mesh`. OpenSSH carries commands, files, status, and logs. The service publishes descriptions once at enrollment and then when their file changes, and collects task results. It does not repeatedly exchange unchanged specs. Pending description updates are retried when a peer is reachable again. tmux preserves live terminal sessions. No permanent master computer or hosted Mesh account is involved.

Every machine writes its own description. Agents can record verified reusable capabilities with `mesh capability add`; updates propagate to peers. The computer running a task can submit child tasks to another enrolled computer, subject to placement/delegation limits. The agent or script coordinates these children; automatic GPU scheduling is not implemented.

The Mac accepts Mesh SSH on **its Tailscale IP, port 2222**, using a user LaunchAgent and enrolled Mesh keys only. System-wide Remote Login remains off. Inspect it with `mesh ssh-server status`; remove it with `mesh ssh-server uninstall`. The Mac and Mesh services run while your user login is available. On the server, systemd user lingering is currently off; survival across full logout/reboot is not promised.

## Repeating the tests

Run the opt-in real-machine suite from this checkout:

```sh
python3 scripts/e2e.py --on green-lighthouse --origin macbook
python3 scripts/e2e.py --on green-lighthouse --origin macbook --providers
```

The second command makes real Codex requests in both directions. Claude testing is separate and optional (`--claude`). The suite writes a dated JSON report under `artifacts/e2e/`. Services should be running, and the reverse-delegation test requires incoming access on the Mac. Background Codex startup on this Mac also required granting Full Disk Access to `/Users/blasmorenolaguna/.local/bin/mesh` in System Settings → Privacy & Security. This was granted and the reverse test passed. Native Codex permissions remain enabled. It uses generated test files and does not enroll or remove peers.

See [validation and screenshots](VALIDATION.md) for the actual results and remaining boundaries.
