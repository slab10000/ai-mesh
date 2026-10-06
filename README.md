# ai-mesh

Use your existing AI agents across the computers you can access through SSH. Keep the conversation on your laptop, send work and files to another computer, follow its progress, and retrieve the results.

This is the first working CLI implementation of the vision in [PROJECT.md](PROJECT.md). The repository remains private. It uses a single Go executable, OpenSSH, ordinary files, and tmux for interactive sessions. There are no Go library dependencies, hosted Mesh account, or MCP server.

## What works

- Discover candidate computers from Tailscale, SSH aliases, and LAN neighbor tables, before Mesh is installed there.
- Enroll a selected SSH account, install the appropriate executable, and exchange public keys. Optionally configure all selected peers for mutual access.
- Keep incoming access optional. An outgoing-only laptop can submit tasks and retrieve results without accepting SSH connections.
- Gather a small initial inventory, record capabilities as agents learn about them, and synchronize each computer's own versioned description.
- Submit persistent Codex, Claude Code, or command jobs with inputs and a context brief. Follow raw traces, inspect status, cancel, retry an uncertain submission, and collect checked output files.
- Track execution and delivery separately. The local service retrieves finished results when the destination becomes reachable again.
- Delegate child jobs with inherited placement, depth, and child-count limits. Each machine has a local execution limit.
- Launch native agent terminals through Mesh, switch computers in the same terminal, and return to a preserved session.
- Install removable instructions for detected Codex, Claude Code, and Gemini installations. Gemini and OpenCode also have interactive launchers; their background task adapters are not implemented.

No computer has a permanent coordinator role. A submitting computer retains its own job receipts; other peers can work together without it after mutual enrollment has completed.

## Install from this checkout

Requirements:

- macOS or Linux, on arm64 or amd64.
- Go 1.24 or newer on the computer building Mesh. Destination computers receive a compiled binary and do not need Go.
- OpenSSH client and `ssh-keygen`. Each incoming destination needs an already working SSH server and an account you may configure.
- tmux on both computers for interactive sessions. Background jobs do not require tmux.
- The desired agents installed and authenticated on their respective computers.

```sh
sh scripts/install.sh
```

This builds the native executable and all four supported destination binaries, installs them into `~/.local/bin`, and starts `mesh setup`. Add that directory to your shell's PATH if necessary:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

The wizard asks whether this computer accepts incoming work and whether to install and configure mutual access on selected computers. It detects installed agents, adds Mesh instructions, installs a user service, and offers discovered SSH destinations. OpenSSH handles passwords, MFA, and initial host-key confirmation directly. Mesh does not store those passwords.

The selected local account is the account running setup. To enroll a different local account, run setup while logged into that account. Mesh does not enable SSH, change system-wide authentication, install agent products, or copy provider credentials.

To inspect the commands before changing any machine:

```sh
make build
./bin/mesh help
./bin/mesh doctor
```

For staged installation, use `sh scripts/install.sh --no-setup`, then run the individual commands below. The [manual testing guide](docs/TESTING.md) walks through green-lighthouse step by step.

## Enroll computers

Initialize this account. Choose an address reachable from the other computers if incoming access is enabled:

```sh
mesh init --name mac --address my-mac --incoming=false
mesh integrate
mesh service install

mesh discover
mesh discover --lan
mesh discover --probe --json
mesh enroll homelab --user alice --name homelab --mutual
```

Discovery does not grant access. Tailscale supplies connectivity and candidate addresses; membership does not require the same owner or Tailscale identity. Existing SSH access must authorize the account and the changes being made.

`mesh enroll` installs `~/.local/bin/mesh` in the destination account. By default it also integrates detected agents and installs the destination's user service. `--integrate=false` and `--service=false` disable those steps. `--binary FILE` selects a destination executable explicitly; otherwise Mesh finds the matching binary installed beside itself or under `dist/`.

Without `--mutual`, enrollment connects the originating account and the selected account. With `--mutual`, it distributes the roster and public keys among the enrolled accounts that accept incoming access. Each computer keeps its own private key.

SSH aliases are convenient for direct enrollment. For mutual access, use a hostname or Tailscale address that every participating computer can resolve; an alias defined only on the Mac is not automatically available on the server.

```sh
mesh machines --check --json
mesh machine show homelab
mesh access --incoming=true
mesh peers reconcile
mesh peers remove homelab
```

`access` updates this account's incoming policy and notifies existing peers. `peers reconcile` explicitly grants mutual access among the currently enrolled peers. It is not just a read-only synchronization command.

