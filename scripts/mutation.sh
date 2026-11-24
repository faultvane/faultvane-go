#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
if [ "$#" -eq 0 ]; then
  set -- circuitbreaker timeout retry bulkhead concurrencylimit loadshed ratelimit
fi
cd "$root/tools"
go run ./mutation -dir "$root" -min "${MUTATION_MINIMUM:-90}" -workers "${MUTATION_WORKERS:-3}" -timeout "${MUTATION_TIMEOUT:-45s}" "$@"
