#!/usr/bin/env bash
# Deploys, activates, and retires the runtimes of wrapped kagent Agents, from
# the deployment bundles "orka agent import kagent --bundle-dir" writes.
#
# A bundle names its runtime after the configuration it pins, so re-pinning an
# Agent (after kagent recompiles it, or after an upgrade) deploys a new
# runtime beside the old one. Orka cannot change a registered AgentRuntime's
# profile in place, and the old runtime keeps serving its sessions until it is
# retired.
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "${script_dir}/.." && pwd)"

usage() {
  cat <<'USAGE'
Usage:
  scripts/kagent-runtime.sh deploy <bundle>
      Build and push the runtime's images, deploy it, and register it with
      Orka under Kubernetes recovery, so Orka restarts it after an Orka
      controller restart. Waits until Orka reports the AgentRuntime Ready. A governed
      Agent's relay moves to the new runtime before registration, because
      Orka's conformance prompt calls it; until activation, governed turns on
      the old runtime fail their tool and model calls. If deploy fails or is
      interrupted before the runtime is Ready, the relay moves back.
  scripts/kagent-runtime.sh activate <bundle> -- <import arguments>
      Point the Orka Agents at their runtimes by re-running the import without
      --dry-run, with the same arguments that wrote the bundles; the import
      refuses arguments that pin other bundles. The script passes
      --bundle-dir, the directory of <bundle>, itself. It switches every Agent in
      its input, and only if all of their runtimes are Ready, so deploy them
      all first. Refuses while the relay of a governed Agent bundled beside
      <bundle> selects another runtime.
  scripts/kagent-runtime.sh retire <bundle>
      Delete the Agent's other runtimes. Orka drains each one first, so this
      waits until their sessions are done. Refuses while a governed Agent's
      relay selects another runtime. Running it again finishes an earlier
      retire that stopped.

<bundle> is a directory the import wrote, <bundle-dir>/<agent>.

Environment:
  KUBECTL_CONTEXT            kubectl context (required)
  NAMESPACE                  namespace of the runtimes (default orka-system)
  CONTROLLER_NAMESPACE       namespace of the Orka controller (default NAMESPACE)
  ORKA                       orka CLI (default: orka)
  WRAPPER_SRC                orka-runtime-kagent checkout (deploy)
  IMAGE_REPO                 image repository prefix, such as ghcr.io/me (deploy)
  BUILD_PLATFORM             image platform (default linux/amd64)
  MODEL_UPSTREAM_TOKEN_FILE  file holding Orka's credential for a governed
                             Agent's model upstream (deploy, governed)
  STORAGE_CLASS              StorageClass of the broker ledger (default: cluster default)
  ORKA_API_URL               Orka API URL the supervisor calls
  SUBSTRATE_NAMESPACE        kagent v1 Substrate namespace (default ate-system)
  READY_TIMEOUT              wait for a runtime to be Ready (default 5m)
  RETIRE_TIMEOUT             wait for an old runtime to drain (default 30m)
USAGE
}

