#!/usr/bin/env bash
#
# Package the chart into a tarball under dist/, for a release to attach.
#
# The version comes from Chart.yaml rather than an argument, so the artifact is
# named for what it actually contains. `helm package` refuses to overwrite, so
# the output is removed first: repackaging the same version while iterating
# should replace the file, not fail.
set -euo pipefail

REPO_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
CHART="$REPO_ROOT/charts/sandbox"
DIST="$REPO_ROOT/dist"

version=$(grep '^version:' "$CHART/Chart.yaml" | awk '{print $2}')
if [ -z "$version" ]; then
  echo "could not read the chart version from $CHART/Chart.yaml" >&2
  exit 1
fi

mkdir -p "$DIST"
rm -f "$DIST/sandbox-$version.tgz"

helm package "$CHART" --destination "$DIST"
ls -l "$DIST/sandbox-$version.tgz"
