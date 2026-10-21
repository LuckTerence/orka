# ADR 0036: Provision agent hosts from GitHub Connections

Date: 2026-10-21

## Status

Proposed. An operator can mark one GitHub `ConnectorProvider` as the source of agent
hosts. A person who connects it gets an `AgentHost` from the provider's template, and Orka
supplies the host's GitHub token from the Connection, instead of a Secret the person
creates. This amends [ADR 0033](0033-user-connectors.md) with one narrow exception to its
custody rule, and [ADR 0035](0035-agent-hosts.md) with a second token source. It depends on
0035. Accepting it waits on the live check under **Acceptance**.

## Context

- The day-0 path we want is: install Orka, sign in to the dashboard, connect GitHub, then
  pick the environment in the Copilot desktop app. With 0035 alone, a person needs
  `kubectl`, RBAC in a namespace, and a raw GitHub token pasted into a Secret.
- 0033 already gives each person a consented, sealed and refreshed GitHub link, owned by
  their verified identity (issuer and subject). Its rule is that linked tokens never enter
  a Pod.
- copilotd needs its GitHub token inside its own process: it reads the token at start for
  relay registration, and later from a file it watches for replacements. Nothing outside
  the Pod can stand in for it. Every process in the host, including the agent, can read
  that token; 0035 accepts this for a single trusted user.
- In relay mode the client brings its own GitHub credential for each session. The host's
  token registers the environment with GitHub's Mission Control, enforces the single-user
  check and makes host-level GitHub calls.
- On 2026-10-21, copilotd 0.9.6 in relay mode accepted a GitHub App user access token
  (`ghu_`, eight-hour expiry, no OAuth scopes) from an operator's GitHub App: identity
  resolution, Mission Control registration and the relay connections all succeeded.
  Before that, only `gh` CLI OAuth tokens had been tried.
- GitHub App user tokens expire after eight hours, and refresh tokens after six months.
  [Once a refresh token is used](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens),
  it and the old access token stop working. So a refresh made for one purpose cuts off
  every other holder of the old access token.
- Today's code does not fit this as is:
  - The credential source serves tool calls only. It needs a tool binding and a frozen
    Connection.
  - AgentHost admission authorizes the Kubernetes caller, while a Connection belongs to an
    identity issuer and subject.
  - The Connection reconciler never refreshes ahead of expiry.
  - Deleting an AgentHost deletes its volume.

## Decision

### The operator opts one provider in, and consent covers it

- `ConnectorProvider.spec.agentHost.templateRef` names an `AgentHostTemplate` in the
  provider's namespace. That provider's Connections get hosts. Providers are operator
  resources, so only an operator can make this choice.
- At most one provider per namespace may set it. If several do, none provisions, and each
  reports the conflict.
- The field is part of the provider's consent authority digest. Links consented before
  the field was set stop matching and show "Reconnect needed", so every exported token
  comes from a consent that disclosed the export.
- On the Connectors page, that provider says that connecting it creates a Copilot
  environment on Orka, and that code running there can read this GitHub credential. Once
  the host exists, the page shows the environment name and the host's phase. It says
  "Host ready", never "Registered": Pod Ready proves the AHP listener answers, not that
  GitHub has registered the environment.
- The provider declares no OAuth scopes, because GitHub App user tokens carry none.
- A dedicated GitHub App, with no tools on its provider and only the repository
  permissions the host's own calls need, is recommended but not required. A provider that
  also serves connector tools works, but its host tokens then carry those tools'
  permissions, and its tool refreshes cut off the host's token (see below).

### The controller creates one host per person

- A Connection to that provider gets a host once it is linked:
  - it is Ready and its consent matches the provider's current digest;
  - its grant is committed;
  - it is the only link its subject has to that provider.
- The controller then creates one AgentHost in the Connection's namespace, named `gh-` plus
  the lowercased GitHub login. The client shows it as `orka-gh-<login>`. The login comes
  from `GET /user` with the first token.
- If that name is already taken by an AgentHost not created for this person, nothing is
  provisioned, and the conflict is shown next to the link.
- The host's spec gets the provider's `templateRef` and
  `githubTokenFrom.connectionRef.name`: the Connection's canonical name, which derives
  from the provider and the owner's issuer and subject.
- `githubTokenFrom` is immutable, and it excludes `githubTokenSecretRef`. Hosts that use a
  Secret keep working unchanged.
- Admission accepts `githubTokenFrom` only on create, and only from the controller's own
  ServiceAccount. The named Connection's provider must set `agentHost` with the same
  template. Naming the template on the provider is the operator's grant, so this path
  skips the template `use` check. No other caller can create a host fed by a Connection.
- Because the host is bound to the Connection's canonical name, a person who disconnects
  and reconnects gets the same host back.