die() {
  echo "kagent-runtime: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

kc() {
  kubectl --context "${KUBECTL_CONTEXT}" -n "${NAMESPACE}" "$@"
}

# Rendered objects carry their own namespaces: a v0 runtime's pod reader lives
# in the kagent Agent's namespace.
apply() {
  kubectl --context "${KUBECTL_CONTEXT}" apply -f -
}

# tostring keeps a false field from failing jq -e.
bundle_field() {
  jq -er --arg field "$1" '.[$field] | tostring' "${bundle}/runtime.json"
}

render() {
  local part=$1
  shift
  local args=(agent-runtime render-kagent "${bundle}" -n "${NAMESPACE}" --part "${part}"
    --configured-image "${configured_image}" --runtime-image "${runtime_image}")
  [ -n "${STORAGE_CLASS:-}" ] && args+=(--storage-class "${STORAGE_CLASS}")
  [ -n "${ORKA_API_URL:-}" ] && args+=(--orka-api-url "${ORKA_API_URL}")
  [ -n "${SUBSTRATE_NAMESPACE:-}" ] && args+=(--substrate-namespace "${SUBSTRATE_NAMESPACE}")
  "${ORKA}" "${args[@]}" "$@"
}

# The images a runtime runs are recorded on its Deployment, so activate and
# retire need no rebuild.
deployed_images() {
  configured_image=$(kc get deployment "${runtime}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="broker")].image}')
  runtime_image=$(kc get deployment "${runtime}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="supervisor")].image}')
  [ -n "${configured_image}" ] && [ -n "${runtime_image}" ] || die "runtime ${runtime} is not deployed; run deploy first"
}

build() {
  local tag=$1 metadata=$2
  shift 2
  docker buildx build --platform "${BUILD_PLATFORM}" --push --metadata-file "${metadata}" -t "${tag}" "$@" >&2
  jq -er '."containerimage.digest"' "${metadata}"
}

relay_service_of() {
  jq -er '.relay' "$1/runtime.json" | sed -E 's#^http://([^.]+)\..*#\1#'
}

# relay_selector prints a relay Service's selector, or nothing if there is no
# such Service. Any other lookup failure fails.
relay_selector() {
  kc get service "$1" --ignore-not-found -o json | jq -c '.spec.selector // {}'
}

# registration_of prints the AgentRuntime's name if it is registered. Any
# lookup failure but NotFound fails.
registration_of() {
  kc get agentruntime "$1" --ignore-not-found -o name
}

same_bundle_dir() {
  [ -d "$1" ] && [ "$(cd "$1" && pwd -P)" = "$2" ]
}

# Until a deployed runtime is Ready, any failure, or an interrupt, moves its
# Agent's relay back to the runtime it selected before.
#
# The restore applies only while the relay still selects this runtime: a
# concurrent deploy of the Agent may have moved it since.
restore_relay() {
  [ -n "${relay_rollback}" ] || return 0
  echo "Runtime ${runtime} is not Ready; moving relay ${relay_service} back." >&2
  kc patch service "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec/selector\",\"value\":{\"orka.ai/kagent-runtime\":\"${runtime}\"}},{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":${relay_rollback}}]" >/dev/null ||
    echo "kagent-runtime: relay ${relay_service} was not moved back to ${relay_rollback}; it no longer selects ${runtime}, or the patch failed" >&2
}

# require_relay refuses unless a governed bundle's relay selects the bundle's
# runtime, which otherwise cannot make its tool and model calls.
require_relay() {
  local b=$1 governed_bundle service want got
  governed_bundle=$(jq -er '.governed | tostring' "$b/runtime.json")
  [ "${governed_bundle}" = true ] || return 0
  service=$(relay_service_of "$b")
  want=$(jq -cn --arg runtime "$(jq -r '.runtime' "$b/runtime.json")" '{"orka.ai/kagent-runtime": $runtime}')
  got=$(relay_selector "${service}")
  [ "${got}" = "${want}" ] ||
    die "relay ${service} selects ${got:-nothing}, not ${want}; move it with: $0 deploy ${b}"
}

# The epoch record lives in the Orka controller's namespace, which several
# Orka installations in one cluster each have.
controller_epoch() {
  kubectl --context "${KUBECTL_CONTEXT}" -n "${CONTROLLER_NAMESPACE}" get cepoch -o json |
    jq -er '[.items[] | select(.spec.name == "orka-controller") | .status.epoch] |
      if length == 1 and (.[0] | type == "number" and . > 0)
      then .[0] else error("expected one initialized controller epoch") end'
}

# The auth Secret is bound to one registration and endpoint. Its values are
# random and never printed; a governed Agent's upstream credential comes from
# MODEL_UPSTREAM_TOKEN_FILE.
ensure_auth_secret() {
  local secret="${runtime}-auth" found key
  found=$(kc get secret "${secret}" --ignore-not-found -o name)
  if [ -z "${found}" ]; then
    # The values go through files in the private work directory, never
    # through a command line.
    local args=()
    for key in controller-token capability-secret provider-token; do
      openssl rand -hex 32 | tr -d '\n' >"${work}/${key}"
      args+=(--from-file="${key}=${work}/${key}")
    done
    if [ "${governed}" = true ]; then
      [ -r "${MODEL_UPSTREAM_TOKEN_FILE:-}" ] || die "a governed Agent needs MODEL_UPSTREAM_TOKEN_FILE"
      # The broker refuses a credential with control characters, such as the
      # newline a file usually ends with.
      tr -d '\r\n' <"${MODEL_UPSTREAM_TOKEN_FILE}" >"${work}/model-upstream-token"
      args+=(--from-file=model-upstream-token="${work}/model-upstream-token")
    fi
    kc create secret generic "${secret}" "${args[@]}" >/dev/null
  fi
  # Orka uses the Secret only with this metadata, which a deploy that stopped
  # after creating it did not set.
  kc label --overwrite secret "${secret}" orka.ai/agent-runtime-auth=true orka.ai/agent-runtime-name="${runtime}" \
    orka.ai/kagent-runtime="${runtime}" orka.ai/kagent-agent="${agent}" >/dev/null
  kc annotate --overwrite secret "${secret}" \
    orka.ai/agent-runtime-endpoint="http://${runtime}.${NAMESPACE}.svc.cluster.local:8080" >/dev/null
}

deploy() {
  need openssl
  if [ "${governed}" = true ]; then
    # The relay Service's name comes from the Agent's relay URL, which an
    # Agent imported separately may share.
    local relay_agent
    relay_service=$(relay_service_of "${bundle}")
    local relay_found
    relay_found=$(kc get service "${relay_service}" --ignore-not-found -o name)
    relay_agent=$(kc get service "${relay_service}" --ignore-not-found -o jsonpath='{.metadata.labels.orka\.ai/kagent-agent}')
    if [ -n "${relay_found}" ] && [ "${relay_agent}" != "${agent}" ]; then
      [ -n "${relay_agent}" ] && die "relay ${relay_service} serves Agent ${relay_agent}; give ${agent} its own relay URL"
      die "Service ${relay_service} exists but is not a relay of this script; to let ${agent} use it, label it orka.ai/kagent-agent=${agent}"
    fi
  fi
  # Only NotFound means the runtime is not deployed yet: rebuilding a deployed
  # runtime would roll new images into it under its registered profile.
  local deployed
  deployed=$(kc get deployment "${runtime}" --ignore-not-found -o name)
  if [ -z "${deployed}" ]; then
    [ -d "${WRAPPER_SRC:-}" ] || die "WRAPPER_SRC must name an orka-runtime-kagent checkout"
    grep -qE '^FROM .* AS config$' "${WRAPPER_SRC}/Dockerfile" ||
      die "${WRAPPER_SRC} predates baking a bundle (its Dockerfile has no config stage); update the checkout"
    [ -n "${IMAGE_REPO:-}" ] || die "IMAGE_REPO is required"
    need docker
  fi
  local digest
  # The directory may hold the governed upstream credential, so it goes on
  # every exit, with the relay rollback.
  work=$(mktemp -d)
  trap 'restore_relay; rm -rf "${work}"' EXIT
  trap 'exit 1' INT TERM HUP
  if [ -n "${deployed}" ]; then
    # The images are part of the runtime's immutable profile. A deployed
    # runtime keeps its images; roll new ones under a new name with the
    # import's --runtime-revision.
    deployed_images
    echo "Runtime ${runtime} is already deployed; reusing its images." >&2
  else
    # The wrapper's Dockerfile reads the configuration from its "config"
    # stage, which this replaces with the bundle.
    digest=$(build "${IMAGE_REPO}/kagent-configured:${runtime}" "${work}/configured.json" \
      --build-context config="${bundle}" "${WRAPPER_SRC}")
    configured_image="${IMAGE_REPO}/kagent-configured@${digest}"
    digest=$(build "${IMAGE_REPO}/kagent-runtime:${runtime}" "${work}/runtime.json" \
      -f "${repo_dir}/workers/acp/images/kagent/Dockerfile" \
      --build-arg KAGENT_RUNTIME_IMAGE="${configured_image}" \
      --build-arg KAGENT_ADAPTER_DIGEST="${configured_image##*@}" "${repo_dir}")
    runtime_image="${IMAGE_REPO}/kagent-runtime@${digest}"
  fi

  ensure_auth_secret
  render workload --controller-epoch "$(controller_epoch)" | apply
  kc rollout status "deployment/${runtime}" --timeout="${READY_TIMEOUT}"
  if [ "${governed}" = true ]; then
    # Orka's conformance prompt reaches kagent, which calls the relay, so the
    # relay must reach this runtime before registration.
    local previous want
    previous=$(relay_selector "${relay_service}")
    want="{\"orka.ai/kagent-runtime\":\"${runtime}\"}"
    render relay >"${work}/relay.json"
    jq '.items |= map(select(.kind == "NetworkPolicy"))' "${work}/relay.json" | apply
    if [ -z "${previous}" ]; then
      jq '.items |= map(select(.kind == "Service"))' "${work}/relay.json" | apply
    elif [ "${previous}" != "${want}" ]; then
      # Apply the Service with what it selects now, so applying moves
      # nothing; then hand it over in one patch that holds only while it
      # still selects that, and replaces the selector outright, since a merge
      # with an older one would select nothing.
      jq --argjson selector "${previous}" '.items |= map(select(.kind == "Service") | .spec.selector = $selector)' \
        "${work}/relay.json" | apply
      relay_rollback=${previous}
      kc patch service "${relay_service}" --type=json -p "[{\"op\":\"test\",\"path\":\"/spec/selector\",\"value\":${previous}},{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":${want}}]" >/dev/null ||
        {
          relay_rollback=""
          die "relay ${relay_service} changed while this deploy ran; deploy again"
        }
    fi
  fi
  # The API server's service proxy reaches the runtime's public endpoint.
  kubectl --context "${KUBECTL_CONTEXT}" get --raw \
    "/api/v1/namespaces/${NAMESPACE}/services/http:${runtime}:control/proxy/v2/capabilities" >"${work}/capabilities.json"
  local deployment_uid registered owner
  deployment_uid=$(kc get deployment "${runtime}" -o jsonpath='{.metadata.uid}')
  render registration --capabilities "${work}/capabilities.json" --deployment-uid "${deployment_uid}" | apply
  # Orka recovers the Deployment, restarting it after an Orka controller
  # restart, once the Deployment names the AgentRuntime.
  registered=$(kc get agentruntime "${runtime}" -o jsonpath='{.metadata.uid}')
  owner=$(kc get deployment "${runtime}" -o jsonpath='{.metadata.annotations.orka\.ai/agent-runtime-recovery-uid}')
  if [ -z "${owner}" ]; then
    kc annotate deployment "${runtime}" "orka.ai/agent-runtime-recovery-uid=${registered}" >/dev/null
  elif [ "${owner}" != "${registered}" ]; then
    die "deployment ${runtime} is recovered by another AgentRuntime (${owner})"
  fi
  # Activation requires this registration, so it cannot switch Agents through
  # an Orka API in another cluster.
  printf '%s\n' "${registered}" >"${bundle}/registration.uid"
  kc wait --for=condition=Ready "agentruntime/${runtime}" --timeout="${READY_TIMEOUT}" ||
    die "runtime ${runtime} did not become Ready"
  relay_rollback=""
  echo "Runtime ${runtime} is Ready. Activate it with: $0 activate ${bundle} -- <the import arguments>"
}

