#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
compose="docker compose -f $root/docker-compose.test.yml"

$compose up -d --wait
if [ "${KEEP_STORES:-0}" != "1" ]; then
  trap '$compose down -v' EXIT
fi

$compose exec -T redis-cluster redis-cli cluster addslotsrange 0 16383 >/dev/null 2>&1 || true

for module in ${STORE_MODULES:-store/redis store/sql store/mongodb}; do
  echo "== $module"
  (cd "$root/$module" && sh "$root/scripts/coverage.sh")
done