Membership changes that cannot reach a peer remain queued for the initiating computer's service to retry. Revocation takes effect on each destination when that destination processes it; it does not terminate existing SSH sessions or running jobs. If the reply to a successful revocation is lost, the queue may require inspection because the removed key can no longer reconnect to confirm it.

Mesh SSH keys grant access to the selected account, including its shell. This is a group of trusted accounts, not an isolation boundary between hostile agents. Existing unrelated authorized keys are preserved. Host-key verification stays enabled.

## Delegate work

A command job is useful for the first connectivity check:

```sh
mesh run --on homelab --agent shell --output ./results/hello --wait -- \
  sh -c 'hostname > "$MESH_OUTPUT_DIR/host.txt"'
```

An agent job can receive files and a task brief:

```sh
mesh run --on homelab --agent codex \
  --input ./instructions.md \
  --context ./handoff.md \
  --output ./results/report \
  --only --wait \
  'Read inputs/instructions.md and create the requested PDF in outputs/.'
```

Use `--agent claude` for Claude Code. The destination uses its normal local provider authentication. Mesh uses each provider's noninteractive CLI and does not promise that a particular subscription covers every workflow.

Inputs arrive under `inputs/` inside a new task workspace. An input directory keeps its basename; `--input ./documents` becomes `inputs/documents/`. Repeat `--input` for more inputs. Deliverable files belong in `outputs/`. Command jobs also receive `MESH_INPUT_DIR`, `MESH_OUTPUT_DIR`, and `MESH_JOB_ID`.

Without `--wait`, submission prints a JSON receipt and returns immediately. Keep its `id`:

```sh
mesh status JOB_ID
mesh watch JOB_ID
mesh collect JOB_ID
mesh cancel JOB_ID
mesh jobs
```

`watch` follows available raw output and exits when execution finishes. `collect` retrieves the results. `--wait` does both after a successful job. Failed and cancelled jobs can still have useful artifacts; collect them explicitly or let the service collect their outputs.

Execution states are `queued`, `running`, `completed`, `failed`, and `cancelled`. Delivery is separately `pending` or `delivered`. A job completed on an offline peer remains awaiting delivery. The service collects automatically into the output directory recorded at submission; without the service, use `mesh collect` or `mesh daemon --once`.

After an uncertain SSH submission, the error includes the saved job ID:

```sh
mesh retry JOB_ID
```

Retry resends the same task identity. The destination returns the original job instead of starting it again. A failed execution needs a new submission if you want to run it again.

Artifacts are checked with SHA-256. Repeated delivery of identical bytes is allowed; different existing files are never silently overwritten. Each input or output bundle is limited to **32 MiB**. Symlinks and special files are not transferred. Keep large datasets/model weights on their destination and record their locations as capabilities.

### Delegation and permissions

An agent or script running in a Mesh job can call `mesh run` again. Children record their parent and root job IDs and inherit placement/depth limits. The parent should wait for and collect its children before assembling its own result.

- Default maximum delegation depth: 2. A root has depth 0.
- Default maximum direct children per task: 4.
- Default simultaneous executing jobs per computer: 2; set `--max-jobs` during initial `mesh init` to change it.
- `--max-depth` and `--max-children` can reduce the limits for a submission.
- `--only` prevents further Mesh delegation and restricts this task to the named destination.
- Cancellation targets one job. It does not recursively cancel children.

These limits are enforced by the Mesh CLI and scheduler. Agents must not evade them through raw SSH. Avoid filling every local slot with parents that wait for children queued on the same computer; use another destination or allow enough local capacity.

Agent permissions still apply. Codex jobs use `workspace-write`; Claude jobs use `acceptEdits`. Mesh does not turn off provider safeguards. A headless agent may be unable to request interactive approval, access the network, or write Mesh state outside its workspace. Configure its normal permissions for the workflow, or use an interactive Mesh session to handle approvals. The shell transport and nested-job lifecycle are tested; permission behavior with your real provider setup needs your test.

## Native terminal sessions

```sh
mesh codex
mesh claude homelab --project '~/work/project'
mesh connect
mesh codex homelab --resume
mesh sessions
```

Quote a destination path containing a tilde so your local shell does not expand it first: `--project '~/work/project'`. Relative project paths resolve under the destination account's home directory. The directory must already exist.

Inside an agent launched through Mesh, ask it to run:

```sh
mesh switch homelab
mesh switch homelab --agent claude --project '~/work/project'
mesh back
```

The current terminal session is preserved in tmux. Mesh switches the active window, keeping a machine/agent label visible. `Ctrl-b d` detaches the local terminal without stopping the sessions. `--resume` reattaches a matching live Mesh session; if none exists, Codex/Claude use their native resume picker.

