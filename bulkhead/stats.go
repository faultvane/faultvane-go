package bulkhead

type Stats struct {
	Permitted int64
	Rejected  int64
	Waited    int64
	InFlight  int
	Waiting   int
}

func (b *Bulkhead) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{
		Permitted: b.permitted,
		Rejected:  b.rejected,
		Waited:    b.waited,
		InFlight:  b.inFlight,
		Waiting:   b.waiters.Len(),
	}
}
