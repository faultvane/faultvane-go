package bulkhead

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
)

func TestDefaults(t *testing.T) {
	t.Run("BH-14 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c != (config{maxConcurrent: 10}) {
			t.Fatalf("config = %+v", c)
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("BH-14 each option sets its value", func(t *testing.T) {
		c, err := newConfig([]Option{WithMaxConcurrent(3), WithMaxWaiting(7), WithMaxWait(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		if c != (config{maxConcurrent: 3, maxWaiting: 7, maxWait: time.Second}) {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("BH-15 the smallest permitted values are valid", func(t *testing.T) {
		if _, err := New(WithMaxConcurrent(1), WithMaxWaiting(0), WithMaxWait(0)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		opts   []Option
		option string
	}{
		{"max concurrent 0", []Option{WithMaxConcurrent(0)}, "MaxConcurrent"},
		{"max concurrent below 0", []Option{WithMaxConcurrent(-1)}, "MaxConcurrent"},
		{"max waiting below 0", []Option{WithMaxWaiting(-1)}, "MaxWaiting"},
		{"max wait below 0", []Option{WithMaxWait(-time.Nanosecond)}, "MaxWait"},
	}
	for _, c := range cases {
		t.Run("BH-15 TK-21 "+c.name, func(t *testing.T) {
			b, err := New(c.opts...)
			if b != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", b, err)
			}
			if want := "faultvane: invalid configuration: " + c.option + " must be "; !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("error = %q, want the prefix %q", err, want)
			}
		})
	}
}
