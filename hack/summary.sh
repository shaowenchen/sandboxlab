#!/usr/bin/env bash
#
# Publish the environment's address, key and catalog to the job summary.
#
# The summary is where the deliverable lives: the run log scrolls away, and the
# Summary is what someone returns to when they need the link again. It reads the
# facts environment.sh recorded rather than reconstructing them, so what is shown
# is what the process that built the environment actually knew.
set -euo pipefail

RESULT_ENV="${SANDBOXLAB_RESULT_ENV:-${SANDBOXLAB_RUNTIME_DIR:-$PWD/.sandboxlab}/result.env}"

# The file is `KEY=value` with the values shell-quoted, so it is read by
# sourcing it. Anything missing is simply unset.
if [ -f "$RESULT_ENV" ]; then
  # shellcheck disable=SC1090  # a path built at runtime, by design
  . "$RESULT_ENV"
fi

console_url="${SANDBOX_CONSOLE_URL:-}"
api_key="${SANDBOX_API_KEY:-}"
catalog="${SANDBOX_CATALOG:-}"
supplied="${SANDBOX_KEY_SUPPLIED:-unknown}"
# Named by environment.sh rather than here: which Secret the key ended up in
# depends on whether this run was given one, and the summary only needs to say
# where it is. The fallback is for a result file written before this was
# recorded — a failed run has whatever it got to — so the row still says
# something true rather than naming an empty Secret.
key_secret="${SANDBOX_KEY_SECRET:-sandboxlab-api-key}"
# Where that Secret lives, for the read-back commands. Read here simply so the
# two `kubectl` lines below do not each carry their own default.
namespace="${SANDBOX_NAMESPACE:-ops-system}"

# Whether the key may be printed. A generated key exists nowhere else and dies
# with the environment, so the summary is the only place it can be handed over;
# a supplied one outlives the run and is already held by whoever set it, so
# publishing it here would publish a live credential.
#
# Decided by the key's origin, never guessed from the value — and only a
# recorded "false" counts as generated. Anything else, including a result file
# that never said, keeps the key out of the summary: the cost of being wrong
# that way is a reader who has to look in the cluster, and the cost of the
# other is a live credential in a public run.
show_key=false
if [ "$supplied" = "false" ]; then
  show_key=true
fi

# What stands in for a kept key, at the width the console draws a credential
# (MASK_WIDTH in internal/console/static) — fixed rather than one bullet per
# character, so the mask says "a key exists and is not shown" and not how long
# it is.
mask='••••••••••••••••••••••••••••••••'

{
  echo "## Sandboxes are ready"
  echo
  if [ -n "$console_url" ]; then
    echo "**Open the console:** <${console_url}>"
    echo
    if [ "$show_key" = "true" ] && [ -n "$api_key" ]; then
      echo "It asks for the API key below. Both are kept in your browser, so it is"
      echo "entered once."
    elif [ -n "$api_key" ]; then
      # Saying "the key below" when the row is masked would send a reader
      # looking for a value that is deliberately not there.
      echo "It asks for the API key this run was given. That key is not printed in"
      echo "this summary — the row below names the Secret it is in."
    else
      echo "It asks for the API key configured for this environment."
    fi
  else
    echo "**No console address was published.** The environment did not get as far as"
    echo "learning one; check the \`tunnel\` step in the log above."
  fi
  echo
  echo "| | |"
  echo "|---|---|"
  if [ -n "$api_key" ]; then
    if [ "$show_key" = "true" ]; then
      echo "| API key | \`${api_key}\` |"
    else
      # The value is deliberately absent. The row stays so a reader can see the
      # environment has a key, and where it is, without the run being the thing
      # that publishes it.
      echo "| API key | \`${mask}\` — supplied for this run, in Secret \`${key_secret}\` |"
    fi
  fi
  if [ -n "$console_url" ]; then
    echo "| Console | <${console_url}> |"
  fi
  if [ -n "$catalog" ]; then
    echo "| Templates | $(tr ',' ' ' <<<"$catalog" | sed 's/ /, /g') |"
  fi
  if [ -n "${SANDBOX_IMAGE:-}" ]; then
    echo "| Image | \`${SANDBOX_IMAGE}\` (built from this commit) |"
  fi
  echo "| Cluster | kind |"
  # Whether the cluster serves the resource metrics API, which is what a
  # sandbox's CPU and memory are read from. Said out loud because its absence is
  # silent everywhere else: the console's Metrics panel and
  # `GET /api/v1/sandboxes/{id}/usage` both report usage as simply unavailable,
  # which is a legitimate answer on a cluster without a metrics API and so reads
  # as "nothing to show" rather than as something that did not come up.
  if [ -n "${SANDBOX_METRICS:-}" ]; then
    echo "| Metrics | yes — CPU and memory are readable in the console |"
  else
    echo "| Metrics | no — the metrics API did not come up, so \`/usage\` reports nothing |"
  fi
  echo
  echo "### Create a sandbox"
  echo
  echo '```bash'
  if [ -n "$console_url" ]; then
    echo "export SANDBOX_URL='${console_url}'"
  fi
  if [ "$show_key" = "true" ] && [ -n "$api_key" ]; then
    echo "export SANDBOX_KEY='${api_key}'"
  elif [ -n "$api_key" ]; then
    # The CLI has to be given the key somehow, and the notes above say where it
    # is — this line is the shape of the command, not the value.
    echo "export SANDBOX_KEY=\"\$(kubectl -n ${namespace} get secret ${key_secret} -o jsonpath='{.data.api-key}' | base64 -d)\""
  fi
  echo
  echo "# What can be created:"
  echo "sandbox catalog"
  echo
  echo "# A complete workspace — shell, files, browser and desktop:"
  echo "sandbox create -t agent-infra --name demo --wait"
  echo "sandbox url demo"
  echo
  echo "# Or open the console and click Create."
  echo '```'
  echo
  echo "A sandbox is reachable at \`${console_url}/sandbox/<name>/<port>/\`, and is"
  echo "deleted when its lifetime runs out — \`sandbox rm <name>\` ends one early."
  if [ "$show_key" != "true" ] && [ -n "$api_key" ]; then
    echo
    echo "The API key is the one this run was given, not one it made up, so it is"
    echo "not printed above — a run that showed it would publish a key that is"
    echo "still live after the run ends. Read it on the runner with:"
    echo
    echo '```bash'
    echo "kubectl -n ${namespace} get secret ${key_secret} -o jsonpath='{.data.api-key}' | base64 -d"
    echo '```'
  fi
  echo
  echo "The CLI is \`sandbox\` from"
  echo "[sandboxlab](https://github.com/shaowenchen/sandboxlab); the whole"
  echo "environment is discarded when this run ends, so cancel the workflow to end it early."
} >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# A notice in the run's timeline, so the link is visible without opening the
# summary — the same courtesy the tunnel agent's own output would have given.
if [ -n "$console_url" ]; then
  echo "::notice title=Sandboxes are ready::${console_url}"
fi
if [ "$show_key" = "true" ] && [ -n "$api_key" ]; then
  # Deliberately NOT ::add-mask::. A key this run generated is the deliverable,
  # and masking it would hide it from the very summary that exists to show it.
  echo "::notice title=Sandbox API key::${api_key}"
elif [ -n "$api_key" ]; then
  # And no notice at all for a supplied key: a notice line is mirrored into the
  # run's log, which is the one place this key must not reach. The summary above
  # says the key exists and where it is.
  echo "::notice title=Sandbox API key::supplied for this run; read it from Secret ${key_secret} on the runner"
fi
