package loadshed

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/faultvane/faultvane-go"
)

func TestDefaults(t *testing.T) {
	t.Run("LS-11 LS-12 TK-20 each option has its documented default", func(t *testing.T) {
		c, err := newConfig(nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.maxInFlight != 100 {
			t.Errorf("maxInFlight = %d, want 100", c.maxInFlight)
		}
		want := map[Priority]float64{Low: 0.5, Normal: 0.75, High: 0.9, Critical: 1}
		for p, threshold := range want {
			if c.thresholds[p] != threshold {
				t.Errorf("threshold of priority %d = %v, want %v", p, c.thresholds[p], threshold)
			}
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("LS-12 LS-13 each option sets its value", func(t *testing.T) {
		c, err := newConfig([]Option{
			WithMaxInFlight(1),
			WithThreshold(Low, 0.1),
			WithThreshold(Normal, 0.2),
			WithThreshold(High, 0.3),
			WithThreshold(Critical, 0.4),
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.maxInFlight != 1 || c.thresholds != [priorities]float64{Critical: 0.4, High: 0.3, Normal: 0.2, Low: 0.1} {
			t.Fatalf("config = %+v", c)
		}
	})

	t.Run("LS-13 LS-14 the limits of the valid range are accepted", func(t *testing.T) {
		for name, opts := range map[string][]Option{
			"largest maximum":     {WithMaxInFlight(math.MaxInt)},
			"smallest thresholds": {WithThreshold(Low, math.SmallestNonzeroFloat64), WithThreshold(Normal, math.SmallestNonzeroFloat64)},
			"equal thresholds":    {WithThreshold(Low, 1), WithThreshold(Normal, 1), WithThreshold(High, 1)},
			"low equal to normal": {WithThreshold(Low, 0.75)},
		} {
			if _, err := New(opts...); err != nil {
				t.Errorf("%s: New() error = %v", name, err)
			}
		}
	})
}

func TestValidation(t *testing.T) {
	const (
		rangeRule = "Threshold must be above 0 and at most 1"
		orderRule = "Threshold must not decrease from Low to Critical"
		levelRule = "Threshold must have the priority Critical, High, Normal, or Low"
	)
	cases := map[string]struct {
		options []Option
		want    string
	}{
		"maximum of zero":                  {[]Option{WithMaxInFlight(0)}, "MaxInFlight must be 1 or more"},
		"negative maximum":                 {[]Option{WithMaxInFlight(-1)}, "MaxInFlight must be 1 or more"},
		"threshold of zero":                {[]Option{WithThreshold(Low, 0)}, rangeRule},
		"negative threshold":               {[]Option{WithThreshold(Low, -0.5)}, rangeRule},
		"threshold above one":              {[]Option{WithThreshold(Critical, math.Nextafter(1, 2))}, rangeRule},
		"threshold that is NaN":            {[]Option{WithThreshold(Normal, math.NaN())}, rangeRule},
		"infinite threshold":               {[]Option{WithThreshold(High, math.Inf(1))}, rangeRule},
		"priority below the levels":        {[]Option{WithThreshold(Critical-1, 0.5)}, levelRule},
		"priority above the levels":        {[]Option{WithThreshold(Low+1, 0.5)}, levelRule},
		"low threshold above normal":       {[]Option{WithThreshold(Low, 0.8)}, orderRule},
		"normal threshold above high":      {[]Option{WithThreshold(Normal, 0.95)}, orderRule},
		"high threshold above critical":    {[]Option{WithThreshold(Critical, 0.85)}, orderRule},
		"an invalid option between others": {[]Option{WithMaxInFlight(5), WithThreshold(Low, 2), WithThreshold(High, 0.95)}, rangeRule},
	}
	for name, tc := range cases {
		t.Run("LS-15 TK-21 "+name+" is rejected", func(t *testing.T) {
			shedder, err := New(tc.options...)
			if shedder != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", shedder, err)
			}
			if !strings.HasSuffix(err.Error(), ": "+tc.want) {
				t.Fatalf("error = %q, want the reason %q", err, tc.want)
			}
		})
	}
}
