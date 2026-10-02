#!/usr/bin/env bash
#
# Start a complete sandboxlab environment on a GitHub runner: a kind
# cluster, an Istio gateway, the control plane from this commit, and a tunnel to
# reach it through.
#
# The whole environment, in order. Nothing here is specific to GitHub Actions —
# the workflow file is — so this is also how the environment is built by hand.
#
# The control plane is built from the working copy and loaded into the cluster
# rather than pulled. That is the point of a sandboxlab environment: what comes up
# is the commit under test, and a published `latest` would test yesterday's.
#
# Set SANDBOXLAB_PUBLIC_HOST to skip the tunnel and use a hostname you
# already have (CI does this, so a build never depends on a public tunnel being
# granted).
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

# ── inputs ──────────────────────────────────────────────────────────────────

: "${SANDBOXLAB_TUNNEL:=cloudflare}"
: "${SANDBOXLAB_DOMAIN:=}"
: "${SANDBOXLAB_PUBLIC_HOST:=}"
: "${SANDBOXLAB_SESSION_HOURS:=4}"
: "${SANDBOXLAB_API_KEY:=}"
: "${SANDBOXLAB_RUNTIME_DIR:=$PWD/.sandboxlab}"
: "${SANDBOXLAB_NAMESPACE:=ops-system}"
: "${SANDBOXLAB_CLUSTER_NAME:=sandbox}"
: "${SANDBOXLAB_BASE_PATH:=/sandbox}"
: "${SANDBOXLAB_GATEWAY_NODEPORT:=30080}"
: "${SANDBOXLAB_IMAGE:=sandbox:sandboxlab}"
: "${SANDBOXLAB_KIND_NODE_IMAGE:=}"
: "${SANDBOXLAB_RELEASE:=sandbox}"
: "${SANDBOXLAB_DEFAULT_TTL:=1h}"
: "${SANDBOXLAB_MAX_TTL:=8h}"
: "${SANDBOXLAB_SKIP_BUILD:=false}"
: "${CLOUDFLARE_TOKEN:=}"
: "${NGROK_TOKEN:=}"

# The API key is made here rather than left to the chart, and it is not a
# detail: the chart generates one when the value is empty, and a value it
# generates is one it cannot print — the templates render before the cluster
# exists, so the Secret's contents are not available to the notes that
# `helm install` prints. What a run then sees is a placeholder where the key
# should be, which is the opposite of the notes' purpose. Generated here, the
# key is known to the installer, so it is real in the notes, in the summary,
# and in the banner.
#
# Not masked, deliberately: the key is the deliverable, and a masked value could
# not be shown anywhere that exists to show it.
if [ -z "$SANDBOXLAB_API_KEY" ]; then
  SANDBOXLAB_API_KEY=$(openssl rand -hex 32)
fi

RUNTIME_DIR="$SANDBOXLAB_RUNTIME_DIR"
mkdir -p "$RUNTIME_DIR"
TUNNEL_LOG="$RUNTIME_DIR/tunnel.log"
RESULT_ENV="$RUNTIME_DIR/result.env"
# What summary.sh reads. It is written and rewritten as facts are learned, and
# published once the environment is ready — so a run that fails partway still
# has every fact it got to, printed by the summary step that always runs.
: > "$RESULT_ENV"

# ── output ──────────────────────────────────────────────────────────────────

log()  { printf '\n\033[1;34m[sandboxlab]\033[0m %s\n' "$*"; }
warn() { printf '\n\033[1;33m[sandboxlab]\033[0m %s\n' "$*" >&2; }
die()  { printf '\n\033[1;31m[sandboxlab]\033[0m %s\n' "$*" >&2; exit 1; }

# show runs a command and prints what it said, indented. A failure is printed
# rather than fatal: a query for something that is not there yet is a fact about
# the install, and stopping on it would hide the component's own output behind a
# shell error.
show() {
  local label="$1"; shift
  printf '\n\033[1;34m[sandboxlab]\033[0m %s\n' "$label"
  "$@" 2>&1 | sed 's/^/  /' || true
}

# A command that fails under `set -e` stops the script with no message at all,
# which is the worst way for a long install to end. This says which command
# failed, on which line, and with what status.
trap 'status=$?; printf "\n\033[1;31m[sandboxlab]\033[0m %s failed (exit %d)\n" "$BASH_COMMAND" "$status" >&2' ERR

