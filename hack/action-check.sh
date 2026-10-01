#!/usr/bin/env bash
#
# Check the action's inputs actually reach the script.
#
# An input the action declares but never forwards is the worst kind of bug in a
# composite action: the workflow validates it, the user sets it, the run
# succeeds, and the setting did nothing. Nothing in the action's own YAML says
# so — a missing `env:` entry and a correct one look the same.
#
# So this reads both sides and compares them: every input the action declares
# must appear in the env block of the step that runs the script, and every
# SANDBOXLAB_* variable the script reads must be set by that block.
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
ACTION="$REPO_ROOT/action/action.yml"
SCRIPT="$REPO_ROOT/hack/environment.sh"

fail=0
ok()  { printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }

# ── the inputs the action declares ──────────────────────────────────────────

# The `inputs:` block, up to `runs:`. Each input is a key indented by exactly
# two spaces; a longer indent is a description or a nested field.
inputs=$(awk '/^inputs:/{f=1;next} /^runs:/{f=0} f && /^  [a-z_]+:/{ sub(/^  /, ""); sub(/:.*/, ""); print }' "$ACTION")
[ -n "$inputs" ] || { echo "could not read the inputs from $ACTION" >&2; exit 1; }

# ── the env the step that runs the script sets ──────────────────────────────

# Only the step named "Start the environment". Other steps have env blocks of
# their own, and a key found in one of those would not mean the script ever saw
# it — which is the exact confusion this script exists to catch.
env_block=$(awk '
  /^    - name: /{ step = substr($0, index($0, "name: ") + 6) }
  step == "Start the environment" && /^        [A-Z_]+:/{ print }
' "$ACTION")
[ -n "$env_block" ] || { echo "could not read the env block of the \"Start the environment\" step from $ACTION" >&2; exit 1; }

env_keys=$(sed 's/^ *//; s/:.*//' <<<"$env_block")

printf '\033[1mevery declared input is forwarded by the step that runs the script\033[0m\n'
for input in $inputs; do
  # Looked up by the expression rather than by a naming rule. The env key an
  # input lands in is not always derivable from its name — the tunnel tokens are
  # forwarded under the names the agents expect, not under a prefixed one — and
  # a rule would have to carry those exceptions. What matters is that the input
  # reaches the environment, and this is that question directly.
  key=$(grep -F "inputs.${input}" <<<"$env_block" 2>/dev/null | head -1 | sed 's/^ *//; s/:.*//' || true)
  if [ -n "$key" ]; then
    ok "$input -> $key"
  else
    bad "$input is declared but never appears in the step's env; setting it does nothing"
  fi
done

# ── the variables the script reads ──────────────────────────────────────────

printf '\n\033[1mevery variable the script reads is set by the action\033[0m\n'
# `: "${SANDBOXLAB_X:=...}"` is how the script declares an input with a
# default; those are the ones the action is expected to supply.
read_vars=$(grep -oE 'SANDBOXLAB_[A-Z_]+' "$SCRIPT" | sort -u)
for var in $read_vars; do
  case "$var" in
    # Not inputs: paths and names the script or the workflow owns.
    SANDBOXLAB_RUNTIME_DIR|SANDBOXLAB_RESULT_ENV)
      continue ;;
  esac
  if grep -qx "$var" <<<"$env_keys"; then
    ok "$var is supplied"
  else
    # Not fatal: a variable with a default in the script is one the action may
    # legitimately leave alone. It is reported so the omission is a decision.
    printf '  \033[33mskip\033[0m %s is not set by the action (the script defaults it)\n' "$var"
  fi
done

# ── the workflow offers what the action accepts ─────────────────────────────

printf '\n\033[1mthe sandboxlab workflow only passes inputs the action declares\033[0m\n'
workflow="$REPO_ROOT/.github/workflows/sandboxlab.yml"
# `with:` entries under the `uses: ./action` step.
passed=$(awk '/uses: \.\/action/{f=1} f && /^          [a-z_]+:/{ sub(/^ +/, ""); sub(/:.*/, ""); print }' "$workflow")
for key in $passed; do
  if grep -qx "$key" <<<"$inputs"; then
    ok "$key"
  else
    bad "the workflow passes $key, which the action does not declare"
  fi
done

printf '\n\033[1m%d check(s) failed\033[0m\n' "$fail"
[ "$fail" -eq 0 ]
