# ai-mesh

ai-mesh lets people use their existing AI agents across the computers they are authorized to access. A person can stay at one computer, continue using familiar tools such as Codex or Claude Code, and have work happen on another machine with the appropriate hardware, files, or environment.

The experience should feel like working with one connected set of computers: choose where to work, delegate a task, switch into a remote agent session, or let an agent distribute suitable work across available machines. Mesh handles the connections, session continuity, context handoffs, progress, and return of results.

This document captures the complete product idea and intended user experience. It is not an implementation plan, delivery schedule, or claim that these capabilities already exist. The project and its repository are private for now.

## Why this project exists

People increasingly have access to several useful computers: a daily laptop, a home server, a spare laptop acting as a homelab, a workstation with a GPU, or a remote development machine. Each may have different strengths and different software, files, datasets, or models already available.

Agent workflows often remain attached to whichever computer started the conversation. Using another machine means manually connecting, moving inputs, recreating context, starting the agent, following its progress, and bringing results back. Moving between computers interrupts the work even when the user already has legitimate access to all of them.

ai-mesh brings those computers into the same working environment while preserving the agents and interfaces the user already knows.

A motivating example is a MacBook used every day and a laptop running as a homelab at home, reachable over SSH. The user should be able to ask an agent on the Mac to use the homelab without manually managing the remote workflow.

## The core experience

### Delegate work and keep the original conversation

The user says:

> Read this file on my Mac, follow its instructions, and create a PDF. Do all the computation on @homelab and bring the result back here.

The local agent can discover the available computers, identify the requested destination, send the necessary files and context, and arrange for an agent there to do the work. The original conversation remains on the Mac.

While the remote task runs, the user can see its status and the messages, commands, outputs, errors, and other trace information exposed by the remote agent. When it finishes, Mesh retrieves the resulting PDF and other requested artifacts. The local agent can then explain the result and continue the conversation.

The user expresses the task and destination. Mesh manages the remote execution and return journey.

### Run a workload on another computer

Some work needs a remote process rather than a second reasoning agent. The current agent may prepare a training script, run it on a GPU computer, monitor the process, and retrieve its outputs while continuing to coordinate from the original machine.

Mesh should support both remote commands and complete remote agent tasks. The appropriate choice depends on the work.

The remote computer supplies the environment for commands, files, and workloads. Moving Codex or Claude Code there does not imply that their provider-hosted language model inference moves onto that computer's hardware.

### Enter a remote agent session

The user can start their usual agent locally or on a selected computer through Mesh. The working command name is `mesh`; the following examples describe the intended interface rather than existing commands:

```sh
mesh claude
mesh claude homelab
mesh codex homelab
mesh connect
mesh claude homelab --project my-project
mesh claude homelab --resume
```

`mesh connect` provides a computer-and-agent picker. The launcher preserves the familiar agent interface, with the keyboard and display on the user's current computer and the selected session running on its destination computer.

The exact rules for selecting a project or resolving an ambiguous resume request remain to be defined.

### Switch computers from inside the conversation

The user can launch Claude or Codex through Mesh and then say:

> I want to work on homelab.

A proposed command such as `/computer homelab` or `/SelectComputer` could express the same intent where the agent supports that integration.

Mesh preserves the current session and changes the connection in the same terminal window to an agent session on the selected computer. The user does not have to open another terminal or manually run SSH.

Later, the user says:

> Take me back to my Mac.

Mesh restores the preserved local session. A persistent machine label makes it clear where commands and filesystem operations will happen. Work that is allowed to continue can remain active after the user switches away, and Mesh keeps track of it.

This same-terminal experience is a core product requirement. It depends on starting the interactive session through Mesh. Background delegation remains useful from an ordinary agent session without using the launcher.

### Let agents use other computers when appropriate

A task can begin on one computer and use additional computers for suitable parts of the work. For example, a homelab agent preparing a report could delegate model training to a GPU computer and independent figure generation to another machine, then assemble the results and return the report to the originating Mac.