# record writes a fact for summary.sh. Later writes of the same key win, which is
# what makes the file readable at any point rather than only at the end.
record() {
  local key="$1" value="$2"
  printf '%s=%q\n' "$key" "$value" >> "$RESULT_ENV"
}

cleanup() {
  log "ending the environment"
  if [ -n "${tunnel_pid:-}" ]; then
    kill "$tunnel_pid" 2>/dev/null || true
  fi
  # The cluster is the last thing removed and the first thing that matters: it
  # holds every sandbox, so deleting it is what reclaims them all.
  kind delete cluster --name "$SANDBOXLAB_CLUSTER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── 1. the inputs are consistent ────────────────────────────────────────────

case "$SANDBOXLAB_TUNNEL" in
  cloudflare|ngrok) ;;
  *) die "tunnel must be 'cloudflare' or 'ngrok', got '$SANDBOXLAB_TUNNEL'" ;;
esac
# The failure this catches is a run that starts a tunnel agent with no credential
# and only discovers it minutes later, from a missing link.
if [ "$SANDBOXLAB_TUNNEL" = "ngrok" ] && [ -z "$NGROK_TOKEN" ]; then
  die "tunnel is 'ngrok' but no ngrok token was supplied"
fi
# A domain can only be named for a tunnel whose ingress was arranged in advance,
# which only a named Cloudflare tunnel has. Checked here rather than after a
# three-minute wait for a hostname that was never coming.
if [ -n "$SANDBOXLAB_DOMAIN" ]; then
  [ "$SANDBOXLAB_TUNNEL" = "cloudflare" ] \
    || die "a domain is only for a named Cloudflare tunnel, not '$SANDBOXLAB_TUNNEL'"
  [ -n "$CLOUDFLARE_TOKEN" ] \
    || die "a domain needs a Cloudflare tunnel token: a quick tunnel is assigned a random hostname, so the console could not be served under the one given"
fi
if [ -n "$SANDBOXLAB_PUBLIC_HOST" ] && [ -n "$SANDBOXLAB_DOMAIN" ]; then
  die "a public host and a domain are two answers to the same question; set one"
fi

# ── 2. the tools are present ────────────────────────────────────────────────

for tool in kind kubectl helm docker istioctl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not installed"
done
# The tunnel agent is checked only when a tunnel will actually be started. A run
# with a supplied host opens no tunnel — that is the whole point of the setting —
# and demanding an agent it will never invoke stops the environment over a binary
# that was never going to run. CI does exactly this, so the check has to know.
if [ -z "$SANDBOXLAB_PUBLIC_HOST" ]; then
  case "$SANDBOXLAB_TUNNEL" in
    cloudflare) command -v cloudflared >/dev/null 2>&1 || die "cloudflared is not installed" ;;
    ngrok)      command -v ngrok       >/dev/null 2>&1 || die "ngrok is not installed" ;;
  esac
fi

