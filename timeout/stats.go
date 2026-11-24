package timeout

type Stats struct {
	Calls    int64
	Timeouts int64
}

func (t *Timeout) Stats() Stats {
	return Stats{Calls: t.calls.Load(), Timeouts: t.timeouts.Load()}
}
