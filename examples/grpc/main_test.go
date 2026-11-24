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
	want := `healthy dependency: ok (circuit closed)
unknown service, request 1: NotFound (circuit closed)
unknown service, request 2: NotFound (circuit closed)
unknown service, request 3: NotFound (circuit closed)
unknown service, request 4: NotFound (circuit closed)
outage, request 1: Unavailable (circuit closed)
outage, request 2: Unavailable (circuit closed)
outage, request 3: Unavailable (circuit open)
outage, request 4: rejected without a call (circuit open)
dependency recovered, open duration not ended: rejected without a call (circuit open)
after the open duration: ok (circuit closed)
normal operation: ok (circuit closed)
`
	if out.String() != want {
		t.Fatalf("QA-13 output:\n%s\nwant:\n%s", out.String(), want)
	}
}