# Object names are looked up, never constructed.
#
# The chart's fullname helper collapses to the release name when the release name
# already contains the chart name — which it does here, because both are
# "sandbox" — so the Deployment is "sandbox" and the Secret is "sandbox-apikey",
# not "sandbox-sandbox" and "sandbox-sandbox-apikey". A script that built those
# by concatenation got a NotFound on the rollout wait of every run.
#
# Looking them up by the label the chart puts on everything is right whatever
# the release is called, and it is the same principle the rest of this uses: ask
# the cluster what exists rather than assume.
chart_object_name() {
  local kind="$1"
  kubectl -n "$SANDBOXLAB_NAMESPACE" get "$kind" \
    -l "app.kubernetes.io/instance=${SANDBOXLAB_RELEASE}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# ── 3. the control plane image, from this commit ────────────────────────────

if [ "$SANDBOXLAB_SKIP_BUILD" = "true" ]; then
  log "using the image ${SANDBOXLAB_IMAGE} as given"
else
  log "building the control plane image from this commit"
  # The same Dockerfile the release publishes, so what is tested here is what
  # ships. --load because the image has to end up in the local docker daemon for
  # kind to be given it.
  docker build \
    --build-arg "VERSION=sandboxlab" \
    --build-arg "COMMIT=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)" \
    -t "$SANDBOXLAB_IMAGE" \
    "$REPO_ROOT"
fi
record SANDBOX_IMAGE "$SANDBOXLAB_IMAGE"

# ── 4. the public address, before anything needs it ─────────────────────────

# The hostname is settled first because the control plane is installed with it:
# it becomes publicURL, which is what the API reports every sandbox's address
# under. Getting it wrong is not a cosmetic problem — the console would hand out
# links to a hostname nothing answers on.
#
# Three cases, and the difference between them is whether a name can be known
# before a tunnel is running:
#
#   a supplied host     known, and no tunnel is started (CI uses this)
#   a named tunnel      known: it is the domain the operator configured
#   a quick tunnel      NOT known until the agent reports it, so the agent runs
#                       first and this waits for it
TUNNEL_HOST=""
PUBLIC_URL=""

if [ -n "$SANDBOXLAB_PUBLIC_HOST" ]; then
  # The value may be a bare host or a whole URL, and which one it is decides
  # nothing except how much of it is already known.
  #
  # A bare host is the case where something else terminates TLS: a real domain
  # pointed at the gateway, so https is the right guess and the host is the Host
  # header. A full URL is the case where there is nothing in front at all — CI
  # reaches the gateway over plain HTTP on the node port, and there is no scheme
  # to infer that would be right.
  #
  # Getting this wrong is quiet rather than loud: guessing https for a run that
  # is reached over http reports every sandbox's address on a port nothing
  # listens on, and the run still comes up looking healthy.
  case "$SANDBOXLAB_PUBLIC_HOST" in
    http://*|https://*)
      PUBLIC_URL="${SANDBOXLAB_PUBLIC_HOST%/}"
      TUNNEL_HOST="${PUBLIC_URL#*://}"
      TUNNEL_HOST="${TUNNEL_HOST%%/*}"
      ;;
    *)
      TUNNEL_HOST="${SANDBOXLAB_PUBLIC_HOST%/}"
      PUBLIC_URL="https://${TUNNEL_HOST}"
      ;;
  esac
  log "using the supplied address ${PUBLIC_URL}; no tunnel will be started"
elif [ -n "$SANDBOXLAB_DOMAIN" ]; then
  TUNNEL_HOST="$SANDBOXLAB_DOMAIN"
  PUBLIC_URL="https://${TUNNEL_HOST}"
  log "the environment will be served at ${PUBLIC_URL}"
fi

open_tunnel() {
  # A named tunnel knows its own ingress, so nothing is passed to the connector
  # but the token. A quick tunnel is told where to send traffic and is given a
  # hostname by Cloudflare.
  case "$SANDBOXLAB_TUNNEL" in
    cloudflare)
      if [ -n "$CLOUDFLARE_TOKEN" ]; then
        log "opening a Cloudflare named tunnel"
        # The ingress is Cloudflare's, not this script's: a named tunnel routes
        # by the rules configured for it, and a --url passed here would be
        # ignored. So what this connects *to* is not something this run can see
        # or set — which is why the wait below is worth doing, and why the
        # agent's output is printed when it does not come up. A 530 is
        # Cloudflare saying it accepted the hostname and found no origin for it,
        # and the fix is an ingress rule (public hostname -> http://127.0.0.1:${SANDBOXLAB_GATEWAY_NODEPORT})
        # in the tunnel's configuration, not anything in this repository.
        log "  its ingress is configured in Cloudflare: ${SANDBOXLAB_DOMAIN} -> http://127.0.0.1:${SANDBOXLAB_GATEWAY_NODEPORT}"
        cloudflared tunnel --no-autoupdate run --token "$CLOUDFLARE_TOKEN" >"$TUNNEL_LOG" 2>&1 &
      else
        log "opening a Cloudflare quick tunnel (no account needed)"
        cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${SANDBOXLAB_GATEWAY_NODEPORT}" >"$TUNNEL_LOG" 2>&1 &
      fi
      ;;
    ngrok)
      log "opening an ngrok tunnel"
      ngrok config add-authtoken "$NGROK_TOKEN" >"$TUNNEL_LOG" 2>&1 || die "ngrok rejected the token"
      ngrok http "$SANDBOXLAB_GATEWAY_NODEPORT" >>"$TUNNEL_LOG" 2>&1 &
      ;;
  esac
  tunnel_pid=$!

  # The agent's output is what explains a failure — "tunnel not found", a bad
  # token — and a log nobody prints hides exactly that. -u because the job log
  # is not a tty and sed would block-buffer.
  tail -f "$TUNNEL_LOG" 2>/dev/null | sed -u 's/^/[tunnel] /' &
  tail_pid=$!

  # An agent that fails on a bad credential exits instantly, and without this
  # check the only symptom is a missing link minutes later.
  sleep 3
  kill -0 "$tunnel_pid" 2>/dev/null || {
    sed 's/^/    /' "$TUNNEL_LOG" 2>/dev/null || true
    die "the ${SANDBOXLAB_TUNNEL} agent exited during startup; its output is above"
  }
}

