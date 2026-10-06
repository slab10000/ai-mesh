# Manual test: Mac → green-lighthouse

This manual checklist complements the completed real-machine tests in [VALIDATION.md](VALIDATION.md) and the [short usage guide](QUICKSTART.md). The default Go tests use fixtures; the opt-in `scripts/e2e.py` suite contacts the destination you specify. The tests below deliberately separate connectivity, file delivery, providers, and terminal switching so failures are easier to locate.

## 1. Build and install locally

From this repository on the Mac:

```sh
sh scripts/install.sh --no-setup
export PATH="$HOME/.local/bin:$PATH"
mesh version
mesh doctor
```

This builds and installs the Mac executable and destination binaries. `--no-setup` postpones enrollment, agent instructions, and services until you request them below. Go 1.24+ is needed for the build. Install tmux on both computers before testing interactive sessions.

## 2. Check your existing SSH access

```sh
tailscale status
ssh blas@green-lighthouse 'uname -s; uname -m; command -v tmux'
```

Use your own SSH username if you repeat this on another server. Use the server's full Tailscale DNS name or address if the short name does not resolve. Confirm any new host key using the verification you normally use for SSH. Mesh uses this existing SSH access; it does not configure the Tailscale account or SSH server.

If the `tailscale` command is not installed, use the Tailscale app's computer list and pass the address directly to SSH and Mesh.

On Linux, `x86_64` maps to `mesh-linux-amd64` and `aarch64` maps to `mesh-linux-arm64`. Both binaries are installed beside the Mac executable automatically.

## 3. Initialize and enroll without background services

```sh
mesh init --name mac --incoming=false
mesh enroll green-lighthouse --user blas \
  --name green-lighthouse --mutual --service=false --integrate=false
mesh machines --check --json
mesh sync
mesh machine show green-lighthouse
```

Expected: two distinct machine IDs, a successful remote check, and the server's OS/architecture and detected tools. The enrollment may prompt through OpenSSH for authentication. Subsequent Mesh calls should use the Mac's generated key without a password.

This keeps the Mac outgoing-only. It still receives results by fetching them over its outgoing SSH connection. If you later want the server to initiate jobs on the Mac, first make your Mac account reachable through SSH, then run `mesh access --incoming=true`. Use the setup wizard initially if you want to choose a reachable local address interactively.

If the Mac was initialized before this guide, `init` keeps its existing identity and settings. Inspect `mesh doctor` and change access with `mesh access` when needed.

## 4. Test file transport without an AI provider

```sh
mkdir -p mesh-manual-test
printf 'hello from the Mac\n' > mesh-manual-test/input.txt
mesh run --on green-lighthouse --agent shell \
  --input mesh-manual-test/input.txt \
  --output mesh-manual-test/received --only --wait -- \
  sh -c 'hostname > "$MESH_OUTPUT_DIR/executed-on.txt"; cat inputs/input.txt > "$MESH_OUTPUT_DIR/echo.txt"'
cat mesh-manual-test/received/executed-on.txt
cmp mesh-manual-test/input.txt mesh-manual-test/received/echo.txt
```

Expected: the host file identifies the server, `cmp` succeeds, and `mesh status JOB_ID` reports `completed` with delivery `delivered`. The JSON receipt printed by `run` contains `JOB_ID`.

Run `mesh collect JOB_ID` again: identical redelivery should succeed. If you deliberately edit a received file, collecting should refuse to overwrite it. Use `mesh collect JOB_ID --output ANOTHER_DIRECTORY` to retrieve a fresh copy.

## 5. Test execution separately from delivery

```sh
mesh run --on green-lighthouse --agent shell \
  --output mesh-manual-test/later -- \
  sh -c 'sleep 30; date > "$MESH_OUTPUT_DIR/finished.txt"'
```

Save the returned ID. Close the submitting terminal or temporarily disconnect the Mac. Reconnect after the server has had enough time:

```sh
mesh status JOB_ID
mesh collect JOB_ID
mesh status JOB_ID
```

Before collection, execution should be `completed` while delivery remains `pending`. After collection, delivery should be `delivered`. There must be only one remote job with this ID.

If submission itself returns an uncertain connection error, use the ID from that error with `mesh retry JOB_ID`. Do not create another `run` just to retry the connection.

