package loadshed

type Stats struct {
	Admitted [priorities]int64
	Shed     [priorities]int64
	InFlight int
}

func (s *Shedder) Stats() Stats {
	stats := Stats{InFlight: s.InFlight()}
	for p := range priorities {
		stats.Admitted[p] = s.admitted[p].Load()
		stats.Shed[p] = s.shed[p].Load()
	}
	return stats
}
