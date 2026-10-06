# Example recipes

Run these commands from the repository root after [installation and enrollment](../README.md#installation). Replace `homelab` with your destination's Mesh name. Use a fresh output directory when repeating a task whose output may change.

## Copy a file through a remote job

This checks input transfer, execution, and output delivery without an AI provider:

```sh
mkdir -p results/roundtrip
printf 'Hello from my laptop.\n' > results/roundtrip/message.txt

mesh run --on homelab --agent shell \
  --input results/roundtrip/message.txt \
  --output results/roundtrip/returned \
  --expect message.txt --expect host.txt --only --wait -- \
  sh -c 'cat "$MESH_INPUT_DIR/message.txt" > "$MESH_OUTPUT_DIR/message.txt"; hostname > "$MESH_OUTPUT_DIR/host.txt"'

cmp results/roundtrip/message.txt results/roundtrip/returned/message.txt
cat results/roundtrip/returned/host.txt
```

`cmp` should exit successfully without output. The hostname should identify your destination. Command jobs run in a new workspace and receive `MESH_INPUT_DIR`, `MESH_OUTPUT_DIR`, and `MESH_JOB_ID`.

## Generate a PDF with a remote agent

Requires Python 3 and an authenticated Codex installation on the destination. No additional PDF package is required by the instructions.

```sh
mesh run --on homelab --agent codex \
  --input examples/pdf-instructions.md \
  --context examples/handoff.md \
  --output results/pdf \
  --expect remote-report.pdf --expect generate_pdf.py \
  --expect generated-on.txt --only --wait \
  'Read inputs/pdf-instructions.md and produce all requested outputs.'
```

Open `results/pdf/remote-report.pdf`, inspect `results/pdf/generated-on.txt`, and compare the document with [the input instructions](pdf-instructions.md). The [task brief](handoff.md) supplies a context marker that should appear in the PDF, demonstrating that the remote agent received context separately from the input file. The generator is returned alongside the document.

Use `--agent claude` for an authenticated Claude Code installation. This example makes a real provider request and uses that account's normal permissions and usage allowance.

## Summarize your own files

Create a `notes/` directory containing the text files you want to summarize, then run:

```sh
mesh run --on homelab --agent codex \
  --input ./notes --output ./results/summary \
  --expect summary.md --only --wait \
  'Read the text files in inputs/notes/. Write outputs/summary.md with the key points, decisions, and open questions. Cite source filenames. Do not install tools.'
```

The directory's basename is preserved: `./notes` becomes `inputs/notes/`. Repeat `--input` to include another file or directory. Mesh transfers regular files in the selected paths, including hidden files; choose inputs intentionally and stay within the 32 MiB bundle limit.

## Hand off a live conversation

Start with `mesh codex` or `mesh claude`. Ask the agent to prepare a brief using [conversation-handoff.md](conversation-handoff.md), select the files the next computer needs, and invoke:

```sh
mesh handoff homelab --context BRIEF.md --input selected-file.txt
```

The two named files must exist before the command runs. A successful result includes `terminal_switched: true`, confirming that the destination agent and terminal connection are ready and an attached frontend displays its window. A detached terminal or failed attachment returns an error even if context has already arrived.

A new destination conversation receives a startup prompt; an existing conversation consumes the brief on its next user turn. Check the receiving conversation with `mesh session`, then read the inbox and selected files before acknowledging its returned ID:

```sh
mesh session
mesh inbox --read
mesh inbox ack HANDOFF_ID
```

Use `mesh back` or **Ctrl-b b** to revisit the source conversation. Ordinary navigation does not create another handoff. If a handoff returns an uncertain result, restore connectivity and repeat the original command with the reported `--id ID`. Trust `mesh session` over cached shell variables, which can describe an older conversation. Provider permissions still apply to these commands.

See the [tutorials](../README.md#tutorials) for background jobs and the [operations reference](../docs/REFERENCE.md) for delegation limits and recovery.