# find_tunnel_host waits for an agent to report the hostname it was given.
find_tunnel_host() {
  local attempt from_log from_api
  for attempt in $(seq 1 60); do
    # The agent's own log is the source that always works: both clients print
    # the URL they were given, and neither needs an account to do it.
    case "$SANDBOXLAB_TUNNEL" in
      cloudflare) from_log=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$TUNNEL_LOG" 2>/dev/null | head -1 || true) ;;
      ngrok)      from_log=$(grep -oE 'https://[a-z0-9-]+\.ngrok-free\.app' "$TUNNEL_LOG" 2>/dev/null | head -1 || true) ;;
    esac
    if [ -n "$from_log" ]; then
      printf '%s' "$from_log"
      return 0
    fi
    # ngrok's local API, as a fallback for a version that prints its URL in a
    # shape the pattern above does not match.
    if [ "$SANDBOXLAB_TUNNEL" = "ngrok" ]; then
      from_api=$(curl -fsS http://127.0.0.1:4040/api/tunnels 2>/dev/null \
        | grep -oE 'https://[a-z0-9-]+\.ngrok-free\.app' | head -1 || true)
      if [ -n "$from_api" ]; then
        printf '%s' "$from_api"
        return 0
      fi
    fi
    kill -0 "$tunnel_pid" 2>/dev/null || return 1
    if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting for the tunnel to report its hostname... ${attempt}"; fi
    sleep 2
  done
  return 1
}

