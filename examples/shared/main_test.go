package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	address := os.Getenv("FAULTVANE_REDIS_ADDR")
	if address == "" {
		address = "127.0.0.1:6399"
	}
	var out strings.Builder
	if err := run(&out, address, fmt.Sprintf("example-test-%d", time.Now().UnixNano())); err != nil {
		t.Fatal(err)
	}
	want := `instance 1: 1 failure, local view closed
instance 2: 1 failure, local view closed
instance 3: 1 failure, local view closed
instance 4: 1 failure, local view closed
instance 5: 1 failure, local view closed
5 failures in the shared window: the circuit is open for all 5 instances
next call on instance 1: circuitbreaker: circuit is open: faultvane: call rejected
`
	if out.String() != want {
		t.Fatalf("QA-13 output:\n%s\nwant:\n%s", out.String(), want)
	}
}
