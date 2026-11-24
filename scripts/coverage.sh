#!/bin/sh
set -eu

total_minimum="${COVERAGE_TOTAL_MINIMUM:-98.0}"
package_minimum="${COVERAGE_PACKAGE_MINIMUM:-95.0}"
profile="${COVERAGE_PROFILE:-coverage.out}"

if [ "${COVERAGE_SKIP_TESTS:-0}" != "1" ]; then
  go test -race -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$profile" ./...
fi

blocks=$(grep -vc '^mode:' "$profile" || true)
if [ "$blocks" -eq 0 ]; then
  echo "coverage: the module has no statements"
  exit 0
fi

awk -v total_minimum="$total_minimum" -v package_minimum="$package_minimum" '
NR > 1 {
  split($1, location, ":")
  file = location[1]
  parts = split(file, path, "/")
  package = substr(file, 1, length(file) - length(path[parts]) - 1)
  if (!($1 in statements)) {
    statements[$1] = $2
    owner[$1] = package
  }
  if ($3 > 0) {
    covered[$1] = 1
  }
}
END {
  for (block in statements) {
    package_total[owner[block]] += statements[block]
    all_total += statements[block]
    if (block in covered) {
      package_covered[owner[block]] += statements[block]
      all_covered += statements[block]
    }
  }
  failed = 0
  for (package in package_total) {
    percent = 100 * package_covered[package] / package_total[package]
    status = "ok"
    if (percent < package_minimum) {
      status = "BELOW MINIMUM"
      failed = 1
    }
    printf "%-70s %6.1f%%  %s\n", package, percent, status
  }
  percent = 100 * all_covered / all_total
  printf "total %.1f%% (minimum %.1f%%, minimum for each package %.1f%%)\n", percent, total_minimum, package_minimum
  if (percent < total_minimum) {
    failed = 1
  }
  exit failed
}' "$profile"
