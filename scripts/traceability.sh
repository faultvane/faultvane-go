#!/bin/sh
set -eu

implemented="scripts/implemented-requirements.txt"
documents=$(ls docs/requirements.md docs/*/requirements.md)
pattern='(TK|SM|DS|GEN|FD|HO|DG|RC|RF|KEY|TTL|PI|API|ERR|CFG|OB|SEC|PF|AT|QA|TO|RT|BH|CL|RL|LS)-[0-9]+'
failed=0

defined=$(grep -ohE "^\| $pattern \|" $documents | grep -oE "$pattern" | sort -u)
tested=$(grep -rohE --include='*_test.go' "$pattern" . | sort -u)

for id in $(cat "$implemented"); do
  if ! echo "$defined" | grep -qx "$id"; then
    echo "traceability: $id is in $implemented but not in the requirements documents"
    failed=1
  fi
  if ! echo "$tested" | grep -qx "$id"; then
    echo "traceability: $id has no test"
    failed=1
  fi
done

for id in $tested; do
  if ! echo "$defined" | grep -qx "$id"; then
    echo "traceability: a test names $id, but the requirements documents do not define it"
    failed=1
  fi
done

if [ "$failed" -eq 0 ]; then
  echo "traceability: $(wc -l < "$implemented" | tr -d ' ') implemented requirements have tests"
fi
exit "$failed"
