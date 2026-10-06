# Quickstart: your first remote job

This guide uses a local computer named `laptop` and an SSH destination named `homelab`, accessed as `alice`. Replace those names with your own. See the [README prerequisites](../README.md#prerequisites) for supported platforms and tools.

## 1. Install locally

On the computer that will submit work, with Git and Go 1.24+ installed:

```sh
git clone https://github.com/slab10000/ai-mesh.git
cd ai-mesh
sh scripts/install.sh --no-setup
export PATH="$HOME/.local/bin:$PATH"
mesh doctor
```

Add the PATH line to your shell startup file for future terminals. Missing configuration is expected before initialization. The installer includes compiled destination binaries; the remote computer does not need Go.

## 2. Connect one computer

First confirm your existing account can connect over SSH:

```sh
ssh alice@homelab 'uname -s'
```

Then initialize an outgoing-only laptop and enroll the destination:

```sh
mesh init --name laptop --incoming=false
mesh integrate
mesh service install
mesh enroll homelab --user alice --name homelab
mesh machines --check
```

The destination should be **reachable**. Enrollment installs Mesh, integrates detected agents, and installs the destination's user service. SSH handles initial authentication and host-key verification. Mesh keys grant access to the selected account; enroll computers and accounts you trust.

The laptop can retrieve results over outgoing SSH without accepting incoming connections. `init` keeps existing configuration if you have already set up this account. Prefer a guided flow? Use `mesh setup` instead of the initialization and enrollment commands above.

## 3. Return a file

```sh
mesh run --on homelab --agent shell \
  --output ./results/hello --expect host.txt --only --wait -- \
  sh -c 'hostname > "$MESH_OUTPUT_DIR/host.txt"'

cat ./results/hello/host.txt
```

The file should contain the destination's hostname. No AI provider or tmux is needed for this check. `--only` prevents further delegation; `--wait` follows execution and retrieves successful results. Use a fresh output directory if repeating a task will produce different bytes.

## 4. Try an agent job

With Codex authenticated and Python 3 installed on the destination:

```sh
mesh run --on homelab --agent codex \
  --input examples/pdf-instructions.md \
  --context examples/handoff.md \
  --output ./results/pdf \
  --expect remote-report.pdf --expect generate_pdf.py \
  --expect generated-on.txt --only --wait \
  'Read inputs/pdf-instructions.md and produce all requested outputs.'
```

Open `results/pdf/remote-report.pdf`. Its generator and hostname evidence should be beside it. This uses the destination's provider login and normal permissions. Use `--agent claude` to select an authenticated Claude Code installation instead.

## 5. Visit a live conversation

Install tmux and the selected agent on both computers, restart agents after integration, then launch:

```sh
mesh codex
```

Press **Ctrl-b**, release, then **m** to select a computer. **Ctrl-b b** returns to the previous conversation; **Ctrl-b d** detaches while keeping sessions alive. Use `mesh shell` to try these controls without an AI provider.

Each conversation keeps its own history. To carry current work along, ask the agent:

> Continue this task on homelab, taking the relevant context and files with you.

The agent prepares a brief and invokes `mesh handoff`. Success includes `terminal_switched: true` only after Mesh verifies the destination agent, its terminal connection, and the displayed window. If context arrives but switching fails, restore connectivity and retry the same command with the reported `--id ID`.

A new destination conversation starts with the brief; an existing conversation reads it on its next user turn. The source conversation remains alive. Normal provider approval and workspace trust prompts still apply.

Use `mesh session` for the current conversation's identity; cached `MESH_*` variables can be stale. Switching needs an attached Mesh terminal. After an upgrade, refresh integration on both computers and start a new Mesh terminal to load the new controller and launch settings.

## Next steps

- [Run jobs asynchronously, inspect status, and recover delivery](../README.md#3-keep-working-while-a-job-runs).
- [Try a file round trip or summarize your own notes](../examples/README.md).
- [Learn enrollment, permissions, services, and cleanup](REFERENCE.md).
- [Validate your two computers step by step](TESTING.md).
- [Inspect recorded real-machine results and screenshots](VALIDATION.md).