Every participating computer can initiate or receive work within its access permissions. A remote agent has access to the same shared picture of available computers and capabilities as an agent on the original machine.

Explicit placement instructions matter:

- "Start this on the homelab" can allow further delegation within the task's constraints.
- "Run everything only on the homelab" keeps all of that task's computation there.

The product should make these meanings clear. Availability or a faster GPU must not silently override a user's restriction.

## Installation and enrollment

The desired setup is simple: install ai-mesh on one computer, select other computers, authenticate to them, and let Mesh configure the selected group. Users can also install Mesh individually on multiple computers.

Mesh should minimize repeated setup. It discovers supported installed agents, adds the instructions needed to use Mesh, installs its service and commands, and gathers an initial description of the machine.

The onboarding experience includes two explicit choices:

> Allow other enrolled computers to run tasks on this computer? Yes / No

> Install Mesh and configure mutual access on the selected computers? Yes / No

The first choice controls incoming access to the computer where setup began. If enabled, the user chooses the local account that other enrolled machines may access. If disabled, that computer can still initiate remote work without accepting incoming tasks.

The second choice lets the user authorize remote installation and mutual configuration across the selected computers. Full mutual access applies to the participating computers that accept incoming access. A computer that declines incoming access is not silently included as an execution destination.

### Discover computers before Mesh is installed

Mesh should find candidate computers on the local network and among visible Tailscale devices, including computers that do not yet have ai-mesh installed. Existing SSH destinations and manually entered addresses are also useful entry points.

Discovery is a convenience for finding candidates. It does not automatically enroll a computer or grant access to it. It also cannot guarantee finding every device: some may be asleep, isolated, unreachable, or not advertising a discoverable service.

For SSH-based enrollment, the destination must already support a reachable SSH connection. A computer without that access needs a local setup step first.

### Access establishes membership

Computers do not need to share an owner, a Tailscale user, or a Mesh account. The relevant condition is that the person enrolling a computer is authorized to use the selected account and to configure Mesh within that account's permissions.

The wizard authenticates to each selected computer. It can use an existing SSH login or request a username and the authentication required by the destination. It then establishes ongoing access so the user does not have to repeat the login for every task, subject to the destination's authentication policies.

Enrollment is scoped to a computer and an account. It does not imply administrator access to the whole machine.

Each computer retains its own private access credentials. Mutual configuration shares the necessary public keys rather than distributing one private key throughout the group. Initial passwords are authentication inputs, not shared memory or information to give an LLM.

Enrolled access should be understandable and removable. Finding a new computer on the network alone must never make it a trusted participant.

## A mesh of peers

ai-mesh has no permanent master computer. The computer that performs installation is a temporary setup coordinator; it has no special runtime role afterward.

With mutual access configured, each participating computer can:

- Discover the other enrolled computers and inspect their known capabilities.
- Initiate tasks on other permitted computers.
- Receive tasks from permitted peers.
- Publish updates about its own capabilities.
- Exchange inputs, outputs, and task context.
- Coordinate work that it delegates to other machines.

Turning off the computer that originally configured the group should not stop the remaining reachable computers from working together.

Coordination belongs to individual tasks. A homelab may coordinate one task while a workstation coordinates another. Peer equality does not remove user-selected access restrictions, and it does not guarantee that every computer is reachable at every moment.

New members can be enrolled later. Existing computers that are offline receive the applicable membership and inventory updates when they reconnect.

## A lightweight, shared understanding of the computers

Every computer should have access to a shared directory of machine descriptions. Agents use it to understand where work can run and what resources may already exist.

The initial inventory should be fast and modest. A short installation-time collection can record readily available information such as the operating system, architecture, CPU, memory, GPU, installed agents, and common tools.

The product should not depend on exhaustive scans, continuous resource monitoring, or a dedicated background LLM to maintain this information. Unknown information can remain unknown until it becomes useful.

### The inventory grows through actual work

As an agent works on a computer, it can recognize information that would help future tasks and record it through Mesh. Examples include:

