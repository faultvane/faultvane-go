package ratelimit

import (
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

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

func TestStoreOptions(t *testing.T) {
	store := NewMemoryStore(faultvane.SystemClock())

	t.Run("RL-33 RL-34 TK-20 each store option has its documented default", func(t *testing.T) {
		c, err := newConfig([]Option{WithRate(40), WithStore(store), WithName("a")})
		if err != nil {
			t.Fatal(err)
		}
		if c.syncInterval != 100*time.Millisecond || c.storeTimeout != 50*time.Millisecond || c.localRate != 40 || c.faultThreshold != 3 {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("RL-42 TK-20 the default logger is the default of log/slog and no observer is set", func(t *testing.T) {
		c, err := newConfig([]Option{WithStore(store), WithName("a")})
		if err != nil || c.logger != slog.Default() || c.observer != nil {
			t.Fatalf("logger = %v, observer = %v, err = %v", c.logger, c.observer, err)
		}
	})

	t.Run("RL-42 the options set the observer and the logger", func(t *testing.T) {
		observer := &events{}
		c, err := newConfig([]Option{WithStore(store), WithName("a"), WithObserver(observer), WithLogger(quiet)})
		if err != nil || c.logger != quiet || c.observer != Observer(observer) {
			t.Fatalf("logger = %v, observer = %v, err = %v", c.logger, c.observer, err)
		}
	})

	t.Run("RL-42 a limiter without a store ignores the observer and the logger", func(t *testing.T) {
		if _, err := New(WithObserver(nil), WithLogger(nil)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("RL-32 RL-33 RL-34 each store option sets its value", func(t *testing.T) {
		c, err := newConfig([]Option{
			WithStore(store), WithName("azAZ09._-"), WithSyncInterval(time.Second), WithStoreTimeout(time.Second - 1),
			WithLocalRate(100), WithFaultThreshold(1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.store != Store(store) || c.name != "azAZ09._-" || c.syncInterval != time.Second || c.storeTimeout != time.Second-1 || c.localRate != 100 || c.faultThreshold != 1 {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("RL-32 a name of 64 characters is accepted", func(t *testing.T) {
		if _, err := newConfig([]Option{WithStore(store), WithName(strings.Repeat("a", 64))}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("RL-20 TK-30 a limiter without a store ignores the store options", func(t *testing.T) {
		limiter, err := New(WithName("a b"), WithSyncInterval(0), WithStoreTimeout(0), WithLocalRate(0), WithFaultThreshold(0))
		if err != nil || limiter.shared != nil {
			t.Fatalf("New() = %+v, %v", limiter, err)
		}
	})

	const nameRule = "Name must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'"
	cases := map[string]struct {
		opts []Option
		want string
	}{
		"RL-32 nil store":                              {[]Option{WithStore(nil)}, "Store must not be nil"},
		"RL-32 no name":                                {[]Option{WithName("")}, nameRule},
		"RL-32 name of 65 characters":                  {[]Option{WithName(strings.Repeat("a", 65))}, nameRule},
		"RL-32 name with a colon":                      {[]Option{WithName("a:b")}, nameRule},
		"RL-32 name outside the ASCII range":           {[]Option{WithName("café")}, nameRule},
		"RL-33 store timeout of zero":                  {[]Option{WithStoreTimeout(0)}, "StoreTimeout must be above 0"},
		"RL-33 interval equal to the store timeout":    {[]Option{WithSyncInterval(50 * time.Millisecond)}, "SyncInterval must be longer than StoreTimeout"},
		"RL-34 local rate of zero":                     {[]Option{WithLocalRate(0)}, "LocalRate must be above 0 and at most Rate"},
		"RL-34 local rate above the rate":              {[]Option{WithLocalRate(100.5)}, "LocalRate must be above 0 and at most Rate"},
		"RL-34 local rate that is NaN":                 {[]Option{WithLocalRate(math.NaN())}, "LocalRate must be above 0 and at most Rate"},
		"RL-34 fault threshold of zero":                {[]Option{WithFaultThreshold(0)}, "FaultThreshold must be 1 or more"},
		"RL-42 nil logger":                             {[]Option{WithLogger(nil)}, "Logger must not be nil"},
		"RL-42 nil observer":                           {[]Option{WithObserver(nil)}, "Observer must not be nil"},
		"RL-14 invalid rate of a limiter with a store": {[]Option{WithRate(0)}, "Rate must be above 0 and at most 1e9"},
	}
	for name, tc := range cases {
		t.Run(name+" is rejected TK-21", func(t *testing.T) {
			limiter, err := New(append([]Option{WithStore(store), WithName("a")}, tc.opts...)...)
			if limiter != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", limiter, err)
			}
			if !strings.HasSuffix(err.Error(), ": "+tc.want) {
				t.Fatalf("error = %q, want the reason %q", err, tc.want)
			}
		})
	}
}
