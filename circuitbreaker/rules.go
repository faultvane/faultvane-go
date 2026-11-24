package circuitbreaker

type rules struct {
	failureThreshold int64
	minimumCalls     int64
	failureRate      float64
	slowCallRate     float64
}

func (r rules) exceeded(calls, failures, slowCalls int64) bool {
	if r.failureThreshold > 0 && failures >= r.failureThreshold {
		return true
	}
	if calls < r.minimumCalls {
		return false
	}
	if float64(failures) >= r.failureRate*float64(calls) {
		return true
	}
	return r.slowCallRate > 0 && float64(slowCalls) >= r.slowCallRate*float64(calls)
}
