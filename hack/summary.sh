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

{
  echo "## Sandboxes are ready"
  echo
  if [ -n "$console_url" ]; then
    echo "**Open the console:** <${console_url}>"
    echo
    echo "It asks for the API key below. Both are kept in your browser, so it is"
    echo "entered once."
  else
    echo "**No console address was published.** The environment did not get as far as"
    echo "learning one; check the \`tunnel\` step in the log above."
  fi
  echo
  echo "| | |"
  echo "|---|---|"
  if [ -n "$api_key" ]; then
    echo "| API key | \`${api_key}\` |"
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
  if [ -n "$api_key" ]; then
    echo "export SANDBOX_KEY='${api_key}'"
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
if [ -n "$api_key" ]; then
  # Deliberately NOT ::add-mask::. The key is the deliverable, and masking it
  # would hide it from the very summary that exists to show it.
  echo "::notice title=Sandbox API key::${api_key}"
fi