activate() {
  [ "$#" -gt 0 ] || die "activate needs the import arguments after --"
  # The import activates the bundles in this bundle's directory, the ones the
  # relay checks below cover, so the script names that directory itself.
  local bundle_dir arg pending=false args=()
  bundle_dir=$(cd "$(dirname "${bundle}")" && pwd -P)
  for arg in "$@"; do
    if [ "${pending}" = true ]; then
      pending=false
      same_bundle_dir "${arg}" "${bundle_dir}" || die "--bundle-dir ${arg} is not ${bundle_dir}, the directory of ${bundle}"
      continue
    fi
    case "${arg}" in
    --dry-run | --dry-run=*) die "activate runs the import without --dry-run" ;;
    --bundle-dir) pending=true ;;
    --bundle-dir=*)
      same_bundle_dir "${arg#--bundle-dir=}" "${bundle_dir}" ||
        die "${arg} is not ${bundle_dir}, the directory of ${bundle}"
      ;;
    *) args+=("${arg}") ;;
    esac
  done
  [ "${pending}" = false ] || die "--bundle-dir needs a directory"
  # The import switches every Agent in its input, which wrote the bundles
  # beside this one, and refuses unless Orka reports all of their runtimes
  # Ready. A governed Agent's relay may have moved since, to a newer runtime.
  local b
  for b in "$(dirname "${bundle}")"/*/; do
    if [ -f "${b}runtime.json" ]; then
      require_relay "${b%/}"
    fi
  done
  "${ORKA}" agent import kagent "${args[@]}" --bundle-dir "${bundle_dir}" -n "${NAMESPACE}"
  [ "$(kc get agents.core.orka.ai "${agent}" -o jsonpath='{.spec.runtime.runtimeRef.name}')" = "${runtime}" ] ||
    die "Agent ${agent} does not use ${runtime}; were the import arguments the ones that wrote this bundle?"
  echo "Agent ${agent} uses ${runtime}. Retire its other runtimes with: $0 retire ${bundle}"
}

retire() {
  [ "$(kc get agents.core.orka.ai "${agent}" -o jsonpath='{.spec.runtime.runtimeRef.name}')" = "${runtime}" ] ||
    die "Agent ${agent} does not use ${runtime} yet; activate it first"
  # The relay may select a runtime this would delete.
  require_relay "${bundle}"
  # A runtime's objects may outlive its registration when an earlier retire
  # timed out or was interrupted, so they name runtimes to retire too.
  local old olds present
  olds=$({
    kc get agentruntimes.core.orka.ai -l "orka.ai/kagent-agent=${agent}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
    kc get deployment,service,persistentvolumeclaim,serviceaccount,secret -l "orka.ai/kagent-agent=${agent}" \
      -o jsonpath='{range .items[*]}{.metadata.labels.orka\.ai/kagent-runtime}{"\n"}{end}'
  } | sort -u)
  for old in ${olds}; do
    [ "${old}" = "${runtime}" ] && continue
    present=$(registration_of "${old}")
    if [ -n "${present}" ]; then
      echo "Retiring ${old}; Orka drains it first." >&2
      kc delete agentruntime "${old}" --wait=false
      kc wait --for=delete "agentruntime/${old}" --timeout="${RETIRE_TIMEOUT}"
    fi
    # The objects serve the registration until it is gone.
    present=$(registration_of "${old}")
    [ -z "${present}" ] || die "AgentRuntime ${old} is still registered"
    kc delete deployment,service,persistentvolumeclaim,serviceaccount,secret -l "orka.ai/kagent-runtime=${old}"
    kubectl --context "${KUBECTL_CONTEXT}" delete role,rolebinding -A \
      -l "orka.ai/kagent-runtime=${old},orka.ai/kagent-runtime-namespace=${NAMESPACE}"
  done
}

[ "$#" -ge 2 ] || {
  usage >&2
  exit 2
}
command=$1
bundle=${2%/}
shift 2
[ -f "${bundle}/runtime.json" ] && [ -f "${bundle}/kagent.json" ] || die "${bundle} is not a deployment bundle"
[ -n "${KUBECTL_CONTEXT:-}" ] || die "KUBECTL_CONTEXT is required"
NAMESPACE=${NAMESPACE:-orka-system}
CONTROLLER_NAMESPACE=${CONTROLLER_NAMESPACE:-${NAMESPACE}}
ORKA=${ORKA:-orka}
BUILD_PLATFORM=${BUILD_PLATFORM:-linux/amd64}
READY_TIMEOUT=${READY_TIMEOUT:-5m}
RETIRE_TIMEOUT=${RETIRE_TIMEOUT:-30m}
need jq
need kubectl
runtime=$(bundle_field runtime)
agent=$(bundle_field agent)
governed=$(bundle_field governed)
relay_service=""
relay_rollback=""
work=""

case "${command}" in
deploy) deploy ;;
activate)
  [ "${1:-}" = -- ] && shift
  activate "$@"
  ;;
retire) retire ;;
*)
  usage >&2
  exit 2
  ;;
esac
