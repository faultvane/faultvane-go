package ratelimit

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/faultvanetest"
)

func TestDefaults(t *testing.T) {
	t.Run("RL-11 RL-12 RL-13 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.rate != 100 || c.burst != 100 || c.clock != faultvane.SystemClock() {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("RL-12 the default burst is the rate rounded up", func(t *testing.T) {
		for rate, want := range map[float64]int{0.001: 1, 1: 1, 2.5: 3, 7: 7, maxRate: maxRate} {
			c, err := newConfig([]Option{WithRate(rate)})
			if err != nil || c.burst != want {
				t.Errorf("rate %v: burst = %d, err = %v, want %d, nil", rate, c.burst, err, want)
			}
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("RL-11 RL-12 RL-13 each option sets its value", func(t *testing.T) {
		clock := faultvanetest.NewClock(testStart)
		c, err := newConfig([]Option{WithRate(2.5), WithBurst(1), WithClock(clock)})
		if err != nil {
			t.Fatal(err)
		}
		if c.rate != 2.5 || c.burst != 1 || c.clock != faultvane.Clock(clock) {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("RL-11 RL-12 the limits of the valid range are accepted", func(t *testing.T) {
		for name, opts := range map[string][]Option{
			"smallest rate": {WithRate(math.SmallestNonzeroFloat64)},
			"largest rate":  {WithRate(maxRate)},
			"largest burst": {WithBurst(math.MaxInt)},
		} {
			if _, err := New(opts...); err != nil {
				t.Errorf("%s: New() error = %v", name, err)
			}
		}
	})
}

func TestValidation(t *testing.T) {
	cases := map[string]struct {
		option Option
		want   string
	}{
		"rate of zero":         {WithRate(0), "Rate must be above 0 and at most 1e9"},
		"negative rate":        {WithRate(-1), "Rate must be above 0 and at most 1e9"},
		"rate above the limit": {WithRate(math.Nextafter(maxRate, math.Inf(1))), "Rate must be above 0 and at most 1e9"},
		"infinite rate":        {WithRate(math.Inf(1)), "Rate must be above 0 and at most 1e9"},
		"rate that is NaN":     {WithRate(math.NaN()), "Rate must be above 0 and at most 1e9"},
		"burst of zero":        {WithBurst(0), "Burst must be 1 or more"},
		"negative burst":       {WithBurst(-1), "Burst must be 1 or more"},
		"nil clock":            {WithClock(nil), "Clock must not be nil"},
	}
	for name, tc := range cases {
		t.Run("RL-14 TK-21 "+name+" is rejected", func(t *testing.T) {
			limiter, err := New(tc.option)
			if limiter != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", limiter, err)
			}
			if !strings.HasSuffix(err.Error(), ": "+tc.want) {
				t.Fatalf("error = %q, want the reason %q", err, tc.want)
			}
		})
	}
}
