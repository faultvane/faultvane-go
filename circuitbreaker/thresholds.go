package circuitbreaker

type Thresholds struct {
	FailureThreshold int64
	MinimumCalls     int64
	FailureRate      float64
	SlowCallRate     float64
}

func (t Thresholds) Exceeded(calls, failures, slowCalls int64) bool {
	if t.FailureThreshold > 0 && failures >= t.FailureThreshold {
		return true
	}
	if calls < t.MinimumCalls {
		return false
	}
	if float64(failures) >= t.FailureRate*float64(calls) {
		return true
	}
	return t.SlowCallRate > 0 && float64(slowCalls) >= t.SlowCallRate*float64(calls)
}
