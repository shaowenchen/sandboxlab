#!/usr/bin/env bash
#
# Check the rendered chart, not just that it renders.
#
# `helm lint` and `helm template` answer "is this valid YAML" and "does it
# render at all". They do not answer the questions a deployment actually fails
# on: a ServiceAccount the Deployment names but nobody created, a probe path the
# server does not serve, an environment variable the chart forgot to pass through
# from its values. Those are all silently-valid, and all of them are an outage.
#
# So this renders the chart — in more than one configuration — and asserts the
# things the deployment depends on. It is run by `make helm-check` and by CI.
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
CHART="$REPO_ROOT/charts/sandbox"

fail=0
pass=0

ok()   { pass=$((pass + 1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad()  { fail=$((fail + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }
# Named `section` rather than `head`: this script pipes through the real
# `head` below, and a function of that name would shadow it.
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# has asserts that the rendered output contains a line, with a message that
# says what it was looking for rather than just that it was missing.
has() {
  local out="$1" needle="$2" what="$3"
  if grep -qF -- "$needle" <<<"$out"; then ok "$what"; else bad "$what (no line matching: $needle)"; fi
}

hasnt() {
  local out="$1" needle="$2" what="$3"
  if grep -qF -- "$needle" <<<"$out"; then bad "$what (found: $needle)"; else ok "$what"; fi
}

# ── the default rendering ───────────────────────────────────────────────────

section "the default rendering"

default=$(helm template sandbox "$CHART" --namespace default)

has "$default" "namespace: ops-system" \
  "the control plane lands in ops-system, not the release namespace"

# The Deployment names a ServiceAccount; the chart has to create that exact one,
# or the pod sits in Pending with a message about a missing account.
sa=$(grep -A2 'kind: ServiceAccount' <<<"$default" | grep 'name: sandbox-control-plane' | head -1 || true)
if [ -n "$sa" ]; then
  ok "the control plane's ServiceAccount is created"
else
  bad "the control plane's ServiceAccount is missing"
fi
has "$default" "serviceAccountName: sandbox-control-plane" \
  "the Deployment names that ServiceAccount"

has "$default" 'livenessProbe' "there is a liveness probe"
has "$default" 'readinessProbe' "there is a readiness probe"
has "$default" 'path: "/healthz"' "liveness probes /healthz"
has "$default" 'path: "/readyz"' "readiness probes /readyz"

# The key must come from a Secret, never a literal in the pod spec — an env
# value here would be readable by anyone who can read the Deployment.
has "$default" 'secretKeyRef' "the API key comes from a Secret"
has "$default" 'key: api-key' "the Secret key is the one the chart writes"
hasnt "$default" 'value: "dWVwYm' "the API key is not written as a literal env value"

has "$default" '"helm.sh/hook": pre-delete' "the cleanup job runs before an uninstall"
has "$default" 'app.kubernetes.io/managed-by=sandboxlab' \
  "the cleanup job deletes by the label the control plane sets"

has "$default" 'runAsNonRoot: true' "the pod runs as a non-root user"
hasnt "$default" 'privileged: true' "nothing runs privileged"

# Every read the API makes has to be granted, or it fails as a 403 at request
# time rather than at install — which is how the metrics and events panels were
# empty on a deployment that was otherwise working. These are the grants whose
# absence is not obvious from the rest of the manifest.
for pair in "pods/log:reading a sandbox's output" \
            "metrics.k8s.io:reading what a sandbox is using" \
            "events:reading the events about a sandbox"; do
  res="${pair%%:*}"; why="${pair#*:}"
  if grep -q "\"$res\"\|\b$res\b" <<<"$default"; then
    ok "the ClusterRole grants $res ($why)"
  else
    bad "the ClusterRole does not grant $res ($why)"
  fi
done

# The secrets grant's *verbs*, not just its presence.
#
# The loop above only asks whether the resource is named, so a rule narrowed back
# to `get` — which is what it was before sandboxes had keys of their own —
# would pass it. `list` is the resolver's cluster-wide listing and `create` is a
# sandbox's key being minted, and a control plane without either fails at
# request time with a 403 on a deployment that otherwise looks fine.
secrets_verbs=$(awk '
  /^ *resources: \["secrets"\]/ { found = 1 }
  found && /^ *verbs:/ { print; exit }
' <<<"$default")
if [ -z "$secrets_verbs" ]; then
  bad "the ClusterRole has no secrets rule, so sandbox keys cannot be resolved or minted"
else
  for verb in get list create update; do
    if grep -q "\"$verb\"" <<<"$secrets_verbs"; then
      ok "the secrets grant carries $verb"
    else
      bad "the secrets grant is missing $verb ($secrets_verbs)"
    fi
  done
fi

# ── the base path ───────────────────────────────────────────────────────────

section "with a base path"

based=$(helm template sandbox "$CHART" --namespace default \
  --set basePath=/sandbox --set publicURL=https://sandbox.example.com)

has "$based" 'path: "/sandbox/healthz"' "liveness follows the base path"
has "$based" 'path: "/sandbox/readyz"' "readiness follows the base path"
has "$based" 'value: "/sandbox"' "the base path reaches the server"
has "$based" 'value: "https://sandbox.example.com"' "the public URL reaches the server"

# ── every value reaches the pod ─────────────────────────────────────────────

section "the values the pod reads"

configured=$(helm template sandbox "$CHART" --namespace default --set-string \
  defaultTTL=15m,maxTTL=2h,maxSandboxes=7,reapInterval=5s,logLevel=debug,sandboxNamespacePrefix=box-)

for pair in "SANDBOX_DEFAULT_TTL:15m" "SANDBOX_MAX_TTL:2h" "SANDBOX_MAX_SANDBOXES:7" \
            "SANDBOX_REAP_INTERVAL:5s" "SANDBOX_LOG_LEVEL:debug" "SANDBOX_NAMESPACE_PREFIX:box-"; do
  name=${pair%%:*}; want=${pair#*:}
  # The value renders on the line after the name, which is how a Kubernetes env
  # list is written.
  got=$(grep -A1 "name: $name\$" <<<"$configured" | grep 'value:' | sed 's/.*value: //' | tr -d '"')
  if [ "$got" = "$want" ]; then
    ok "$name reaches the pod as $want"
  else
    bad "$name = $got, want $want"
  fi
done

# ── Istio ───────────────────────────────────────────────────────────────────

section "with Istio"

istio=$(helm template sandbox "$CHART" --namespace default \
  --set istio.enabled=true --set istio.gateway=istio-system/ingressgateway \
  --set basePath=/sandbox)

has "$istio" 'kind: VirtualService' "a VirtualService is rendered"
has "$istio" '- "istio-system/ingressgateway"' "it attaches to the named Gateway"
has "$istio" 'prefix: "/sandbox/"' "it matches the base path"
has "$istio" 'host: sandbox.ops-system.svc.cluster.local' "it routes to the control plane's Service"

# The route must carry no timeout, which is how Istio is made to never cut a
# streamed sandbox request off: its route translation sets a zero timeout unless
# the VirtualService names one, overriding Envoy's own 15s default.
#
# Anchored to the chart's own indentation and stripped of comments, because an
# unanchored `timeout:` also matches the comment above the field — which is
# where the last bug hid: `timeout: 0s` was there for real, Istio 1.24 refused
# it at admission ("must be a valid duration greater than 1ms"), and the install
# failed on a route that rendered perfectly.
render_timeout=$(sed 's/#.*//' <<<"$istio" \
  | grep -cE '^( +|\t*)timeout:' || true)
if [ "$render_timeout" -eq 0 ]; then
  ok "no timeout is set, so a streamed sandbox request is never cut off"
else
  bad "the route sets a timeout; a streamed sandbox request would be cut off"
fi

# Without a gateway the chart should refuse, not render something that attaches
# to nothing — a VirtualService with an empty gateway is accepted by the API
# server and serves nothing, which is a silent failure.
if helm template sandbox "$CHART" --namespace default --set istio.enabled=true >/dev/null 2>&1; then
  bad "istio.enabled with no gateway should refuse to render"
else
  ok "istio.enabled with no gateway refuses to render"
fi

# ── the API key ─────────────────────────────────────────────────────────────

section "the API key"

# `helm template` has no cluster to look in, so it generates a key — which is
# what an install does the first time, so the rendering is representative. What
# this checks is that the value the operator set is what lands in the Secret:
# the chart b64-encodes it, and an encoding bug here is a key nobody can use.
pinned=$(helm template sandbox "$CHART" --namespace default --set apiKey=my-fixed-key \
  | grep 'api-key:' | head -1 | sed 's/.*api-key: *//' | tr -d '"')
if [ "$pinned" = "$(printf 'my-fixed-key' | base64)" ]; then
  ok "apiKey is rendered as its base64 encoding"
else
  bad "apiKey rendered as $pinned, want the base64 of my-fixed-key"
fi

# A generated key must be long enough to be worth anything. 32 characters is
# the chart's own choice and this is where it is held to it.
generated=$(helm template sandbox "$CHART" --namespace default \
  | grep 'api-key:' | head -1 | sed 's/.*api-key: *//' | tr -d '"' | base64 -d)
if [ "${#generated}" -ge 32 ]; then
  ok "a generated key is at least 32 characters (${#generated})"
else
  bad "a generated key is only ${#generated} characters"
fi

# A key the release should not carry goes in a Secret the operator named. The
# chart then creates none of its own, and the Deployment reads the named one —
# which is the whole point: the value never enters the release's values, its
# rendered manifests, or helm's output.
named=$(helm template sandbox "$CHART" --namespace default --set existingSecret=team-key)
if grep -qE '^kind: Secret$' <<<"$named"; then
  bad "existingSecret still renders a key Secret of the chart's own"
else
  ok "existingSecret renders no key Secret"
fi
# Note the lack of -q on the first grep: -q stops at the first match and prints
# nothing, so the context lines this depends on would never reach the pipe. It
# is the same trap environment.sh documents around `grep -q` and SIGPIPE.
if grep -A2 'secretKeyRef:' <<<"$named" | grep -q 'name: team-key'; then
  ok "the Deployment reads its key from the Secret that was named"
else
  bad "the Deployment does not name team-key as the source of its key"
fi

# Both settings is a contradiction, not a precedence: one of them would be
# silently ignored, and which one is not something an operator should have to
# know. Refused, so the mistake is at the install rather than in a key nobody
# can explain.
if helm template sandbox "$CHART" --namespace default \
  --set existingSecret=team-key --set apiKey=my-fixed-key >/dev/null 2>&1; then
  bad "existingSecret and apiKey together should be refused"
else
  ok "existingSecret and apiKey together refuse to render"
fi

# The helper is not memoized: `randAlphaNum` runs on every call, so two calls
# return two different keys. Anything that printed "the key" would be guessing
# which of them the Secret got — which is why the notes do not call it. Held
# here as a source-level rule, because the failure is a mismatch nobody would
# see in a rendered manifest.
#
# The needle is split so this check does not match its own text.
needle="include \"sandbox.api""Key\""
if grep -q "$needle" "$CHART/templates/NOTES.txt"; then
  bad "the notes call the key helper, which is not memoized and may not be the key the Secret got"
else
  ok "the notes never call the key helper, so they cannot print a stale key"
fi

# ── the notes ───────────────────────────────────────────────────────────────

section "the notes helm prints on install"

# `helm template` does not render NOTES.txt — Helm prints it from the install,
# not the manifest — so the file is rendered as an ordinary template to be seen
# at all. It is copied rather than renamed in place: the working tree is not
# this script's to change.
#
# A notes file is free text and not valid YAML, so rendering it as a manifest
# makes helm report a parse error while `--debug` still prints what it produced.
# That is why the assignments below end in `|| true` and read the debug output:
# the error is about the shape of the file, not about what it says.
#
# What is asserted is the rule the notes follow now: only a key this release
# generated itself is printed. A key it was *given* — a dispatch input, a
# repository secret, a shell — is masked, because publishing someone else's
# credential in a log is not the notes' to decide.
notes_dir=$(mktemp -d)
trap 'rm -rf "$notes_dir"' EXIT
cp -r "$CHART"/. "$notes_dir"/
mv "$notes_dir/templates/NOTES.txt" "$notes_dir/templates/notes-rendered.txt"

render_notes() {
  helm template sandbox "$notes_dir" --namespace default --debug "$@" 2>&1 || true
}

# The supplied-key path. The value must not be in the notes at all, and the
# notes must still say which key is live and where to read it — a run that says
# nothing at all about the key is the other way to get this wrong.
notes_with_key=$(render_notes --set apiKey=my-fixed-key --set publicURL=https://sandbox.example.com)

if grep -qF 'my-fixed-key' <<<"$notes_with_key"; then
  bad "the notes print the key they were given, which is not theirs to print"
else
  ok "the notes do not print a key the release was given"
fi
if grep -qF 'This release was given a key 12 characters long' <<<"$notes_with_key"; then
  ok "the notes say a key was supplied, and how long it is"
else
  bad "the notes do not say that a key was supplied rather than generated"
fi
if grep -qF "get secret sandbox-apikey" <<<"$notes_with_key"; then
  ok "the notes say where to read the key that is live"
else
  bad "the notes do not say where to read the supplied key"
fi
# And the CLI export is the read-back command, never a literal: the notes have
# no key to put in it.
if grep -qF 'export SANDBOX_KEY="$(kubectl' <<<"$notes_with_key"; then
  ok "the CLI export in the notes reads the key back rather than embedding one"
else
  bad "the CLI export in the notes does not read the key back"
fi

# The named-Secret path: same rule, and the Secret in the notes is the
# operator's, not the chart's.
notes_named=$(render_notes --set existingSecret=team-key --set publicURL=https://sandbox.example.com)
if grep -qF 'get secret team-key' <<<"$notes_named"; then
  ok "the notes name the Secret the operator supplied"
else
  bad "the notes do not name the supplied Secret"
fi
if grep -qF 'get secret sandbox-apikey' <<<"$notes_named"; then
  bad "the notes point at the chart's own Secret although another was named"
else
  ok "the notes do not fall back to the chart's own Secret"
fi

# A release that let the chart generate the key cannot print it either — the
# templates render before the cluster exists — so that path says where to read
# it too.
notes_generated=$(render_notes --set publicURL=https://sandbox.example.com)
if grep -qF 'get secret sandbox-apikey' <<<"$notes_generated"; then
  ok "the notes say where to read a key the chart generated"
else
  bad "the notes do not say where to find a generated key"
fi

# ── summary ─────────────────────────────────────────────────────────────────

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