- A working environment for generating PDFs.
- A Python environment with a particular training library.
- A useful dataset already stored on that machine.
- Downloaded model weights that do not need to be transferred again.
- A tool that was installed, upgraded, removed, or found not to work.

Entries should retain enough context to be useful. A capability in one virtual environment or project does not automatically apply to the entire computer. Observations should be distinguishable from user rules and directly measured facts.

The user should not have to maintain the directory manually. Agents contribute useful observations during normal work, with refresh and verification available when information is missing or stale.

### Files are shared and kept current

Each computer publishes its own machine description during initial enrollment, and the other computers keep copies. After that, descriptions are shared only when their contents change. Idle computers should not repeatedly exchange unchanged specs on a timer. Pending updates reach disconnected computers when they reconnect.

The desired result is a shared view that converges as machines communicate. Cached information remains useful while a peer is offline, but must not be presented as a guaranteed live reading. Before dispatching work, Mesh checks that the destination can actually accept it.

Concurrent updates should preserve useful information and avoid silently losing changes. Each computer being responsible for its own description reduces conflicts between machines. Agents on the same machine may still discover different capabilities concurrently.

LLM-assisted reconciliation of descriptive notes is a possible extension. Hardware facts, access permissions, and user rules must not be invented to resolve a conflict. The specific synchronization and conflict-resolution mechanism is outside this product vision.

### Capability and permission are different

A powerful computer may be technically capable of a task without being available or permitted to run it. The shared view can include user preferences such as allowing training or limiting heavy work to times when a laptop is plugged in.

Such preferences are intended capabilities of the product; the initial inventory can remain small. An agent observing a useful resource does not grant itself permission to use it.

## Tasks, progress, and results

A remote task is a durable piece of work with a destination, inputs, relevant context, expected outputs, and observable status.

Mesh should make it possible to understand what is happening, inspect the available trace, continue the conversation, respond when the remote agent needs input, and stop work when appropriate. The amount and kind of trace available depends on what the underlying agent exposes.

A task running on an available destination should survive the originating computer disconnecting. The user can reconnect to observe progress or retrieve results. Reconnecting should not accidentally start the same task again.

### Execution and delivery are separate

Two questions must remain distinct:

- Has the task finished executing?
- Have its outputs reached the requested destination?

A PDF can be complete on the homelab while the Mac is offline. Mesh retains that result and delivers it when the Mac becomes available. The same distinction applies to intermediate results exchanged between agents working on a larger task.

The user should be able to tell whether work is still running, needs attention, failed, finished remotely, or is awaiting delivery. Results should arrive in the requested local location without silently replacing unrelated work.

## Distributed work and nested delegation

Agents can use Mesh to delegate complete tasks or independent subtasks to other agents and machines. The aim is to make useful hardware and environments accessible, and to accelerate work when parallel execution helps.

Delegated work retains its relationship to the original request, its constraints, and its expected return destination. Child tasks inherit relevant limits, including allowed computers and restrictions on further delegation or concurrency.

This relationship lets an agent gather its children's outputs and continue its own work. It also makes the activity understandable to the user instead of leaving a collection of unrelated remote sessions.

Distribution should account for actual suitability: available resources, required software, relevant data already present, and the cost of transferring inputs. Launching more agents is not automatically faster; work with dependencies still needs coordination.

A computer disconnecting does not imply that its running task has failed, and being a peer does not mean another computer can automatically resume an interrupted process. Recovery must reflect what is known about the work and any saved results.

## Familiar agents and a CLI-first interface

The first experience centers on the CLI versions of mainstream agents, beginning with Codex and Claude Code. Mesh should preserve their normal interactive interfaces and extend their ability to work across computers.

The CLI is the primary integration surface. Agents use Mesh through their existing ability to run commands. An MCP server is not required for the initial product.

Installation should detect supported agents and add concise instructions explaining how to discover machines, delegate work, inspect progress, retrieve outputs, request a computer switch, and record useful capabilities. It should preserve the user's existing agent instructions and configuration, and make Mesh's additions removable.

