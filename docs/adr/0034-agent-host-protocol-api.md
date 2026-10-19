# ADR 0034: Serve the Agent Host Protocol as an Orka client API

Date: 2026-10-17

## Status

Proposed. Adds an [Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) endpoint to the Orka API server, behind `--enable-ahp-api` (default off), with
session backends. The first backend runs each turn as an Orka Task on Orka's built-in
runtimes over ACP. A backend that re-hosts sessions from per-user agent hosts
(ADR 0035, proposed separately) is left for later. Lands in stages; the core
capabilities listed under **Prerequisites** come first.

## Context

AHP defines how clients share a synchronized view of agent sessions served by a
standalone host. The host owns sessions, worktrees, tools and approvals; the client is a
UI. AHP clients include the GitHub Copilot desktop app and VS Code's agent sessions UI.
AHP sits above ACP: a host speaks AHP to its clients and may speak ACP to the agents
behind it.

Orka already serves two client protocols, the OpenAI- and Anthropic-compatible APIs,
which map standard requests onto Orka execution. Gateways (`orka.gateway.v1`) carry
text-only envelopes for messaging platforms and cannot carry tool rows, diffs,
approvals or cancellation. A separate bridge that maps AHP onto a gateway stubs most of
AHP for that reason.

What a host must get right, learned from the reference host in VS Code, the public
AHP-over-ACP hosts and the AHP issue tracker:

- There is no server SDK. Microsoft's Go module
  (`github.com/microsoft/agent-host-protocol/clients/go`, MIT) provides wire types,
  pure reducers and a WebSocket transport. The per-action version table and the table of
  client-dispatchable actions exist only in the TypeScript sources.
- The protocol has one `serverSeq` per host and no user concept. Hosts that keep that
  sequence in memory break reconnect after a restart; an empty replay to an unknown client
  left clients showing stale state. Anyone who learns a `clientId` can hijack its
  reconnect.
- Approval modes are not in the spec; each host defines its own session-config vocabulary,
  and clients differ in which `_meta` keys and URI shapes they expect.
- Transport gating is outside the protocol and should happen at the handshake. Clients
  pass a connection token as `?tkn=`; some can send headers.
- ACP-to-AHP mappings converge on a tool-call state machine, the plan as one synthetic
  tool row, diffs as file edits whose contents the host serves, never defaulting a
  permission to an "always" option, and settling pending permissions before cancelling.

Orka today runs each agent turn as a Task with policy-decided approvals. Task and
Session event streams exist, but they are read from a journal every few seconds,
assistant text is persisted only after redaction at turn end, and ACP diff and terminal
content is dropped. Session-scoped workspace reuse exists, fenced by the Session's UID
and lineage. There is no cancel request short of deleting the Task, no human prompt for
built-in runtimes' permission requests, and no read access to a live workspace.

## Decision

### An AHP endpoint in the API server

`/ahp` accepts AHP over WebSocket (text frames, one JSON-RPC message each) next to the
OpenAI- and Anthropic-compatible groups. It is off by default.

### AHP connection tokens identify users and carry no more authority than they had

- Minting an AHP connection token requires explicit permission, checked like any other
  API operation, and an OIDC or context-token caller (the identities that may own
  Connections). ServiceAccount callers cannot mint one.
- A token minted from a context token keeps that token's scopes, namespace and
  transaction constraints and expires no later than it; Orka stores the constraints and
  never the raw context token or TxToken.
- Tokens are stored hashed, compared in constant time, revocable and scoped to `/ahp`.
- The token arrives as `?tkn=` or `Authorization: Bearer`. A missing, wrong or revoked
  token is refused with 401 before the WebSocket upgrade. Browser origins are refused.
  URLs and `authenticate` frames are redacted from every log.
- Every AHP command and client action is authorized as the Orka operation it causes
  (for example creating a Task or answering an approval), with the token's constraints;
  being allowed onto `/ahp` authorizes nothing else. `requestedBy` is stamped only after
  that check, as provenance.
- Revocation or expiry closes every connection using the token and stops delivery,
  including queued output; passive subscribers are not exempt.
- `initialize`, `subscribe` and `reconnect` authorize each channel. Replay and snapshots
  include only what the user may still see; anything else is returned in `missing`.

### The host core

- It uses the Go module's types and reducers; Orka ports the two missing tables.
- Each user has their own sequence, replay journal, snapshots and session catalog, stored
  durably in Orka's store. Replay is bounded by count and bytes; a gap beyond it gets
  fresh snapshots.
- A client record binds a `clientId` to the user who first used it and stores the
  negotiated protocol version and dialect. `reconnect` restores them before delivering
  anything; with no record, or a record for another user, it is rejected and the client
  must `initialize` again. It never gets an empty replay.
- Every client action is validated before it is applied; a rejected action is echoed back
  with `rejectionReason`.
- Each connection negotiates its version independently (0.9.0 and 1.x), and the host never
  sends an action newer than the negotiated version, in live delivery, replay or
  snapshots.
- A dialect layer per connection translates client-specific URI shapes, `_meta` keys and
  session-config vocabulary. The Copilot desktop app comes first; it uses the
  `approvalMode` vocabulary (`manual`, `assisted`, `allow-all`) and project commands.

### Session backends

A session router chooses a backend when a session is created. The first backend is ACP;
an AHP backend for agent hosts is a later decision.

### The ACP backend