Switching starts or returns to a destination session. It does **not** migrate the previous conversation. Use `--context` and selected files for an explicit handoff. Agent-specific `/SelectComputer` slash commands are not installed in this version; `mesh connect` provides the terminal picker and the installed instructions support natural-language switching.

Remote switch requests return through a Unix socket forwarded over the existing SSH connection. This needs SSH stream-local forwarding enabled on the destination. It works even when the originating laptop declines incoming SSH access. Interactive tmux sessions survive ordinary client disconnection; a host reboot requires native agent resume rather than restoring a running process.

## Shared machine descriptions

```sh
mesh machine refresh
mesh capability add pdf --environment '~/venvs/reports' \
  --note 'ReportLab is installed here; used successfully to create a PDF.'
mesh capability add dataset-images --environment /srv/data/images \
  --note 'Existing training dataset; inspect before reuse.'
mesh capability remove pdf --environment '~/venvs/reports'
mesh sync
```

Initial collection records OS, architecture, CPU, RAM, NVIDIA GPU information when available, and common installed tools. It does not scan your datasets, read provider credentials, run model inference, or continually benchmark the machine. Unknown facts remain unknown. Provider login readiness, non-NVIDIA GPU details, and automatic battery/training rules are not implemented yet.

`machines --check --json` adds a live connection check, Mesh workload counts, concurrency capacity, free space on the Mesh filesystem, and last contact. Plain `machines` uses cached descriptions. A recorded capability is an observation, not permission to use the resource.

Each computer writes only its own inventory file. Concurrent local updates are locked and revisions prevent older peer copies from replacing newer ones. The service exchanges inventories every 30 seconds; `mesh sync` requests an immediate exchange. There is no LLM merge worker. Membership, credentials, and user permissions are not inferred from inventory notes.

Different capability keys are preserved independently. For the same name and environment, the last completed local update replaces the earlier note.

## Service and stored data

```sh
mesh service install
mesh service status
mesh daemon --once
mesh service uninstall
mesh integrate --remove
```

The macOS service is a user LaunchAgent; loading it needs an active GUI login for that account. Linux uses `systemd --user`; an active user manager is required. Persistent operation after logout can require administrator-configured lingering. Mesh reports service installation failures instead of claiming success. On a system without a user service manager, run `mesh daemon` under your existing supervisor.

State lives under `~/.ai-mesh`:

| Location | Contents |
| --- | --- |
| `config.json`, `keys/`, `known_hosts` | Local identity, peers, this machine's private key, peer host keys |
| `machines/`, `contacts/` | Versioned descriptions and last successful contact |
| `jobs/ID/` | Task state, workspace, inputs, outputs, and raw logs |
| `receipts/` | Outgoing tasks, original requests for retry, and local delivery destinations |
| `pending/` | Authorized membership changes awaiting confirmation |
| `sessions/`, `logs/` | Terminal metadata and service/controller logs |

Private keys remain local. Task briefs and files are copied only as part of the requested handoff. Raw logs and receipts may contain task content; this version does not implement automatic retention or cleanup. Finished results remain on the executing computer after delivery.

Agent integration appends a marked block to the active global instruction file (`AGENTS.md`/existing `AGENTS.override.md`, `CLAUDE.md`, or `GEMINI.md`) and saves a `.pre-mesh` backup when that file already has content. `integrate --remove` removes Mesh's block while keeping surrounding instructions. Restart an already-running agent so it reads updated instructions.

`MESH_HOME` selects an alternative state directory. `MESH_USER_HOME` is intended for isolated development fixtures. Remote enrollment uses the standard `~/.ai-mesh` and `~/.local/bin/mesh` locations. Do not clone a state directory between computers: each installation needs its own identity and private key.

## Development and current boundaries

```sh
make check       # go vet and race-enabled tests
make dist        # macOS/Linux, arm64/amd64 executables
```

Tests use temporary accounts, fake SSH endpoints, fake agents, and an isolated tmux server. Python 3 is needed by the SSH fixture. The tmux test skips when tmux is absent. No tests contact a real remote machine, install a service, alter real provider configuration, or make an inference call.

The implementation covers the initial transport and CLI workflow. Full desktop integration, agent-specific slash commands, forwarded headless questions/approvals, automatic workload placement, broad memory replication, and exact cross-provider conversation migration remain outside this version. LAN discovery is best effort from neighbor caches or an explicit IPv4 `/24`-or-smaller port scan; mDNS advertising/browsing and Windows support are not included.

Jobs are durable across ordinary client disconnects, not arbitrary host shutdown or administrator session cleanup. If an executing worker disappears, recovery marks it failed and does not automatically rerun it. There is no distributed transaction or process checkpointing layer.
