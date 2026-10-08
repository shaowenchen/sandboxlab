#!/usr/bin/env bash
#
# Check that a key this run was *given* never reaches the run's output.
#
# This is the one rule in the debugger that cannot be checked by looking at a
# rendered artifact: the same line prints a key or a mask depending on where the
# key came from, and a mistake shows up only in a public run that has already
# published a live credential. So it is checked by running the publisher twice,
# with each origin, and asserting the value is in the output a generated key
# produces and absent from the output a supplied one does.
#
# What it runs is hack/summary.sh, the process that writes the job Summary and
# the notice lines. environment.sh's own banner follows the same recorded flag —
# the assertions below read it from the source, because running the banner would
# mean standing up a cluster.
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SUMMARY="$REPO_ROOT/hack/summary.sh"
ENVIRONMENT="$REPO_ROOT/hack/environment.sh"

fail=0
pass=0
ok()  { pass=$((pass + 1)); printf '  \033[32mok\033[0m   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  \033[31mFAIL\033[0m %s\n' "$1"; }

# Two keys that are obviously not each other, and long enough to look real: the
# point is to search the output for one, and a short shared substring would make
# the search meaningless.
generated_key="GENERATED0000000000000000000000000000000000000000000000000000000000"
supplied_key="SUPPLIED11111111111111111111111111111111111111111111111111111111111"

runtime=$(mktemp -d)
trap 'rm -rf "$runtime"' EXIT

# Only the facts summary.sh reads; anything missing is simply unset there.
write_result() {
  cat > "$1" <<EOF
SANDBOX_API_KEY='$2'
SANDBOX_KEY_SUPPLIED='$3'
SANDBOX_KEY_SECRET='sandboxlab-api-key'
SANDBOX_CONSOLE_URL='https://sandboxlab-1.example.com/sandboxlab'
SANDBOX_NAMESPACE='ops-system'
SANDBOX_READY='true'
EOF
}

render() {
  SANDBOXLAB_RESULT_ENV="$1" bash "$SUMMARY" 2>&1
}

# ── the generated key is the deliverable, and is printed ────────────────────

write_result "$runtime/generated.env" "$generated_key" "false"
generated_out=$(render "$runtime/generated.env")

if grep -qF "$generated_key" <<<"$generated_out"; then
  ok "a generated key is printed in the summary"
else
  bad "a generated key is missing from the summary, so nobody can read it"
fi
if grep -qF "export SANDBOX_KEY='${generated_key}'" <<<"$generated_out"; then
  ok "the CLI recipe carries a generated key"
else
  bad "the CLI recipe does not carry a generated key"
fi
if grep -qF "::notice title=Sandbox API key::${generated_key}" <<<"$generated_out"; then
  ok "a generated key gets a notice line"
else
  bad "a generated key gets no notice line"
fi

# ── a supplied key is not this run's to print ───────────────────────────────

write_result "$runtime/supplied.env" "$supplied_key" "true"
supplied_out=$(render "$runtime/supplied.env")

if grep -qF "$supplied_key" <<<"$supplied_out"; then
  bad "a supplied key reached the summary, which outlives the run"
else
  ok "a supplied key is absent from the summary"
fi
if grep -qF "supplied for this run" <<<"$supplied_out"; then
  ok "the summary says the key was supplied rather than generated"
else
  bad "the summary does not say that the key was supplied"
fi
# The row has to name where the key is: a summary that says only "a key exists"
# leaves a reader on the runner with no way to find it.
if grep -qF 'sandboxlab-api-key' <<<"$supplied_out"; then
  ok "the summary names the Secret holding the supplied key"
else
  bad "the summary does not say where the supplied key is"
fi
if grep -qF 'export SANDBOX_KEY="$(kubectl' <<<"$supplied_out"; then
  ok "the CLI recipe reads a supplied key back rather than embedding it"
else
  bad "the CLI recipe does not read a supplied key back"
fi

# ── and a result file that never said keeps it out too ──────────────────────

# The third case, and the one a real run can produce: the key was recorded but
# the flag was not, because the run failed before that line. The safe reading is
# the one that leaks nothing.
cat > "$runtime/unknown.env" <<EOF
SANDBOX_API_KEY='$supplied_key'
SANDBOX_CONSOLE_URL='https://sandboxlab-1.example.com/sandboxlab'
EOF
unknown_out=$(render "$runtime/unknown.env")
if grep -qF "$supplied_key" <<<"$unknown_out"; then
  bad "a key of unrecorded origin reached the summary"
else
  ok "a key of unrecorded origin stays out of the summary"
fi

# ── the runner's own mask is armed, but only under Actions ──────────────────