For cancellation, submit a `sleep 120` job and use `mesh cancel JOB_ID`. Check for `cancelled`; this cancels that job, not any separately submitted children.

## 6. Try your real agent

Ensure the desired CLI is already authenticated on the server under the enrolled account. Do not copy credential files through Mesh.

```sh
mesh run --on green-lighthouse --agent codex \
  --input mesh-manual-test/input.txt \
  --output mesh-manual-test/agent-result --only --wait \
  'Read inputs/input.txt. Write a short summary to outputs/summary.md.'
```

Repeat with `--agent claude` if desired. This step uses the provider normally and may consume subscription usage. `mesh watch JOB_ID` shows raw provider events and permission/authentication errors.

For a PDF test, send your instruction file with `--input`, optionally send a handoff brief with `--context`, and request a PDF in `outputs/`. The server needs whatever PDF tools the task uses. If permissions or authentication require interaction, test in a native session below; Mesh does not bypass those checks.

## 7. Enable agent instructions and automatic delivery

```sh
mesh integrate
mesh service install
ssh blas@green-lighthouse \
  '"$HOME/.local/bin/mesh" integrate; "$HOME/.local/bin/mesh" service install'
mesh service status
```

The server service needs a working `systemd --user` manager on Linux. If unavailable, run `~/.local/bin/mesh daemon` there under your normal process supervisor. The Mac service requires the current account's GUI login. Both services should report failures rather than silently ignore them.

Repeat a short task without `--wait`; the Mac service should retrieve its output on a later poll. Let the Mac disconnect and reconnect to test delayed retrieval. Keep the remote job directory until the local copy is verified.

## 8. Switch terminals and return

```sh
mesh codex
```

Inside that Codex conversation, ask it to run `mesh switch green-lighthouse`. Expected: the same terminal changes to the remote Codex session, with the machine label visible. Ask the remote agent to run `mesh back`; the original Mac session should still be present.

You can start directly on the server with `mesh claude green-lighthouse`, choose through `mesh connect`, or use `--project '~/existing/project'`. Destination project paths must exist. `Ctrl-b d` detaches; launch again with `--resume` to reconnect.

This test needs tmux on both sides and SSH Unix-socket forwarding. An agent sandbox may require permission to call the Mesh controller socket. A normal terminal outside the Mesh-launched session lacks that session's controller environment and cannot issue its switch commands. Switching does not transfer the conversation automatically.

## 9. Inspect and remove setup changes

Useful diagnostics:

```sh
mesh jobs
mesh status JOB_ID
mesh machine show green-lighthouse
mesh doctor
```

Task events and worker errors are under `~/.ai-mesh/jobs/JOB_ID/` on the executing computer. The Mac's service log is `~/.ai-mesh/logs/daemon.log`; Linux service logs are available through `journalctl --user -u ai-mesh`. Session-controller logs are under `~/.ai-mesh/logs/` on the originating computer.

To remove the integration and service from an account, run `mesh integrate --remove` and `mesh service uninstall` on that account. Use `mesh peers remove green-lighthouse` from the Mac while both sides are reachable to revoke the Mesh relationship. Stop unwanted jobs separately. Keep state until pending deliveries and revocations have been checked; uninstalling a service does not erase results or revoke SSH keys by itself.

When reporting a problem, include the command, Mesh version, relevant OS/architecture, job ID, and the relevant error. Review logs before sharing them because they can contain task inputs and provider output.

## Change-driven inventory sharing

After initial enrollment, a computer publishes its description only when it changes or a newly enrolled peer needs its first copy. `mesh sync` flushes pending publications; repeated calls with no changes should make no inventory SSH requests. The maintenance timer retries missed deliveries and handles jobs.

The race-enabled tests exercise idle ticks, persistent acknowledgments, offline peers, lost acknowledgments, direct edits without revision changes, atomic saves, new/re-enrolled peers, and publication before a one-hour maintenance tick.

For a real Mac/server check:

```sh
python3 scripts/e2e_inventory.py --ssh blas@green-lighthouse
```

This writes and removes uniquely named temporary capability entries directly in each computer's own description, verifies automatic publication in both directions, observes 65 seconds of idle maintenance, and restarts both installed Mesh services to check that unchanged descriptions are not resent. It does not change membership or SSH keys. The JSON report is written to `artifacts/e2e/inventory-events.json`.