# A quick tunnel or a named one with the agent started first: both need the
# agent up before the control plane is installed, because the hostname is what
# publicURL is set to.
if [ -z "$TUNNEL_HOST" ]; then
  open_tunnel
  host=$(find_tunnel_host) || die "the tunnel did not report a hostname; its output is above"
  TUNNEL_HOST=${host#https://}
  TUNNEL_HOST=${TUNNEL_HOST#http://}
  log "the tunnel reported ${TUNNEL_HOST}"
fi

# A tunnel that has just reported its hostname serves https, and this is the one
# place that knows it. A run with an address already settled does NOT come
# through here — its PUBLIC_URL was decided above, where a supplied value could
# say http — so this cannot overwrite it with a scheme that is wrong for it.
if [ -z "$PUBLIC_URL" ]; then
  PUBLIC_URL="https://${TUNNEL_HOST}"
fi

record SANDBOX_PUBLIC_HOST "$TUNNEL_HOST"
record SANDBOX_PUBLIC_URL "$PUBLIC_URL"
record SANDBOX_BASE_PATH "$SANDBOXLAB_BASE_PATH"
# The node port the gateway is reachable on from the host, which is what the
# script's own checks use — and what an environment with no tunnel is reached
# through.
record SANDBOX_GATEWAY_URL "http://127.0.0.1:${SANDBOXLAB_GATEWAY_NODEPORT}"

# Start the tunnel agent here, once the address is known and before anything
# waits on it.
#
# The named-tunnel case used to leave this to section 9, and section 8 waits for
# the tunnel to serve the console at the end of the run — so the wait was for
# something that had not been started. The sequence was:
#
#     [sandboxlab] waiting for the tunnel to serve the console at https://...
#     curl: (22) ... 530
#
# Two minutes of 530s, a warning, and a finished-looking run whose link did not
# work yet. The agent came up a few lines later, so the link usually started
# working shortly after — which is exactly the kind of fault that gets left
# alone, because the symptom is a delay rather than a failure.
#
# Here rather than in section 9 because the check and the thing checked are the
# same fact: an agent is running, so waiting for it to serve is meaningful. It
# is also harmless to start now — cloudflared connects out and the local address
# answers once the cluster is up, and it reconnects until then.
if [ -z "$SANDBOXLAB_PUBLIC_HOST" ] && [ -z "${tunnel_pid:-}" ]; then
  open_tunnel
fi

# ── 5. the cluster ──────────────────────────────────────────────────────────

log "creating the kind cluster"

# The node port mapping is what makes the gateway reachable from outside: the
# tunnel (or anything on this runner) connects to a port on the host, and kind
# forwards it into the node where the ingress gateway is listening.
#
# One mapping is enough, and the port in it does not leak into routing: a
# request to "http://host:30080/path" carries "Host: host:30080", but Istio sets
# IgnorePortInHostMatching on the gateway's route configuration, so Envoy drops
# the port before matching.
cat > "$RUNTIME_DIR/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${SANDBOXLAB_CLUSTER_NAME}
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "ingress-ready=true"
    extraPortMappings:
      - containerPort: ${SANDBOXLAB_GATEWAY_NODEPORT}
        hostPort: ${SANDBOXLAB_GATEWAY_NODEPORT}
        protocol: TCP
EOF

kind_args=(--config "$RUNTIME_DIR/kind.yaml" --wait 120s)
if [ -n "$SANDBOXLAB_KIND_NODE_IMAGE" ]; then
  kind_args+=(--image "$SANDBOXLAB_KIND_NODE_IMAGE")
fi
kind create cluster "${kind_args[@]}"

show "the cluster" kubectl get nodes -o wide

# The image is loaded rather than pushed: there is no registry here, and the
# point is to run this commit's code, not an image from somewhere.
if [ "$SANDBOXLAB_SKIP_BUILD" != "true" ]; then
  log "loading ${SANDBOXLAB_IMAGE} into the cluster"
  kind load docker-image "$SANDBOXLAB_IMAGE" --name "$SANDBOXLAB_CLUSTER_NAME"
fi

# ── 6. the ingress gateway ──────────────────────────────────────────────────

log "installing Istio (this is the slow step)"

# The community default profile, into the community default namespace, producing
# the community default gateway name. Nothing is overridden for kind, because
# nothing needs to be: Istio's platform profiles carry only CNI paths, and kind
# uses the standard containerd layout.
istioctl install --set profile=default -y

kubectl -n istio-system wait --for=condition=available --timeout=300s \
  deployment/istio-ingressgateway

# The `Gateway` resource, which istioctl deliberately does not install.
#
# `istioctl install` produces the Deployment, the Service and the RBAC, and
# stops: a Gateway resource is the operator's to write, because it is what says
# which ports and hosts this proxy serves. A VirtualService that names a gateway
# which does not exist has nothing to be programmed into, so the proxy has no
# route and answers 404 — while the pod behind it is healthy and serving. That
# symptom is worth the four lines below.
#
# The chart does not create this and should not: a gateway is cluster
# infrastructure that an installation attaches to. This environment assembles
# that infrastructure, so it belongs here.
gateway_selector=$(kubectl -n istio-system get deployment istio-ingressgateway \
  -o jsonpath='{.spec.selector.matchLabels}')
[ -n "$gateway_selector" ] \
  || die "the ingress gateway deployment has no selector, so a Gateway resource cannot be bound to it"

log "creating the Gateway resource istioctl does not install"
kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: istio-ingressgateway
  namespace: istio-system
spec:
  selector: ${gateway_selector}
  servers:
    - port:
        number: 80
        name: http
        protocol: HTTP
      hosts:
        - "*"
EOF

show "istio (the control plane, the gateway deployment and the Gateway)" \
  kubectl -n istio-system get deployment,service,gateway.networking.istio.io

# Expose the gateway's HTTP port on the node port kind already maps to the host.
#
# The Service is a LoadBalancer by default, which never gets an address on kind,
# so it has to become a NodePort for anything outside the cluster to reach it.
#
# `--type=strategic` is required, not stylistic. The gateway's port list carries
# `patchMergeKey: port`, which is a *strategic* merge instruction — a plain JSON
# merge patch ignores it and replaces the whole list, deleting the status port
# (15021) and HTTPS (443) along with their names. That would leave a gateway
# that cannot report its own health, and it would survive the check below, since
# port 80's nodePort is correct either way.
log "exposing the gateway on node port ${SANDBOXLAB_GATEWAY_NODEPORT}"
kubectl -n istio-system patch svc istio-ingressgateway --type=strategic -p "$(cat <<EOF
spec:
  type: NodePort
  ports:
    - port: 80
      nodePort: ${SANDBOXLAB_GATEWAY_NODEPORT}
EOF
)"

gateway_nodeport=$(kubectl -n istio-system get svc istio-ingressgateway \
  -o jsonpath='{.spec.ports[?(@.port==80)].nodePort}')
[ "$gateway_nodeport" = "$SANDBOXLAB_GATEWAY_NODEPORT" ] \
  || die "the gateway's HTTP port is on node port '${gateway_nodeport}', expected ${SANDBOXLAB_GATEWAY_NODEPORT}"

# The two ports a merge patch would have silently removed. Checked because the
# check above passes either way.
for port in 15021 443; do
  kubectl -n istio-system get svc istio-ingressgateway \
    -o jsonpath="{.spec.ports[?(@.port==${port})].port}" | grep -qx "$port" \
    || die "the gateway lost its port ${port} when it was patched to NodePort"
done

# ── 7. the control plane ────────────────────────────────────────────────────

kubectl create namespace "$SANDBOXLAB_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

install_args=(
  --namespace "$SANDBOXLAB_NAMESPACE"
  --set "publicURL=${PUBLIC_URL}"
  --set "basePath=${SANDBOXLAB_BASE_PATH}"
  --set "defaultTTL=${SANDBOXLAB_DEFAULT_TTL}"
  --set "maxTTL=${SANDBOXLAB_MAX_TTL}"
  --set "istio.enabled=true"
  --set "istio.gateway=istio-system/istio-ingressgateway"
  # Always set, never left to the chart: a key the chart generates is one the
  # chart's own notes cannot print. See where it is made, above.
  --set "apiKey=${SANDBOXLAB_API_KEY}"
)
if [ "$SANDBOXLAB_SKIP_BUILD" != "true" ]; then
  # The image was loaded into the node, so there is nothing to pull. Pulling
  # would fail on a name no registry knows.
  repo=${SANDBOXLAB_IMAGE%:*}
  tag=${SANDBOXLAB_IMAGE##*:}
  install_args+=(
    --set "image.repository=${repo}"
    --set "image.tag=${tag}"
    --set "image.pullPolicy=Never"
  )
fi

log "installing the control plane into ${SANDBOXLAB_NAMESPACE}, served at ${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}"
helm upgrade --install "$SANDBOXLAB_RELEASE" "$REPO_ROOT/charts/sandbox" "${install_args[@]}"

log "waiting for the control plane to roll out"
deployment_name=$(chart_object_name deployment)
[ -n "$deployment_name" ] || die "the chart did not create a deployment in ${SANDBOXLAB_NAMESPACE}"
kubectl -n "$SANDBOXLAB_NAMESPACE" rollout status --timeout=180s "deployment/${deployment_name}"

# The key comes from whichever Secret the chart made, found by the same label —
# and the check that it exists is separate from the check that it has a value,
# because "the Secret is missing" and "the Secret is empty" are different
# mistakes and the message should say which.
secret_name=$(chart_object_name secret)
[ -n "$secret_name" ] || die "the chart did not create an API key Secret in ${SANDBOXLAB_NAMESPACE}"
API_KEY=$(kubectl -n "$SANDBOXLAB_NAMESPACE" get secret "$secret_name" \
  -o jsonpath='{.data.api-key}' | base64 -d)
[ -n "$API_KEY" ] || die "the API key Secret ${secret_name} has no api-key in it"
# The value in the Secret and the value the running pod is using are two
# different things, and the chart gives the pod that value through an env var
# rather than reading the Secret again — so a Secret updated without the pod
# being restarted leaves a pod authenticating with the old key while this script
# holds the new one. Every request then fails 401, and the 401 is the only
# symptom: the API is up, the route exists, and the key looks right.
#
# Compared here rather than left to the first request, because at that point the
# answer is "the API refused the key the chart installed" and the real cause —
# a Deployment that was never rolled — is three steps back.
pod_key=$(kubectl -n "$SANDBOXLAB_NAMESPACE" get deploy \
  -l app.kubernetes.io/instance=sandbox \
  -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="SANDBOX_API_KEY")].valueFrom.secretKeyRef.name}')
