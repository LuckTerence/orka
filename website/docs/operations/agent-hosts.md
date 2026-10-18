---
description: "Run a long-lived, single-user agent host, such as an Agent Host Protocol daemon, on Orka-owned Pods."
---

# Agent hosts

An `AgentHost` runs one long-lived agent host for one user: a daemon that serves the
[Agent Host Protocol](https://microsoft.github.io/agent-host-protocol/specification/overview.html)
(AHP) to clients such as the GitHub Copilot desktop app. Sessions, tools, approvals and
worktrees stay inside the host; the client is a UI. Orka admits the host, runs it from an
operator's `AgentHostTemplate`, restarts it, suspends and wakes it, and cleans it up. The
design is [ADR 0035](https://github.com/orka-agents/orka/blob/main/docs/adr/0035-agent-hosts.md).

Each host is one Pod, a PersistentVolumeClaim for its state, a NetworkPolicy and Secrets in
a dedicated host namespace. Orka does not see the host's turns; it governs who may run a
host, what it runs, what it may reach and the policy pinned in its template.

:::warning[One trusted user per host]
- The sample host, `copilotd`, serves exactly one GitHub user per process.
- The host's agent holds the user's GitHub token, and every process in the Pod runs as
  one uid, so code the agent runs can read it. Egress to TCP 443 is open. Run hosts only
  for code the user trusts; "public repositories only" is not a sandbox boundary.
:::

## Turn it on

1. Create the host namespace and the controller's Role in it. The Kustomize install binds
   the controller's manager role per namespace:

   ```bash
   kubectl apply -k config/agent-hosts
   ```

2. Start the controller with:

   ```text
   --enable-agent-hosts
   --agent-host-namespace=orka-agent-hosts
   --agent-host-agent-image=<controller image>@sha256:<digest>
   --agent-host-controller-pod-labels=control-plane=controller-manager,app.kubernetes.io/name=orka
   ```

   The agent image is normally the controller image itself; it carries the
   `orka-host-agent` sidecar. The Pod labels select the controller Pods allowed to read
   host status through the NetworkPolicy.

3. Create an `AgentHostTemplate` (see
   [`config/samples/core_v1alpha1_agenthosttemplate.yaml`](https://github.com/orka-agents/orka/blob/main/config/samples/core_v1alpha1_agenthosttemplate.yaml))
   and grant `use` on it to the people who may run hosts:

   ```yaml
   apiVersion: rbac.authorization.k8s.io/v1
   kind: Role
   metadata:
     name: agent-host-user
   rules:
     - apiGroups: ["core.orka.ai"]
       resources: ["agenthosttemplates"]
       resourceNames: ["copilotd"]
       verbs: ["use"]
     - apiGroups: ["core.orka.ai"]
       resources: ["agenthosts"]
       verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
   ```

Creating an `AgentHost` requires `use` on its template, and naming a token Secret requires
`get` on that Secret, both checked against the caller by an admission policy.

## Run a host

The sample template runs `copilotd` in relay mode: it registers the host as an environment
on the user's GitHub account and accepts clients through GitHub's relay. The cluster needs
no inbound path.

```bash
kubectl create secret generic alice-github-token --from-literal=token=<GitHub token>
kubectl apply -f config/samples/core_v1alpha1_agenthost.yaml
kubectl get agenthost alice
```

When the host is `Ready`, the environment `orka-alice` appears in the client's
**Environments** list. The token's owner is the only GitHub user the host serves. Orka
copies the token into the host namespace and keeps the copy current, so rotating the
Secret reaches the running host without a restart.

### Standalone access

A template with `access: Standalone` skips the relay. The client port-forwards to the
host's loopback listener and presents the host's connection token, which Orka writes to
the Secret named in `status.connectionSecretRef`:

```bash
ns=$(kubectl get secret alice-connection -o jsonpath='{.data.namespace}' | base64 -d)
pod=$(kubectl get secret alice-connection -o jsonpath='{.data.pod}' | base64 -d)
port=$(kubectl get secret alice-connection -o jsonpath='{.data.port}' | base64 -d)
kubectl -n "$ns" port-forward "pod/$pod" "8788:$port"
```

Add the host as `ws://127.0.0.1:8788/?tkn=<token>`. Port-forward needs
`pods/portforward` on that Pod.

## The host contract

Orka adds to the template's container named `host`, and promises nothing else:

| Variable | Meaning |
| --- | --- |
| `ORKA_HOST_ID` | The AgentHost UID, stable for the host's life. |
| `ORKA_HOST_NAME` | The AgentHost name, for display. |
| `ORKA_CONNECTION_TOKEN_FILE` | A per-host random token in a memory-backed volume mounted only into `host`. Read it once and delete it. |
| `ORKA_GITHUB_TOKEN_FILE` | The user's GitHub token, read-only, when the template's `githubToken` allows it. |
| `ORKA_STATUS_DIR` | A directory shared with the Orka host agent for the idle status file. |

The host must serve AHP on `127.0.0.1:<listenPort>`, gated by the connection token, and
keep everything that must survive a restart under `/workspace`. When the template sets
`idleStatusFile`, the host writes JSON with at least `idle` and `idleSince` there;
copilotd's `--idle-status-file` format fits.

The `orka-host-agent` sidecar answers the host's probes. The Pod is Ready only after an AHP
`initialize` and `ping` succeed against the loopback listener; the check never sends
`authenticate`, so it cannot claim a single-user host. Afterwards it only checks that the
listener accepts connections, so it never keeps the host from being idle.

## Restarts, suspension and wake

- A host Pod that terminates, or whose listener stays down, is replaced with a new one
  on the same volume. A turn that was running is lost; sessions resume from the volume.
- `spec.suspended: true` stops the Pod and keeps the volume and Secrets. Setting it back
  to `false` starts the host again.
- A template's `idleSuspend.after` suspends hosts that report idle for that long. A
  client cannot wake a suspended relay host; set `spec.wakeRequestedAt` to the current
  time to start it:

  ```bash
  kubectl patch agenthost alice --type merge \
    -p "{\"spec\":{\"wakeRequestedAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}"
  ```

Hosts stay on unless a template enables idle suspension.

## Troubleshooting

| `Ready` reason | Meaning |
| --- | --- |
| `Provisioning` | The Pod is starting. The message names a container waiting on an image or volume. |
| `Restarting` | The host's Pod failed; Orka replaces it after a backoff that grows from 10 seconds to 5 minutes. |
| `TemplateNotFound` | The template does not exist in the AgentHost's namespace. |
| `InvalidTemplate` | The template breaks a rule in the message. |
| `GitHubTokenRequired` / `GitHubTokenUnavailable` | The template needs a token and the referenced Secret or key is missing. |
| `ObjectCollision` | An object with a host's name exists in the host namespace but was not created for it. Orka never adopts or deletes it. |
| `StateVolumeLost` | The host's volume was deleted. Delete and recreate the AgentHost. |
| `HostAgentUnavailable` | The controller has no `--agent-host-agent-image`. |
| `HostNamespaceChanged` | The controller's `--agent-host-namespace` changed after the host was created. Delete and recreate it. |

A host that fails for a missing template or token has its Pod stopped and keeps its
volume; when the token's Secret is gone, Orka also deletes its copy.

Deleting an AgentHost removes every object Orka created for it, and only those.
