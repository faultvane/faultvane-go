package concurrencylimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Time{} }

func TestDefaults(t *testing.T) {
	t.Run("CL-20 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.initialLimit != 20 || c.minLimit != 1 || c.maxLimit != 200 {
			t.Errorf("limits = %d, %d, %d", c.initialLimit, c.minLimit, c.maxLimit)
		}
		if c.backoffRatio != 0.9 || c.latencyThresholdSet {
			t.Errorf("backoffRatio = %v, latency threshold on = %v", c.backoffRatio, c.latencyThresholdSet)
		}
		if c.clock != faultvane.SystemClock() {
			t.Error("the clock is not the system clock")
		}
		if !c.overloadIf(context.DeadlineExceeded) || !c.overloadIf(fmt.Errorf("call: %w", context.DeadlineExceeded)) {
			t.Error("an ended deadline is not an overload signal")
		}
		if c.overloadIf(nil) || c.overloadIf(context.Canceled) || c.overloadIf(errors.New("x")) {
			t.Error("a result without an ended deadline is an overload signal")
		}
	})

	t.Run("CL-26 TK-20 the default logger is the default of log/slog and no observer is set", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil || c.logger != slog.Default() || c.observer != nil {
			t.Fatalf("logger = %v, observer = %v, err = %v", c.logger, c.observer, err)
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("CL-20 each option sets its value", func(t *testing.T) {
		c, err := newConfig([]Option{
			WithInitialLimit(5),
			WithMinLimit(2),
			WithMaxLimit(9),
			WithLatencyThreshold(time.Second),
			WithBackoffRatio(0.5),
			WithOverloadIf(func(error) bool { return true }),
			WithClock(fixedClock{}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.initialLimit != 5 || c.minLimit != 2 || c.maxLimit != 9 || c.backoffRatio != 0.5 {
			t.Errorf("config = %+v", c)
		}
		if !c.latencyThresholdSet || c.latencyThreshold != time.Second {
			t.Errorf("latency threshold = %v, on = %v", c.latencyThreshold, c.latencyThresholdSet)
		}
		if !c.overloadIf(nil) || c.clock != (fixedClock{}) {
			t.Error("OverloadIf or the clock is not the given value")
		}
	})

	t.Run("CL-26 the options set the observer and the logger", func(t *testing.T) {
		observer := &events{}
		c, err := newConfig([]Option{WithObserver(observer), WithLogger(quiet)})
		if err != nil || c.logger != quiet || c.observer != Observer(observer) {
			t.Fatalf("logger = %v, observer = %v, err = %v", c.logger, c.observer, err)
		}
	})

	t.Run("CL-21 the values at the limits of the ranges are valid", func(t *testing.T) {
		valid := [][]Option{
			{WithMinLimit(1), WithInitialLimit(1), WithMaxLimit(1)},
			{WithMinLimit(20), WithMaxLimit(20)},
			{WithLatencyThreshold(time.Nanosecond)},
			{WithBackoffRatio(math.SmallestNonzeroFloat64)},
			{WithBackoffRatio(math.Nextafter(1, 0))},
		}
		for i, opts := range valid {
			if _, err := New(opts...); err != nil {
				t.Errorf("case %d: %v", i, err)
			}
		}
	})
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		opts   []Option
		option string
	}{
		{"min limit 0", []Option{WithMinLimit(0)}, "MinLimit"},
		{"min limit below 0", []Option{WithMinLimit(-1), WithInitialLimit(-1)}, "MinLimit"},
		{"max limit below min limit", []Option{WithMinLimit(5), WithInitialLimit(5), WithMaxLimit(4)}, "MaxLimit"},
		{"initial limit below min limit", []Option{WithMinLimit(5), WithInitialLimit(4)}, "InitialLimit"},
		{"initial limit above max limit", []Option{WithInitialLimit(201)}, "InitialLimit"},
		{"default initial limit above max limit", []Option{WithMaxLimit(19)}, "InitialLimit"},
		{"latency threshold 0", []Option{WithLatencyThreshold(0)}, "LatencyThreshold"},
		{"latency threshold below 0", []Option{WithLatencyThreshold(-time.Second)}, "LatencyThreshold"},
		{"backoff ratio 0", []Option{WithBackoffRatio(0)}, "BackoffRatio"},
		{"backoff ratio 1", []Option{WithBackoffRatio(1)}, "BackoffRatio"},
		{"backoff ratio below 0", []Option{WithBackoffRatio(-0.5)}, "BackoffRatio"},
		{"backoff ratio not a number", []Option{WithBackoffRatio(math.NaN())}, "BackoffRatio"},
		{"nil OverloadIf", []Option{WithOverloadIf(nil)}, "OverloadIf"},
		{"nil clock", []Option{WithClock(nil)}, "Clock"},
		{"CL-26 nil logger", []Option{WithLogger(nil)}, "Logger"},
		{"CL-26 nil observer", []Option{WithObserver(nil)}, "Observer"},
	}
	for _, c := range cases {
		t.Run("CL-21 TK-21 "+c.name, func(t *testing.T) {
			l, err := New(c.opts...)
			if l != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", l, err)
			}
			if want := "faultvane: invalid configuration: " + c.option + " must "; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("error = %q, want the prefix %q", err, want)
			}
		})
	}
}
