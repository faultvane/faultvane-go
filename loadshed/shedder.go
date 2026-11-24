package loadshed

import (
	"context"
	"sync/atomic"

	"github.com/faultvane/faultvane-go"
)

type Shedder struct {
	maxInFlight float64
	thresholds  [priorities]float64
	inFlight    atomic.Int64
}

var _ faultvane.Policy = (*Shedder)(nil)

func New(opts ...Option) (*Shedder, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}
	return &Shedder{maxInFlight: float64(cfg.maxInFlight), thresholds: cfg.thresholds}, nil
}

func (s *Shedder) InFlight() int { return int(s.inFlight.Load()) }

func (s *Shedder) admit(p Priority) bool {
	for {
		inFlight := s.inFlight.Load()
		if float64(inFlight)/s.maxInFlight >= s.thresholds[p] {
			return false
		}
		if s.inFlight.CompareAndSwap(inFlight, inFlight+1) {
			return true
		}
	}
}

func (s *Shedder) Do(ctx context.Context, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.admit(PriorityFrom(ctx)) {
		return ErrShed
	}
	defer s.inFlight.Add(-1)
	return call(ctx)
}
