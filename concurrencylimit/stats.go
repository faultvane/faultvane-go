package concurrencylimit

type Stats struct {
	Permitted int64
	Rejected  int64
	Decreases int64
	Limit     int
	InFlight  int
}

func (l *Limiter) Stats() Stats {
	return Stats{
		Permitted: l.permitted.Load(),
		Rejected:  l.rejected.Load(),
		Decreases: l.decreases.Load(),
		Limit:     l.Limit(),
		InFlight:  l.InFlight(),
	}
}