- Agents: Orka's built-in runtimes (copilot, claude, codex, opencode). Each Agent in the
  user's namespace on a built-in runtime with a model is one model of that runtime's AHP
  agent, offered only when the user's token would let them run it. No agent advertises a
  protected resource: Orka never asks for the user's own credentials.
- An AHP session is an Orka Session. An AHP turn is a Task with `sessionRef`, the chosen
  Agent and the project's repository in `spec.workspace`, created and authorized as the
  person. A session keeps the Agent of its first turn, because an Orka Session is bound to
  one runtime profile; a turn naming another model is refused. When an operator names an
  execution workspace class, turns use session-scoped workspaces so files stay warm;
  otherwise each write turn publishes and the next turn starts from it.
- Projects are repositories the user adds; Orka never clones them on the host, because
  each turn's Task clones its own workspace. The agent never holds Git credentials. The
  operator names one credential Secret that is looked up in each user's namespace; without
  it, sessions clone public repositories read-only. Write turns publish to an Orka-owned
  session branch through the Publisher.
- Mapping: text and reasoning chunks stream as deltas; tool calls follow the AHP state
  machine and are named as soon as they start, with their raw input; diffs are file edits
  the agent reports, listed in the session's changeset, whose before and after contents
  the host serves; usage is reported per turn. Plan updates are not mapped yet.
- Approvals: a session's `approvalMode` is `manual` or `allow-all` (`assisted` is treated
  as `manual`; Orka has no model-reviewed approvals). In `manual`, each turn's Task holds
  every permission request policy would allow for its requester, shown as an AHP
  confirmation; policy answers what it rejects. In `allow-all`, policy decides. Only the
  session owner may answer. Orka never chooses an "always" option on the user's behalf.
- Cancellation answers pending permissions as cancelled, cancels the Task, and kills the
  agent process if the cancel does not settle in time, so late updates cannot reach the
  next turn.
- Live text streams to the session owner with exact-value redaction of the credentials
  Orka injected, matched across chunk boundaries before any fan-out. The journal and
  transcript keep only fully redacted text.

### Host reach

The TCP channel is not advertised. Client-provided tools are never offered to agents.
`resourceRead` serves only the contents a session's changeset lists; nothing on the host
or in a workspace is readable through it. Resource watches are not offered, and
`completions` answers with an empty list.

### Workspaces and terminals

Clients treat the host as a machine with git checkouts: they run their own git commands
in a project's checkout and a session's worktree through AHP terminals, and a client may
refuse to create a session or send a message until those commands work. When an operator
names a workspace image, Orka gives each AHP user one workspace Pod in a dedicated,
restricted namespace:

- the user's projects are cloned at their project paths, without credentials, so only
  repositories readable anonymously can be checked out there;
- each session has a git worktree at its working directory, on the session's branch;
- AHP terminals are shells in the Pod, streamed over the terminal channel.

The Pod holds no credentials, accepts no inbound traffic and reaches only DNS and HTTPS.
Agents never run there: each turn still runs as a Task in its own workspace, publishes to
the session's branch, and the worktree fast-forwards to what it published. A project's
branch belongs to one session. The workspace is ephemeral and is rebuilt from the user's
projects and sessions when it starts again; it is removed with the user's last session
unless a terminal still runs. Terminals are usable only by connections allowed to create
Tasks, and disposing one stops everything its shell started.

### Client dialects

Clients extend the protocol through `_meta` keys and host-specific commands. Orka serves
the Copilot dialect for every connection, because clients do not identify themselves at
`initialize`: the `copilot.projectManagement` root extension and the
`extensions/listProjects`, `extensions/cloneProject` and `extensions/getPlan` commands.
Unknown extension commands are answered with method-not-found.

### Prerequisites in Orka core

Each lands separately and is useful without AHP:

1. Extend the existing Task event pipeline to push text and reasoning deltas, tool calls
   with their name, input and file edits (relative to the workspace), and plan updates,
   redacted as above. A runtime sends the new fields only to a controller that asks for
   them; Session streams keep durable events only.
2. A cancel request for a running Task.
3. Human decisions for built-in runtimes' permission requests.
4. Use the existing session-scoped workspace reuse, with its UID, lineage and fencing,
   for interactive sessions; no new Session or workspace identity.
5. Read access to a session's changed files. The first release serves the before and
   after contents the agent reports with each edit, rather than reading a workspace.
6. AHP connection tokens and identity mapping.

## Consequences

- Orka joins the conversation: every turn is a governed Task with Orka-held Git
  credentials, models through the provider proxy, approvals and an audit trail.
- The endpoint is a large, stateful surface. Client dialects drift with client releases
  and must be tested against the real clients, not only the spec.
- The journal stays in the single API server replica's store until Orka's storage work
  allows more; reconnects then need sticky routing.
- Copilot app users get Orka's runtimes, including Copilot's CLI over ACP, not the
  native `copilotd` experience; that remains ADR 0035's agent hosts.
- The gateway bridge for the Copilot app is no longer needed once this serves chat.

### Alternatives considered

- **Extend the gateway bridge.** The gateway protocol cannot carry what AHP needs.
- **Register Orka itself with GitHub's relay.** That relay's protocol is not public.
- **Keep the journal in memory**, as the reference host does. Breaks reconnect on restart
  and cannot be filtered per user.
- **Depend on a third-party AHP server library.** None is official or mature; the AHP
  repository tracks the request for one.
