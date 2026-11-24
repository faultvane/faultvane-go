package faultvane_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

type tracer struct {
	name  string
	trace *[]string
}

func (p tracer) Do(ctx context.Context, call func(context.Context) error) error {
	*p.trace = append(*p.trace, p.name+" before")
	err := call(ctx)
	*p.trace = append(*p.trace, p.name+" after")
	return err
}

type rejecter struct{ err error }

func (p rejecter) Do(context.Context, func(context.Context) error) error { return p.err }

type deadline time.Duration

func (d deadline) Do(ctx context.Context, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d))
	defer cancel()
	return call(ctx)
}

type attempts int

func (n attempts) Do(ctx context.Context, call func(context.Context) error) error {
	var err error
	for range int(n) {
		if err = call(ctx); err == nil || errors.Is(err, faultvane.ErrRejected) {
			return err
		}
	}
	return err
}

func TestErrors(t *testing.T) {
	t.Run("TK-11 the two base errors are different", func(t *testing.T) {
		if errors.Is(faultvane.ErrRejected, faultvane.ErrInvalidConfig) || errors.Is(faultvane.ErrInvalidConfig, faultvane.ErrRejected) {
			t.Fatal("ErrRejected and ErrInvalidConfig match each other")
		}
	})
}

func TestSystemClock(t *testing.T) {
	clock := faultvane.SystemClock()
	before := time.Now()
	now := clock.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("Now() = %v, want a time from %v to %v", now, before, after)
	}
}

func TestCombine(t *testing.T) {
	t.Run("TK-3 the first policy is the outermost policy", func(t *testing.T) {
		var trace []string
		pipeline := faultvane.Combine(tracer{"a", &trace}, tracer{"b", &trace}, tracer{"c", &trace})
		err := pipeline.Do(context.Background(), func(context.Context) error {
			trace = append(trace, "call")
			return nil
		})
		want := []string{"a before", "b before", "c before", "call", "c after", "b after", "a after"}
		if err != nil || !slices.Equal(trace, want) {
			t.Fatalf("trace = %v, err = %v, want %v", trace, err, want)
		}
	})

	t.Run("TK-3 a pipeline without policies runs the call", func(t *testing.T) {
		called := false
		err := faultvane.Combine().Do(context.Background(), func(context.Context) error {
			called = true
			return nil
		})
		if err != nil || !called {
			t.Fatalf("called = %v, err = %v", called, err)
		}
	})

	t.Run("TK-3 a later change of the argument slice does not change the pipeline", func(t *testing.T) {
		var trace []string
		policies := []faultvane.Policy{tracer{"a", &trace}}
		pipeline := faultvane.Combine(policies...)
		policies[0] = tracer{"changed", &trace}
		_ = pipeline.Do(context.Background(), func(context.Context) error { return nil })
		if want := []string{"a before", "a after"}; !slices.Equal(trace, want) {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	})

	t.Run("TK-11 a rejection stops the inner policies and the call", func(t *testing.T) {
		var trace []string
		pipeline := faultvane.Combine(tracer{"outer", &trace}, rejecter{faultvane.ErrRejected}, tracer{"inner", &trace})
		err := pipeline.Do(context.Background(), func(context.Context) error {
			trace = append(trace, "call")
			return nil
		})
		want := []string{"outer before", "outer after"}
		if !errors.Is(err, faultvane.ErrRejected) || !slices.Equal(trace, want) {
			t.Fatalf("trace = %v, err = %v, want %v and ErrRejected", trace, err, want)
		}
	})

	t.Run("TK-12 the error of the call returns without a change", func(t *testing.T) {
		var trace []string
		failure := errors.New("dependency failed")
		err := faultvane.Combine(tracer{"a", &trace}, tracer{"b", &trace}).Do(context.Background(), func(context.Context) error {
			return failure
		})
		if err != failure {
			t.Fatalf("err = %v, want the same error value", err)
		}
	})

	t.Run("TK-13 the context of an outer policy reaches the call", func(t *testing.T) {
		var trace []string
		hasDeadline := false
		err := faultvane.Combine(deadline(time.Hour), tracer{"inner", &trace}).Do(context.Background(), func(ctx context.Context) error {
			_, hasDeadline = ctx.Deadline()
			return nil
		})
		if err != nil || !hasDeadline {
			t.Fatalf("hasDeadline = %v, err = %v", hasDeadline, err)
		}
	})

	t.Run("TK-2 a retry and a timeout fit the policy interface", func(t *testing.T) {
		calls := 0
		deadlines := 0
		failure := errors.New("temporary")
		err := faultvane.Combine(attempts(3), deadline(time.Hour)).Do(context.Background(), func(ctx context.Context) error {
			calls++
			if _, ok := ctx.Deadline(); ok {
				deadlines++
			}
			if calls < 3 {
				return failure
			}
			return nil
		})
		if err != nil || calls != 3 || deadlines != 3 {
			t.Fatalf("calls = %d, deadlines = %d, err = %v, want 3, 3, nil", calls, deadlines, err)
		}
	})

	t.Run("TK-2 a retry does not repeat a rejection", func(t *testing.T) {
		rejections := 0
		counting := policyFunc(func(context.Context, func(context.Context) error) error {
			rejections++
			return faultvane.ErrRejected
		})
		err := faultvane.Combine(attempts(3), counting).Do(context.Background(), func(context.Context) error { return nil })
		if !errors.Is(err, faultvane.ErrRejected) || rejections != 1 {
			t.Fatalf("rejections = %d, err = %v, want 1 and ErrRejected", rejections, err)
		}
	})

	t.Run("TK-2 a retry returns the last error after all attempts", func(t *testing.T) {
		calls := 0
		err := faultvane.Combine(attempts(3)).Do(context.Background(), func(context.Context) error {
			calls++
			return errors.New("failure " + strconv.Itoa(calls))
		})
		if calls != 3 || err == nil || err.Error() != "failure 3" {
			t.Fatalf("calls = %d, err = %v, want 3 and failure 3", calls, err)
		}
	})
}

type policyFunc func(context.Context, func(context.Context) error) error

func (f policyFunc) Do(ctx context.Context, call func(context.Context) error) error {
	return f(ctx, call)
}

func TestExecute(t *testing.T) {
	t.Run("TK-2 the value and a nil error return", func(t *testing.T) {
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(), func(context.Context) (int, error) {
			return 42, nil
		})
		if value != 42 || err != nil {
			t.Fatalf("value = %d, err = %v, want 42, nil", value, err)
		}
	})

	t.Run("TK-12 the value and the error of the call return together", func(t *testing.T) {
		failure := errors.New("partial")
		value, err := faultvane.Execute(context.Background(), faultvane.Combine(), func(context.Context) (string, error) {
			return "partial result", failure
		})
		if value != "partial result" || err != failure {
			t.Fatalf("value = %q, err = %v", value, err)
		}
	})

	t.Run("TK-11 a rejection returns the zero value", func(t *testing.T) {
		value, err := faultvane.Execute(context.Background(), rejecter{faultvane.ErrRejected}, func(context.Context) (int, error) {
			return 42, nil
		})
		if value != 0 || !errors.Is(err, faultvane.ErrRejected) {
			t.Fatalf("value = %d, err = %v, want 0 and ErrRejected", value, err)
		}
	})
}
