package retry

type Stats struct {
	Calls       int64
	Retries     int64
	Failed      int64
	GateStops   int64
	BudgetStops int64
}

func (r *Retry) Stats() Stats {
	return Stats{
		Calls:       r.calls.Load(),
		Retries:     r.retries.Load(),
		Failed:      r.failed.Load(),
		GateStops:   r.gateStops.Load(),
		BudgetStops: r.budgetStops.Load(),
	}
}
