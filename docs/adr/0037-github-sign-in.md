# ADR 0037: Sign people in with GitHub

Date: 2026-10-21

## Status

Proposed. Adds GitHub as a way for people to sign in to Orka, beside OIDC. `orka login`
uses GitHub's device flow with the operator's GitHub App, and Orka issues its own
short-lived session token. The same login links the person's GitHub account as a
Connection, which provisions their agent host under
[ADR 0036](0036-agent-hosts-from-connections.md). Builds on
[ADR 0033](0033-user-connectors.md). OIDC sign-in is unchanged.

## Context

- The day-0 path we want: an operator runs `helm install`, then each of N people runs one
  CLI command and starts working on GitHub from the Copilot desktop app.
- Today Orka's API accepts OIDC JWTs, ServiceAccount tokens and context tokens. Neither
  the CLI nor the dashboard has a sign-in flow: people paste a token from their identity
  provider.
- GitHub is not an OIDC provider for people; it issues opaque OAuth tokens. A GitHub App
  can enable the device flow, and with expiring user tokens it issues an eight-hour access
  token and a six-month refresh token. Both were confirmed live on 2026-10-21.
- GitHub's check-token API (`POST /applications/{client_id}/token`, Basic auth with the
  app's client ID and secret) confirms that a token was issued to that app and names its
  user. An invalid token gets 404.
- ADR 0036 already needs one GitHub App per installation, for the agent-host provider.

## Decision

### One GitHub App per installation

The operator's GitHub App serves sign-in, the agent-host `ConnectorProvider` and the
hosts' tokens. It has the device flow and expiring user tokens turned on, and its client
ID and secret are the provider's. Keep its permissions narrow: everything it may do, a
signed-in person's host may do.

### `orka login`

1. `GET /api/v1/auth/config` is unauthenticated. It returns the sign-in methods and the
   GitHub App's client ID, and nothing secret.
2. The CLI runs the device flow against GitHub itself. It shows the code and the URL,
   polls GitHub, and never holds the client secret.
3. It sends the GitHub access and refresh tokens to `POST /api/v1/auth/github` once.
   Orka then:
   - checks with the check-token API that the token was issued to its own app, so a
     token another app obtained cannot sign in;
   - takes the identity from GitHub: issuer `https://github.com`, subject the numeric
     user ID (logins can change), username the login;
   - requires active membership of a configured org, or of a configured team
     (`GET /user/memberships/orgs/{org}`, `state: active`). The app must be installed on
     those orgs with read access to members. Any error denies sign-in.
   - issues an Orka session token: random, shown once, stored only as a hash with its
     owner and a twelve-hour expiry, as AHP tokens are.
4. The CLI keeps the session token in the user's configuration directory, readable only
   by them. `orka logout` revokes it.

The API's authentication accepts session tokens beside OIDC. A session's identity is a
verified person, like an OIDC sign-in: it may link accounts and mint AHP tokens, and it
works in the sign-in namespace, which is the watch namespace. Session tokens are never
logged.

### Renewal and offboarding

- Before a session expires, the CLI renews it at `POST /api/v1/auth/session/renew`. Orka
  refreshes the person's GitHub token from custody and checks membership again. A person
  no longer in an allowed org or team cannot renew; they sign in again, and are refused.
- The agent host's token refresh under ADR 0036, at most every eight hours, checks
  membership too. If it is gone, the controller handles it as a disconnect: it stops the
  host and keeps its volume.
- Removing someone from the GitHub org therefore ends their Orka access within one
  session lifetime, and ends their host within one token lifetime.

### Signing in also links GitHub

- `POST /api/v1/auth/github` records the GitHub tokens as the person's Connection to the
  agent-host provider, committed to sealed custody as a consent completion is. It is
  bound to the provider's current consent authority digest. ADR 0036 then provisions the
  person's host.
- Before it shows the device code, the CLI prints ADR 0036's disclosure: signing in
  starts your Copilot environment on Orka, and code running there can read this GitHub
  credential.
- `orka login --no-host` links the account for renewal and connector use, and records
  that the person declined a host. ADR 0036 provisions none for such a Connection.

### Configuration

The chart takes the GitHub App's client ID and a Secret holding its client secret, the
allowed orgs and teams, and whether signing in creates hosts. From these it renders the
`ConnectorProvider` with the agent-host template, and the controller flags.

### Not in this record

- Signing in to the dashboard with GitHub's web flow. The CLI comes first; the dashboard
  can accept a session token from `orka dashboard`.
- GitHub Enterprise Server: this record covers github.com only.
- Mapping GitHub teams to Orka roles beyond allowing or refusing sign-in.

## Acceptance

The ADR 0036 acceptance run is done with two people who sign in with `orka login`. A third
person outside the allowed org is refused. Removing one person from the org then ends
their renewal and stops their host.

## Consequences

- Orka issues and verifies its own session tokens. They are a new credential: hashed at
  rest, short-lived, revocable and never logged.
- Sign-in depends on GitHub's availability and on the app staying installed on the
  allowed orgs.
- One app's permissions serve sign-in, any connector tools its provider declares, and
  hosts.
- Session tokens need a new SQLite table, and so a migration step.
- OIDC deployments are unaffected. Both sign-in methods can be configured at once.

### Alternatives considered

- **Dex, an OIDC broker with a GitHub connector, in front of Orka's OIDC.** It needs no
  sign-in code in Orka, but it is another component to deploy and run. People would still
  need an OIDC CLI login and a separate `orka connect`.
- **Raw GitHub tokens as Orka bearer tokens.** Orka would issue no token of its own, but
  every request would cost a GitHub call, and the CLI would hold a credential with
  repository access.
- **Checking membership with the app's installation token.** That needs the app's private
  key in Orka. The person's own token is enough.