# A supplied key arrives as a workflow input, which GitHub does not mask the way
# it masks a `secrets.*` value, so the script asks the runner to mask it. That
# call has to be guarded by GITHUB_ACTIONS: run by hand, `::add-mask::<the key>`
# is printed to stdout, which would leak the key in the very line meant to
# protect it. Checked by running the block rather than by reading it.
add_mask_block=$(awk '
  /^if \[ "\$KEY_SUPPLIED" = "true" \] && \[ -n "\$\{GITHUB_ACTIONS:-\}" \]; then$/ { buf = $0 ORS; collecting = 1; next }
  collecting { buf = buf $0 ORS }
  collecting && /^fi$/ { printf "%s", buf; exit }
' "$ENVIRONMENT")
if [ -z "$add_mask_block" ]; then
  bad "could not find the add-mask call in $ENVIRONMENT"
else
  # Under Actions: the directive is emitted, and the key is in it.
  with_actions=$(SANDBOXLAB_API_KEY="$supplied_key" KEY_SUPPLIED=true GITHUB_ACTIONS=true \
    bash -c "$add_mask_block" 2>&1)
  if grep -qF "::add-mask::${supplied_key}" <<<"$with_actions"; then
    ok "the key is handed to the runner's masker under Actions"
  else
    bad "the key is not masked under Actions"
  fi
  # Not under Actions: nothing is printed at all, so the literal directive —
  # which carries the key — cannot reach stdout.
  without_actions=$(SANDBOXLAB_API_KEY="$supplied_key" KEY_SUPPLIED=true \
    bash -c "$add_mask_block" 2>&1)
  if [ -n "$without_actions" ]; then
    bad "the add-mask line prints when not running under Actions: ${without_actions}"
  else
    ok "nothing is printed when the script is run by hand"
  fi
fi

# ── the banner follows the same flag ────────────────────────────────────────

# The banner is printed after a cluster is up, so it is not run here. What is
# run is the block that decides the two lines it prints: it is lifted out of the
# script and executed with both origins, which is the same code the banner uses.
# A source-level grep would pass on a branch whose printf interpolated the wrong
# variable, and that is exactly the mistake worth catching.
# The 854-line block is the one that builds the banner's two lines, and it is
# identified by containing `key_line=` rather than by its line number — the
# install section has if-blocks on the same condition, and only this one decides
# what gets printed.
banner_lines=$(awk '
  /\$KEY_SUPPLIED.*then/ { buf = $0 ORS; collecting = 1; next }
  collecting { buf = buf $0 ORS }
  collecting && /^fi$/ { if (buf ~ /key_line=/) { printf "%s", buf; exit } ; collecting = 0; buf = "" }
' "$ENVIRONMENT")
if [ -z "$banner_lines" ]; then
  bad "could not find the banner's key branch in $ENVIRONMENT"
else
  # The mask and the Secret are read from the script so this stays honest if
  # either changes — the assertions below are about the *key*, not the wording.
  mask=$(grep -o "KEY_MASK='[^']*'" "$ENVIRONMENT" | head -1 | sed "s/^KEY_MASK='//; s/'$//")
  run_banner() {
    API_KEY="$supplied_key" KEY_MASK="$mask" secret_name="sandboxlab-api-key" \
    SANDBOXLAB_NAMESPACE="ops-system" KEY_SUPPLIED="$1" \
      bash -c "$banner_lines"$'\nprintf "%s\\n%s\\n" "$key_line" "$key_export"'
  }
  banner_supplied=$(run_banner true)
  if grep -qF "$supplied_key" <<<"$banner_supplied"; then
    bad "the banner prints a supplied key"
  else
    ok "the banner does not print a supplied key"
  fi
  if grep -qF "$mask" <<<"$banner_supplied"; then
    ok "the banner masks a supplied key"
  else
    bad "the banner does not mask a supplied key"
  fi
  if grep -qF 'sandboxlab-api-key' <<<"$banner_supplied"; then
    ok "the banner names the Secret holding a supplied key"
  else
    bad "the banner does not say where a supplied key is"
  fi

  banner_generated=$(run_banner false)
  if grep -qF "$supplied_key" <<<"$banner_generated"; then
    ok "the banner prints a generated key"
  else
    bad "the banner does not print a generated key"
  fi
fi

# The mask is a literal in each script — summary.sh writes the Summary, the
# banner is printed from environment.sh, and neither sources the other — so the
# only thing worth checking is that the two are the same. A value that masks one
# way in the Summary and another in the banner reads as two different keys.
env_mask=$(grep -o "KEY_MASK='[^']*'" "$ENVIRONMENT" | head -1 | sed "s/^KEY_MASK='//; s/'$//")
sum_mask=$(grep -o "mask='[^']*'" "$SUMMARY" | head -1 | sed "s/^mask='//; s/'$//")
if [ -z "$env_mask" ] || [ -z "$sum_mask" ]; then
  bad "could not read a mask from one of the scripts"
elif [ "$env_mask" = "$sum_mask" ]; then
  ok "the banner and the summary mask with the same string"
else
  bad "the masks differ: banner '${env_mask}', summary '${sum_mask}'"
fi

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