[ -n "$pod_key" ] || pod_key="$secret_name"
if [ "$pod_key" != "$secret_name" ]; then
  warn "the Deployment reads its key from '${pod_key}' but the chart made '${secret_name}'; a request with the new key will be refused"
fi
record SANDBOX_API_KEY "$API_KEY"
record SANDBOX_CONSOLE_URL "${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}"

# ── 8. the environment answers ──────────────────────────────────────────────

# One address, through the gateway, on the node port. This is the path a caller
# takes, so it is the path to test over — not a port-forward, which would skip
# the gateway and the VirtualService and prove neither.
local_url="http://127.0.0.1:${SANDBOXLAB_GATEWAY_NODEPORT}${SANDBOXLAB_BASE_PATH}"

log "waiting for the gateway to serve the console"
served=false
for attempt in $(seq 1 60); do
  if curl -fsS -o /dev/null -H "Host: ${TUNNEL_HOST}" "${local_url}/healthz"; then
    served=true
    break
  fi
  if [ $((attempt % 10)) -eq 0 ]; then log "  still waiting... (${attempt})"; fi
  sleep 3
done
[ "$served" = "true" ] || {
  show "the gateway's routes" kubectl -n istio-system get virtualservice,gateway
  show "the control plane's pod" kubectl -n "$SANDBOXLAB_NAMESPACE" describe pod -l app.kubernetes.io/name=sandbox
  die "the gateway did not serve the console within 180s"
}