- The first export records the GitHub account's numeric ID in the host's status. A later
  token for a different account fails the host with `GitHubIdentityChanged`, so it never
  serves the existing volume to another account.
- No owner reference runs from a Connection to its host. Deleting a Connection never
  deletes a volume.
- These hosts follow their template by name, like any other host: recreating the template
  under its name moves them to it on their volumes (0035). Pointing the provider at a
  different template affects only hosts created afterwards, because `templateRef` is
  immutable.

### Orka hands the host access tokens only, and keeps them fresh

- A controller-only host credential resolver sits beside the tool path. It shares that
  path's single-flight refresh, provider and consent checks, and custody fencing. It
  returns the access token, its expiry and the custody version, and it needs no Task, tool
  binding or requester stamp.
- The AgentHost reconciler writes the access token into the host's existing token copy in
  the host namespace, which is the 0035 Secret. The refresh token never leaves sealed
  custody, and no token Secret appears in the person's namespace.
- The host refreshes 15 minutes before expiry.
- A rotation committed for any other reason, such as a tool call, notifies the host
  reconciler. A recheck of the custody version every five minutes repairs a lost
  notification.
- copilotd picks up the replaced file. A brief relay reconnect during rotation is
  accepted.
- None of this needs SQLite DDL: the pinned account ID lives in AgentHost status.

### Disconnect stops the host and keeps its volume

- These conditions make the Connection unusable:
  - it is deleted, or no longer Ready;
  - its consent is stale;
  - its provider no longer names this template;
  - a refresh fails for good.
- When that happens, the controller stops the Pod, confirms it is gone, deletes the token
  copy and keeps the AgentHost and its volume. The host is `Failed` with
  `ConnectionUnavailable`.
- This check runs before suspension, so a suspended host loses its token copy too, and
  waking it cannot bypass the check. Reconnecting with the same GitHub account restarts the
  host on its volume.
- Deleting the AgentHost still deletes its volume, as in 0035. That stays an
  administrator's action.
- Disconnecting deletes Orka's copy of the token, as in 0033. Revoking the grant through
  GitHub's app-authenticated endpoints is a follow-up.
- Removing a person from the identity provider does not delete their Connection. To
  offboard someone, an administrator deletes the Connection or the host.

### What a cluster needs

- The existing Connectors install (OIDC sign-in, a callback URL, a durable store and its
  key), plus `agentHosts.enabled`.
- A GitHub App with expiring user tokens and Orka's callback URL.
- A sample provider and template that the operator applies once. Rendering them from Helm
  values is a follow-up.

### Not in this record

- Waking a suspended host from the client, and a per-person choice of whether to have a
  host.
- More than one host provider or template per namespace.

## Acceptance

Before this is accepted, one live run must pass:
1. Install with Helm on a fresh cluster, sign in to the dashboard and connect GitHub.
2. `orka-gh-<login>` appears in the Copilot app.
3. A turn that clones a private repository completes.
4. Force a token refresh, then complete another turn.
5. Disconnect: the host stops and its token copy is gone.
6. Reconnect: the host resumes on the same volume.

That run also settles which token copilotd uses to clone: the host's, or the client's.

## Consequences

- 0033's rule gains one named exception. Access tokens from an agent-host provider reach
  that person's own host, where the host and its agent can read them.
  - A Connection's `readOnly` mode and Orka's tool approvals do not limit what the host
    does with the token.
  - The refresh token still never leaves custody.
- A host's token carries the GitHub App's permissions intersected with the person's own
  access. A narrow app limits it; copying the token cannot.
- A tool call's refresh cuts off the host's token until the host's copy is rewritten. A
  dedicated provider avoids this.
- Enforcement depends on the controller. If it stays down past an access token's expiry,
  the host cannot refresh, but relay sessions that are already open are not guaranteed to
  end.
- Environment names come from GitHub logins, so a renamed account keeps its old name.

### Alternatives considered

- **Secrets only (0035).** This is proven and needs no exception, but it costs `kubectl` and
  a pasted long-lived token. It stays as the escape hatch.
- **A "Create agent host" button or API.** It adds an action, and authorization for its
  lifecycle, without changing what runs. Deferred.
- **A person's own YAML naming their Connection.** Admission would have to map a
  Kubernetes caller to an identity subject. It adds nothing on day 0.
- **Keeping the token out of the Pod with an injecting proxy.** copilotd needs a real token
  at start, and it talks to GitHub over TLS, so a proxy would have to intercept TLS.
  Rejected.
- **Long-lived tokens through Connections (OAuth App tokens or PATs).** They would need no
  refresh, but they would put non-expiring tokens in Pods. Expiring GitHub App tokens are
  preferred.
