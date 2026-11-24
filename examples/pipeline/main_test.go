package main

import (
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	want := `healthy dependency: receipt (circuit closed)
outage, request 1: failed (circuit closed)
outage, request 2: failed (circuit closed)
outage, request 3: failed (circuit open)
outage, request 4: rejected without a call (circuit open)
dependency recovered, open duration not ended: rejected without a call (circuit open)
after the open duration: receipt (circuit closed)
normal operation: receipt (circuit closed)
`
	if out.String() != want {
		t.Fatalf("QA-13 output:\n%s\nwant:\n%s", out.String(), want)
	}
}
