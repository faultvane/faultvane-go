package circuitbreaker

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func TestDefaults(t *testing.T) {
	t.Run("CFG-2 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig("payment-service", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.rules != (rules{minimumCalls: 20, failureRate: 0.5}) {
			t.Errorf("rules = %+v", c.rules)
		}
		if c.windowSize != 10*time.Second || c.windowBuckets != 10 {
			t.Errorf("window = %v, %d", c.windowSize, c.windowBuckets)
		}
		if c.openDuration != 30*time.Second || c.probeTimeout != 30*time.Second {
			t.Errorf("openDuration = %v, probeTimeout = %v", c.openDuration, c.probeTimeout)
		}
		if c.halfOpenMaxProbes != 1 || c.halfOpenSuccessThreshold != 1 {
			t.Errorf("halfOpenMaxProbes = %d, halfOpenSuccessThreshold = %d", c.halfOpenMaxProbes, c.halfOpenSuccessThreshold)
		}
		if c.isSlow(time.Hour) {
			t.Error("slow call detection is on")
		}
		if c.classifier(errors.New("x")) != Failure || c.clock != faultvane.SystemClock() {
			t.Error("classifier or clock is not the default")
		}
	})

	t.Run("CFG-2 the success threshold follows the probe limit", func(t *testing.T) {
		c, err := newConfig("a", []Option{WithHalfOpenMaxProbes(3)})
		if err != nil || c.halfOpenSuccessThreshold != 3 {
			t.Fatalf("halfOpenSuccessThreshold = %d, err = %v, want 3, nil", c.halfOpenSuccessThreshold, err)
		}
	})

	t.Run("CFG-2 an explicit success threshold stays", func(t *testing.T) {
		c, err := newConfig("a", []Option{WithHalfOpenMaxProbes(3), WithHalfOpenSuccessThreshold(5)})
		if err != nil || c.halfOpenSuccessThreshold != 5 {
			t.Fatalf("halfOpenSuccessThreshold = %d, err = %v, want 5, nil", c.halfOpenSuccessThreshold, err)
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("FD-5 CFG-1 each option sets its value", func(t *testing.T) {
		classifier := func(error) Result { return Ignored }
		clock := fixedClock{}
		c, err := newConfig("a", []Option{
			WithFailureRateThreshold(1),
			WithMinimumCalls(1),
			WithFailureThreshold(1),
			WithSlowCallDuration(time.Nanosecond),
			WithSlowCallRateThreshold(1),
			WithWindowSize(3 * time.Nanosecond),
			WithWindowBuckets(3),
			WithOpenDuration(time.Nanosecond),
			WithHalfOpenMaxProbes(2),
			WithHalfOpenSuccessThreshold(1),
			WithProbeTimeout(time.Nanosecond),
			WithClassifier(classifier),
			WithClock(clock),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.rules != (rules{failureThreshold: 1, minimumCalls: 1, failureRate: 1, slowCallRate: 1}) {
			t.Errorf("rules = %+v", c.rules)
		}
		if c.windowSize != 3 || c.windowBuckets != 3 || c.openDuration != 1 || c.probeTimeout != 1 {
			t.Errorf("durations = %+v", c)
		}
		if c.halfOpenMaxProbes != 2 || c.halfOpenSuccessThreshold != 1 {
			t.Errorf("half-open = %d, %d", c.halfOpenMaxProbes, c.halfOpenSuccessThreshold)
		}
		if !c.isSlow(time.Nanosecond) || c.isSlow(0) {
			t.Error("slow call duration is not 1ns")
		}
		if c.classifier(nil) != Ignored || c.clock != clock {
			t.Error("classifier or clock is not set")
		}
	})
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return testStart }

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		opts   []Option
		option string
	}{
		{"empty name", "", nil, "name"},
		{"name with 65 characters", strings.Repeat("a", 65), nil, "name"},
		{"name with a space", "a b", nil, "name"},
		{"name with a colon", "a:b", nil, "name"},
		{"name with a brace", "{a}", nil, "name"},
		{"name with a slash", "a/b", nil, "name"},
		{"name with a character outside ASCII", "café", nil, "name"},
		{"failure rate 0", "a", []Option{WithFailureRateThreshold(0)}, "FailureRateThreshold"},
		{"failure rate below 0", "a", []Option{WithFailureRateThreshold(-0.1)}, "FailureRateThreshold"},
		{"failure rate above 1", "a", []Option{WithFailureRateThreshold(1.01)}, "FailureRateThreshold"},
		{"failure rate not a number", "a", []Option{WithFailureRateThreshold(math.NaN())}, "FailureRateThreshold"},
		{"minimum calls 0", "a", []Option{WithMinimumCalls(0)}, "MinimumCalls"},
		{"failure threshold 0", "a", []Option{WithFailureThreshold(0)}, "FailureThreshold"},
		{"failure threshold below 0", "a", []Option{WithFailureThreshold(-1)}, "FailureThreshold"},
		{"slow call duration without rate", "a", []Option{WithSlowCallDuration(time.Second)}, "SlowCallDuration and SlowCallRateThreshold"},
		{"slow call rate without duration", "a", []Option{WithSlowCallRateThreshold(0.5)}, "SlowCallDuration and SlowCallRateThreshold"},
		{"slow call duration 0", "a", []Option{WithSlowCallDuration(0), WithSlowCallRateThreshold(0.5)}, "SlowCallDuration"},
		{"slow call rate 0", "a", []Option{WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(0)}, "SlowCallRateThreshold"},
		{"slow call rate above 1", "a", []Option{WithSlowCallDuration(time.Second), WithSlowCallRateThreshold(2)}, "SlowCallRateThreshold"},
		{"window buckets 0", "a", []Option{WithWindowBuckets(0)}, "WindowBuckets"},
		{"window size 0", "a", []Option{WithWindowSize(0)}, "WindowSize"},
		{"window size below 0", "a", []Option{WithWindowSize(-time.Second)}, "WindowSize"},
		{"window size shorter than the buckets", "a", []Option{WithWindowSize(9 * time.Nanosecond)}, "WindowSize"},
		{"open duration 0", "a", []Option{WithOpenDuration(0)}, "OpenDuration"},
		{"half-open probes 0", "a", []Option{WithHalfOpenMaxProbes(0), WithHalfOpenSuccessThreshold(1)}, "HalfOpenMaxProbes"},
		{"half-open success threshold 0", "a", []Option{WithHalfOpenSuccessThreshold(0)}, "HalfOpenSuccessThreshold"},
		{"probe timeout 0", "a", []Option{WithProbeTimeout(0)}, "ProbeTimeout"},
		{"nil classifier", "a", []Option{WithClassifier(nil)}, "Classifier"},
		{"nil clock", "a", []Option{WithClock(nil)}, "Clock"},
	}
	for _, c := range cases {
		t.Run("CFG-3 TK-21 "+c.name, func(t *testing.T) {
			b, err := New(c.id, c.opts...)
			if b != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", b, err)
			}
			if want := "faultvane: invalid configuration: " + c.option + " "; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("err = %q, want the prefix %q", err, want)
			}
		})
	}

	valid := []struct {
		name string
		id   string
		opts []Option
	}{
		{"name with 64 characters", strings.Repeat("a", 64), nil},
		{"name with one character", "a", nil},
		{"name with each permitted type of character", "azAZ09._-", nil},
		{"failure rate 1", "a", []Option{WithFailureRateThreshold(1)}},
		{"smallest failure rate", "a", []Option{WithFailureRateThreshold(math.SmallestNonzeroFloat64)}},
		{"minimum calls 1", "a", []Option{WithMinimumCalls(1)}},
		{"failure threshold 1", "a", []Option{WithFailureThreshold(1)}},
		{"slow call values at their limits", "a", []Option{WithSlowCallDuration(time.Nanosecond), WithSlowCallRateThreshold(1)}},
		{"window size equal to the buckets", "a", []Option{WithWindowSize(10 * time.Nanosecond)}},
		{"one bucket", "a", []Option{WithWindowBuckets(1)}},
		{"shortest durations", "a", []Option{WithOpenDuration(time.Nanosecond), WithProbeTimeout(time.Nanosecond)}},
	}
	for _, c := range valid {
		t.Run("CFG-3 valid: "+c.name, func(t *testing.T) {
			if _, err := New(c.id, c.opts...); err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
		})
	}
}

func FuzzValidName(f *testing.F) {
	for _, seed := range []string{"", "a", "payment-service", "a b", "{a}", strings.Repeat("z", 64), strings.Repeat("z", 65), "A.b_c-9", "café"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		const permitted = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
		want := len(name) >= 1 && len(name) <= 64 && strings.Trim(name, permitted) == ""
		if got := validName(name); got != want {
			t.Fatalf("validName(%q) = %v, want %v", name, got, want)
		}
	})
}
