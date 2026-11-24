package circuitbreaker

import "testing"

func TestThresholdsExceeded(t *testing.T) {
	rate := Thresholds{MinimumCalls: 10, FailureRate: 0.5}
	count := Thresholds{MinimumCalls: 10, FailureRate: 1, FailureThreshold: 3}
	slow := Thresholds{MinimumCalls: 10, FailureRate: 1, SlowCallRate: 0.5}

	cases := []struct {
		name                       string
		rules                      Thresholds
		calls, failures, slowCalls int64
		want                       bool
	}{
		{"FD-2 failure rate below the threshold", rate, 10, 4, 0, false},
		{"FD-2 failure rate equal to the threshold", rate, 10, 5, 0, true},
		{"FD-2 failure rate above the threshold", rate, 10, 6, 0, true},
		{"FD-2 one call below the minimum number of calls", rate, 9, 9, 0, false},
		{"FD-2 calls equal to the minimum number of calls", rate, 10, 10, 0, true},
		{"FD-2 no calls", rate, 0, 0, 0, false},
		{"FD-1 failures below the count threshold", count, 2, 2, 0, false},
		{"FD-1 failures equal to the count threshold below the minimum number of calls", count, 3, 3, 0, true},
		{"FD-1 failures above the count threshold", count, 20, 4, 0, true},
		{"FD-1 count threshold off", rate, 9, 9, 0, false},
		{"FD-4 slow call rate below the threshold", slow, 10, 0, 4, false},
		{"FD-4 slow call rate equal to the threshold", slow, 10, 0, 5, true},
		{"FD-4 slow call rate below the minimum number of calls", slow, 9, 0, 9, false},
		{"FD-4 slow call rate off", rate, 10, 0, 10, false},
		{"FD-2 failure rate of 1 needs all calls to fail", count, 10, 2, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rules.Exceeded(c.calls, c.failures, c.slowCalls); got != c.want {
				t.Fatalf("Exceeded(%d, %d, %d) = %v, want %v", c.calls, c.failures, c.slowCalls, got, c.want)
			}
		})
	}
}
