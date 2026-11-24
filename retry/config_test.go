package retry

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func TestDefaults(t *testing.T) {
	t.Run("RT-15 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.maxAttempts != 3 || c.initialBackoff != 100*time.Millisecond || c.maxBackoff != 10*time.Second {
			t.Errorf("maxAttempts = %d, backoff = %v to %v", c.maxAttempts, c.initialBackoff, c.maxBackoff)
		}
		if !c.retryIf(errors.New("x")) || !c.gate() || c.budgetSet {
			t.Error("the retry rule, the gate, or the budget is not the default")
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("RT-14 each option sets its value", func(t *testing.T) {
		c, err := newConfig([]Option{
			WithMaxAttempts(1),
			WithBackoff(time.Nanosecond, 2*time.Nanosecond),
			WithRetryIf(func(error) bool { return false }),
			WithGate(func() bool { return false }),
			WithBudget(1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.maxAttempts != 1 || c.initialBackoff != 1 || c.maxBackoff != 2 {
			t.Errorf("maxAttempts = %d, backoff = %v to %v", c.maxAttempts, c.initialBackoff, c.maxBackoff)
		}
		if c.retryIf(errors.New("x")) || c.gate() || !c.budgetSet || c.budgetRatio != 1 {
			t.Error("the retry rule, the gate, or the budget is not set")
		}
	})
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		opts   []Option
		option string
	}{
		{"max attempts 0", []Option{WithMaxAttempts(0)}, "MaxAttempts"},
		{"max attempts below 0", []Option{WithMaxAttempts(-1)}, "MaxAttempts"},
		{"initial backoff 0", []Option{WithBackoff(0, time.Second)}, "Backoff"},
		{"initial backoff below 0", []Option{WithBackoff(-time.Second, time.Second)}, "Backoff"},
		{"maximum backoff below the initial backoff", []Option{WithBackoff(2, 1)}, "Backoff"},
		{"nil retry rule", []Option{WithRetryIf(nil)}, "RetryIf"},
		{"nil gate", []Option{WithGate(nil)}, "Gate"},
		{"budget 0", []Option{WithBudget(0)}, "Budget"},
		{"budget below 0", []Option{WithBudget(-0.1)}, "Budget"},
		{"budget above 1", []Option{WithBudget(math.Nextafter(1, 2))}, "Budget"},
		{"budget not a number", []Option{WithBudget(math.NaN())}, "Budget"},
		{"budget without a limit", []Option{WithBudget(math.Inf(1))}, "Budget"},
	}
	for _, c := range cases {
		t.Run("RT-14 TK-21 "+c.name, func(t *testing.T) {
			r, err := New(c.opts...)
			if r != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", r, err)
			}
			if want := "faultvane: invalid configuration: " + c.option + " must "; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("err = %q, want the prefix %q", err, want)
			}
		})
	}

	valid := []struct {
		name string
		opts []Option
	}{
		{"max attempts 1", []Option{WithMaxAttempts(1)}},
		{"largest max attempts", []Option{WithMaxAttempts(math.MaxInt)}},
		{"shortest backoff", []Option{WithBackoff(time.Nanosecond, time.Nanosecond)}},
		{"longest backoff", []Option{WithBackoff(math.MaxInt64, math.MaxInt64)}},
		{"budget 1", []Option{WithBudget(1)}},
		{"smallest budget", []Option{WithBudget(math.SmallestNonzeroFloat64)}},
	}
	for _, c := range valid {
		t.Run("RT-14 valid: "+c.name, func(t *testing.T) {
			if _, err := New(c.opts...); err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
		})
	}
}

func TestBackoffCeiling(t *testing.T) {
	c := config{initialBackoff: 100 * time.Millisecond, maxBackoff: 10 * time.Second}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 10 * time.Second, 10 * time.Second}
	for i, ceiling := range want {
		if got := c.backoffCeiling(i + 1); got != ceiling {
			t.Errorf("RT-8 backoffCeiling(%d) = %v, want %v", i+1, got, ceiling)
		}
	}

	t.Run("RT-8 the ceiling is equal to the maximum when the doubled value is equal to it", func(t *testing.T) {
		c := config{initialBackoff: time.Second, maxBackoff: 4 * time.Second}
		if got := c.backoffCeiling(3); got != 4*time.Second {
			t.Fatalf("backoffCeiling(3) = %v, want 4s", got)
		}
	})

	t.Run("RT-8 the ceiling does not overflow", func(t *testing.T) {
		c := config{initialBackoff: 1, maxBackoff: math.MaxInt64}
		if got := c.backoffCeiling(63); got != 1<<62 {
			t.Fatalf("backoffCeiling(63) = %d, want %d", got, 1<<62)
		}
		for _, attempt := range []int{64, 65, 1000, math.MaxInt} {
			if got := c.backoffCeiling(attempt); got != math.MaxInt64 {
				t.Fatalf("backoffCeiling(%d) = %d, want the maximum", attempt, got)
			}
		}
	})
}

func FuzzBackoffCeiling(f *testing.F) {
	f.Add(int64(100*time.Millisecond), int64(10*time.Second), 1)
	f.Add(int64(1), int64(math.MaxInt64), 63)
	f.Add(int64(1), int64(math.MaxInt64), 64)
	f.Add(int64(3), int64(1000), 9)
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), 2)
	f.Fuzz(func(t *testing.T, initial, maximum int64, attempt int) {
		c := config{maxAttempts: 1, initialBackoff: time.Duration(initial), maxBackoff: time.Duration(maximum), retryIf: func(error) bool { return true }, gate: func() bool { return true }}
		if valid := initial > 0 && maximum >= initial; valid != (c.validate() == nil) {
			t.Fatalf("validate() = %v for the backoff %d to %d", c.validate(), initial, maximum)
		} else if !valid || attempt < 1 {
			return
		}
		want := new(big.Int).Lsh(big.NewInt(initial), uint(min(attempt-1, 64)))
		if limit := big.NewInt(maximum); want.Cmp(limit) > 0 {
			want = limit
		}
		if got := c.backoffCeiling(attempt); got != time.Duration(want.Int64()) {
			t.Fatalf("backoffCeiling(%d) = %d for the backoff %d to %d, want %d", attempt, got, initial, maximum, want)
		}
	})
}

func FuzzBudgetRatio(f *testing.F) {
	for _, seed := range []float64{0, 1, 0.1, -1, 2, math.NaN(), math.Inf(1), math.SmallestNonzeroFloat64} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ratio float64) {
		r, err := New(WithBudget(ratio))
		if valid := ratio > 0 && ratio <= 1; valid != (err == nil) {
			t.Fatalf("New(WithBudget(%v)) error = %v", ratio, err)
		} else if !valid {
			return
		}
		if perCall := r.budget.unitsPerCall; perCall < 1 || perCall > unitsPerToken || float64(perCall) < ratio*unitsPerToken-1 || float64(perCall) > ratio*unitsPerToken+1 {
			t.Fatalf("a first attempt adds %d units for the ratio %v", perCall, ratio)
		}
	})
}
