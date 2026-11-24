#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root/scenarios"
FAULTVANE_SCENARIOS=1 go test -count=1 -timeout 60m -v -run TestScenarios . "$@"