Agent support will depend on each agent's documented extension and execution capabilities. Automatic setup should be predictable for supported agents rather than assuming every agent exposes identical features.

The terminal launcher is used for the seamless same-window switching experience. Ordinary agent sessions can still use the CLI for background delegation. Desktop integration is a future extension where the host application provides suitable capabilities; a new chat application is not a prerequisite.

## Context, sessions, and shared memory

Switching computers, resuming a session, and handing off a task are related but distinct experiences:

| Experience | Intended meaning |
| --- | --- |
| Start fresh | Begin a new agent session on the selected computer. |
| Resume | Reconnect to a selected existing session on that computer. |
| Return | Restore the session the user previously switched away from. |
| Hand off | Give another agent the context and files needed to continue a task. |

The user should understand which experience is happening. A machine switch does not inherently transfer the previous conversation.

For a task handoff, the destination needs the objective, relevant conversation context, decisions, constraints, inputs, and expected results. Available conversation history can accompany a concise task brief when useful.

Exact migration of a complete session between machines or different agent products is not an established universal capability. Native continuation can be used where supported; a portable task brief and selected context are the baseline across agents. Mesh should not promise access to hidden or unavailable provider state.

The immediate shared knowledge is the computer directory and the context and artifacts needed for ongoing work. Over time, project knowledge and decisions could also become accessible across agents and computers.

### Future memory replication

A longer-term idea is to keep replicas or caches of useful project memory and artifacts on multiple computers, so information remains accessible even when its original computer is offline. This resembles a distributed cache for agent knowledge.

That future could include versioned project notes, task histories, artifact references, datasets, and other reusable material. Replicating information is separate from migrating a live process or guaranteeing recovery of an interrupted workload.

Broad distributed memory is a future direction, not a dependency for the initial product. Its conflict, retention, and replication behavior remains to be defined.

## Provider accounts and subscriptions

The desired user experience is to continue using familiar, officially authenticated agents and existing subscriptions wherever their providers support that use.

Mesh membership and agent-provider authentication are separate. Access to a remote computer does not establish a Codex or Claude login there, and Mesh should not assume that enrolling one machine transfers provider credentials to the others.

Subscription compatibility, programmatic access, and the permissions of a distributed product must be validated for each supported provider and integration. Existing subscriptions are a product preference, not a blanket promise that every remote or automated workflow will be covered.

## Principles that define the project

- **Keep the user's familiar workflow.** The user stays in their chosen agent and can remain in the same terminal window.
- **Make computers useful as a group.** Hardware, software, and existing data can inform where work happens.
- **Keep setup lightweight.** Fast initial discovery and inventory should be enough to begin.
- **Automate the repetitive work.** Connections, remote setup, file movement, status, and result collection should be handled by Mesh.
- **Respect explicit enrollment and task boundaries.** Discovery, account access, mutual participation, and permission to delegate have distinct meanings.
- **Give peers equal capabilities within their permissions.** No computer is a permanent master or required central hub for the others' work.
- **Preserve work across client disconnects.** A lost connection should not erase a session, duplicate a task, or discard completed results.
- **Make execution location visible.** The user should know which computer and account are being used.
- **Keep shared knowledge honest.** Distinguish observed facts, cached information, and user instructions.

## Questions still open

The product direction is established, while several details remain open for later design:

- The final project branding and exact CLI and slash-command syntax.
- Supported operating systems and agents at each stage of the product.
- How destination sessions are selected when more than one can be resumed.
- The fidelity of context transfer available from each agent.
- Provider subscription compatibility and authentication requirements.
- The precise experience for forwarding questions and approvals from remote agents.
- Membership changes, access removal, and conflict handling while computers are disconnected.
- How automatic placement and delegation limits are presented to the user.
- The scope and behavior of future shared project memory and replicated caches.
- Whether distribution eventually becomes open source, hosted, or a combination; the project remains private for now.

These questions refine the product. Its central idea remains that a person can work naturally with their usual agents while their authorized computers cooperate to carry out the work and return the results wherever the person needs them.
