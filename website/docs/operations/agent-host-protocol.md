---
title: Agent Host Protocol clients
description: "Let Agent Host Protocol clients, such as the GitHub Copilot desktop app, run sessions on Orka's built-in runtimes."
---

Orka serves the [Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) at `/ahp`. An AHP client, such as the GitHub Copilot desktop app, connects to Orka
as a remote agent host. It sees Orka's built-in runtimes as agents, picks a repository,
and chats. Each message runs as an agent Task in an Orka Session, governed like any other
Task: Orka's policy, credentials, provider proxy and audit trail apply. The design is
[ADR 0034](https://github.com/orka-agents/orka/blob/main/docs/adr/0034-agent-host-protocol-api.md).

## Turn it on

Start the controller in the `harness-v2` execution mode with:

```text
--enable-ahp-api
--oidc-issuer=<issuer> --oidc-audience=<audience> --oidc-allowed-subjects=<patterns>
--ahp-git-credential-secret=<secret name>   # optional
--ahp-workspace-class=<class name>          # optional
```

People mint AHP connection tokens with their OIDC or context-token identity; see the
[API reference](../reference/api-reference.md#agent-host-protocol). ServiceAccounts cannot
mint them.

## What a person sees

- **Agents and models.** Each Agent in the person's namespace that uses a built-in
  runtime and names a model becomes one model of that runtime's agent: for example an
  Agent with `runtime.type: copilot` and `model.name: claude-haiku-5.5`. Models the
  person's token could not run are not offered.
- **Projects.** The person adds a repository by its https URL. Orka does not clone it on
  the host: each turn's Task clones it in its own workspace.
- **Sessions.** A session is an Orka Session. It keeps the model of its first message,
  because an Orka Session is bound to one runtime profile. Start a new session to use
  another model.
- **Turns.** Text, reasoning and tool calls stream while the Task runs. Files the agent
  edits appear in the session's changes, with their before and after contents.
  Cancelling the turn cancels its Task.
- **Approvals.** With `manual` approvals, Orka holds every permission request its policy
  would allow and asks the person; policy still refuses what it refuses. With
  `allow-all`, policy decides alone. `assisted` behaves like `manual`.

## Repositories and credentials

The agent never holds Git credentials. With `--ahp-git-credential-secret`, Orka looks up
that Secret in each person's namespace and uses its `token` key to clone the repository
and publish the agent's changes. Each turn that changes files publishes them to the
session's branch, `orka/session-<uid>`, and the next turn starts from it. Without the
Secret, sessions clone public repositories read-only, and a turn that changes files
fails with `ReadOnlyWorkspaceModified`.

With `--ahp-workspace-class`, turns use session-scoped execution workspaces, so a
session's files stay warm between turns. This needs an installed workspace provider.

## Workspaces and terminals

Clients such as the Copilot app run their own git commands on the host, through
terminals, before they create a session or send a message. Give them a place to do that:

```bash
kubectl apply -k config/ahp-workspaces
```

and start the controller with
`--ahp-workspace-image=<image with sh and git>@sha256:<digest>`. Each AHP user then gets
one workspace Pod in `orka-ahp-workspaces` (`--ahp-workspace-namespace`):

- each project is cloned at its path, and each session has a git worktree on its branch;
- terminals are shells in that Pod;
- the Pod holds no credentials, so only repositories readable without credentials can be
  checked out there, and it reaches only DNS and HTTPS;
- agents never run there: each turn publishes to the session's branch, and the session's
  worktree fast-forwards to it.

The Pod's files are ephemeral: Orka rebuilds the checkouts when it starts again, and
removes the Pod with the person's last session unless a terminal is open.

## Connect the Copilot app

1. Mint a token: `POST /api/v1/ahp-tokens` with your OIDC token.
2. In the app, add a remote agent host of type **AHP (WS)** with the URL
   `wss://<orka-api-host>/ahp?tkn=<token>`.
3. Pick the host, add a repository as a project, choose a model, and send a message.

## Not offered yet

- Resource watches and host file browsing. `resourceRead` serves only the contents in a
  session's changes.
- Workspace checkouts of repositories that need credentials to read.
- Tools the client offers to agents.
- Plan updates.
- Agent hosts as a session backend.