log "confirming the API answers with its key"
# The key has to be checked over the same path a caller uses, because the chart
# passing it and the server reading it are two different things — and a Secret
# whose value never reached the environment looks identical to a working one
# until someone tries a request.
if ! curl -fsS -H "Host: ${TUNNEL_HOST}" -H "X-Sandbox-Key: ${API_KEY}" "${local_url}/api/v1/catalog" >/dev/null; then
  die "the API refused the key the chart installed; the Secret and the environment do not agree"
fi
# And that it refuses the wrong one, which is what says the check above meant
# something. This request is *supposed* to fail, so its curl error is expected
# output rather than a symptom — printed to nowhere so the job log does not
# carry a `curl: (22) ... 401` that reads like a fault next to the real ones.
if curl -fsS -o /dev/null -H "Host: ${TUNNEL_HOST}" -H "X-Sandbox-Key: wrong" \
  "${local_url}/api/v1/catalog" 2>/dev/null; then
  die "the API accepted a wrong key"
fi
record SANDBOX_CATALOG "$(curl -fsS -H "Host: ${TUNNEL_HOST}" -H "X-Sandbox-Key: ${API_KEY}" \
  "${local_url}/api/v1/catalog" | grep -o '"id": *"[^"]*"' | sed 's/.*: *"//; s/"$//' | paste -sd, - || true)"

# And once through the tunnel, which is the last thing that can be wrong: the
# gateway can serve and the tunnel still route nothing.
if [ -z "$SANDBOXLAB_PUBLIC_HOST" ]; then
  log "waiting for the tunnel to serve the console at ${PUBLIC_URL}"
  tunnel_served=false
  for attempt in $(seq 1 40); do
    if curl -fsS -o /dev/null --max-time 10 "${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}/healthz"; then
      tunnel_served=true
      break
    fi
    if [ $((attempt % 10)) -eq 0 ]; then log "  still waiting through the tunnel... (${attempt})"; fi
    sleep 3
  done
  if [ "$tunnel_served" = "true" ]; then
    log "the tunnel is serving the environment"
  else
    # What the agent said, before the warning about it. A link that does not
    # work is the one failure a person notices from outside the run, and the
    # reason is always in the agent's own output — a token that expired, a
    # tunnel name that no longer exists, an ingress rule pointing somewhere
    # else. The log is a few lines and it is the whole diagnosis.
    show "the tunnel agent's last output" tail -30 "$TUNNEL_LOG"
    warn "the tunnel has not served the environment yet; the link may work shortly"
  fi
fi

record SANDBOX_READY "true"

# A sandbox is created and deleted as the last check, because everything above
# proves the control plane is up and none of it proves it can do its job. This
# is also what proves the RBAC, which is the part most likely to be wrong: the
# control plane can serve the catalog with no cluster access at all.
log "creating a sandbox to confirm the control plane can create one"
created=$(curl -fsS -X POST -H "Host: ${TUNNEL_HOST}" -H "X-Sandbox-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"template":"python","name":"sandboxlab-smoke","ttl":"5m"}' \
  "${local_url}/api/v1/sandboxes" 2>&1) || die "the smoke sandbox could not be created: ${created}"

# It is deleted by the reaper when its five minutes are up; the cluster goes
# with the job either way. Removing it here just leaves the environment tidy for
# whoever is about to use it.
curl -fsS -X DELETE -H "Host: ${TUNNEL_HOST}" -H "X-Sandbox-Key: ${API_KEY}" \
  "${local_url}/api/v1/sandboxes/sandboxlab-smoke" >/dev/null 2>&1 || true

show "the sandboxes" kubectl get namespaces -l app.kubernetes.io/managed-by=sandboxlab

# ── 9. publish ──────────────────────────────────────────────────────────────

# The tunnel was started back in section 4, once its address was known and
# before anything waited on it — see the note there for what went wrong when it
# was not.

# The summary, and the banner below, are published from here rather than from a
# step after this script. The script holds the session open for hours, so a step
# that ran once it returned would leave the run's Summary — where the console
# link and the API key live — empty for the whole session, which is exactly when
# someone is looking for them. Publishing before the wait is also what makes the
# deliverable exist at all: everything anyone needs is printed while the
# environment is up, not after it is gone.
#
# summary.sh prints what environment.sh recorded, rather than deriving anything,
# so what appears is what this process actually learned.
bash "$REPO_ROOT/hack/summary.sh"

# The same facts again, unmissably, in the log. The Summary is a tab someone has
# to know to open; a run is read by scrolling, and a key that only exists in the
# tab is a key nobody finds. This is what makes the link and the key part of the
# run's own output.
cat <<EOF

=====================================================================
 sandboxlab is ready

   Console:  ${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}
   API key:  ${API_KEY}

   The console asks for that address and this key; both are kept in your
   browser. A sandbox is served under
   ${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}/sandbox/<name>/<port>/.

   Create one from the CLI:

     export SANDBOX_URL='${PUBLIC_URL}${SANDBOXLAB_BASE_PATH}'
     export SANDBOX_KEY='${API_KEY}'
     sandbox catalog
     sandbox create -t all-in-one --name demo --wait
     sandbox url demo

=====================================================================
EOF

# ── 10. stay up ─────────────────────────────────────────────────────────────

if [ "$SANDBOXLAB_SESSION_HOURS" = "0" ]; then
  log "the environment is up and runs until the job times out or the workflow is cancelled"
else
  log "the environment is up and runs for ${SANDBOXLAB_SESSION_HOURS}h"
fi

# The loop exists to notice a tunnel that has died, which is the one thing that
# makes a running environment unreachable. The alternative — sleeping out the
# whole session and finding out at the deadline — spends hours on an address
# that stopped working in the first minute.
deadline=$(( $(date +%s) + SANDBOXLAB_SESSION_HOURS * 3600 ))
while true; do
  if [ "$SANDBOXLAB_SESSION_HOURS" != "0" ] && [ "$(date +%s)" -ge "$deadline" ]; then
    break
  fi
  # The loop is a signal-handled sleep: a cancelled workflow kills the script,
  # and the EXIT trap tears the environment down.
  sleep 30 &
  wait $! 2>/dev/null || break
  if [ -n "${tunnel_pid:-}" ] && ! kill -0 "$tunnel_pid" 2>/dev/null; then
    warn "the tunnel agent has exited; the console is no longer reachable from outside"
    warn "the environment is still up inside the cluster, and the run will end at its deadline"
    # Nothing to restart it with — the credential may be gone — so the state is
    # reported once and the loop waits out the session rather than spinning.
    tunnel_pid=""
  fi
done

log "environment over"
