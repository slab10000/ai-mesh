# Contributing to ai-mesh

Start with the [README](README.md) for the user workflow and [PROJECT.md](PROJECT.md) for the longer-term direction. The [operations reference](docs/REFERENCE.md) describes behavior implemented today.

## Development setup

Use macOS or Linux with Go 1.24+, Git, `make`, a C compiler for Go's race detector, OpenSSH (`ssh` and `ssh-keygen`), Python 3, and tmux. The tests use fake agents, so you do not need a provider account or a second computer.

```sh
git clone https://github.com/slab10000/ai-mesh.git
cd ai-mesh
make build
./bin/mesh help
make check
make dist
```

`make check` runs `go vet ./...` and `go test -race ./...`. Install tmux so terminal tests run rather than skip. `make dist` cross-compiles macOS and Linux binaries for both arm64 and amd64. Building does not enroll machines, install a service, or alter agent instructions.

## Find your way around

| Path | Responsibility |
| --- | --- |
| `cmd/mesh/` | Executable entry point. |
| `internal/mesh/cli.go` | Command parsing, help, and setup wizard. |
| `internal/mesh/enroll.go`, `transport.go` | Account enrollment, SSH access, and peer transport. |
| `internal/mesh/ssh_config.go` | Managed SSH client aliases and local repair during installation or upgrades. |
| `internal/mesh/jobs.go`, `dispatch.go`, `files.go` | Job lifecycle, provider adapters, scheduling, and checked file transfer. |
| `internal/mesh/sessions.go`, `session_identity.go`, `session_display.go`, `handoff.go` | tmux sessions, conversation identity, verified visible switching, and explicit context handoff. |
| `internal/mesh/session_exit.go` | Return to the calling shell on provider exit, replay final output, and preserve exit status. |
| `internal/mesh/inventory*.go` | Machine descriptions, capabilities, and publication. |
| `internal/mesh/service.go`, `pending.go` | User services and retrying pending work. |
| `internal/mesh/*_test.go` | Isolated regression and integration tests. |
| `scripts/` | Build/install scripts and opt-in real-machine validation. |
| `examples/`, `docs/` | User recipes, operational details, and recorded evidence. |

## Make a change

1. Create a branch with one clear purpose. Discuss substantial changes in an issue first.
2. Keep command behavior, `mesh help`, and the corresponding documentation in agreement. Use generic names such as `laptop`, `homelab`, and `alice` in tutorials.
3. Format changed Go files with `gofmt`. Add a regression test when changing behavior, especially for transport, permissions, retries, file paths, and concurrency.
4. Run `make check`. Run `make dist` if you change Go code or build scripts. For documentation changes, verify links, paths, and example flags against the CLI.
5. Open a pull request explaining the problem, resulting behavior, and checks you ran. Call out any untested platform or provider behavior.

Preserve these distinctions in code and documentation:

- Execution completion and delivery completion are separate states.
- Switching selects a live conversation; a handoff explicitly transfers context and files.
- Context delivery alone is not a confirmed terminal switch. Only report handoff success after verifying the destination and attached frontend; retain the same handoff ID for recovery.
- Resolve the calling conversation through Mesh identity logic rather than cached shell variables, which may describe another live conversation.
- Cached inventory describes capability; it does not grant permission.
- A lost connection is not proof that a job failed. Retry the same identity after an uncertain submission.
- Provider authentication, user permissions, and explicit placement limits remain in force.

## Test isolation

The default suite uses temporary state, fake SSH endpoints and agent executables, and isolated tmux servers. It does not contact your enrolled peers, install real services, or make provider requests.

For manual experiments, `MESH_HOME` and `MESH_USER_HOME` can select disposable state and user-file directories. They do not isolate arbitrary shell commands or replace the fake agents and SSH endpoints used in the tests. Never point an isolated experiment at a real peer unintentionally.

The scripts below are **opt-in real-machine tests**, described in [TESTING.md](docs/TESTING.md). They require already-enrolled computers you control:

```sh
python3 scripts/e2e.py --on homelab --origin laptop
python3 scripts/e2e.py --on homelab --origin laptop --providers
```

The second command makes real Codex requests in both directions. Both computers need incoming access for the reverse-delegation tests. `--claude` adds an optional Claude test. The inventory suite also changes temporary capability entries and restarts Mesh services; read its description before running it.

Do not label fixture coverage as real provider or hardware validation. Keep dated evidence and its limitations together in [VALIDATION.md](docs/VALIDATION.md).

## Report a bug

Include your Mesh version, operating system and architecture, the command that failed, expected versus actual behavior, and a minimal reproduction. For job failures, include the relevant status/error code and a short log excerpt. Remove credentials, private inputs, personal paths, and unrelated provider output before sharing.

Generated binaries, local results, runtime state, and provider credentials do not belong in a pull request.
