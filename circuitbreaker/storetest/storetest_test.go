package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
)

func memory() *circuitbreaker.MemoryStore {
	return circuitbreaker.NewMemoryStore(faultvane.SystemClock())
}

func TestMemoryStoreConformance(t *testing.T) {
	Run(t, func(*testing.T) circuitbreaker.Store { return memory() })
}

type acceptsEachGeneration struct{ *circuitbreaker.MemoryStore }

func (s acceptsEachGeneration) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	current, _ := s.Load(ctx, name)
	report.Generation = current.Generation
	return s.MemoryStore.Report(ctx, name, report)
}

type acceptsEachProbeResult struct{ *circuitbreaker.MemoryStore }

func (s acceptsEachProbeResult) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	current, _ := s.Load(ctx, name)
	result.Generation = current.Generation
	return s.MemoryStore.CompleteProbe(ctx, name, result)
}

type grantsEachProbe struct{ *circuitbreaker.MemoryStore }

func (s grantsEachProbe) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	grant, err := s.MemoryStore.AcquireProbe(ctx, name, request)
	grant.Granted = true
	return grant, err
}

type ignoresTheLimit struct{ *circuitbreaker.MemoryStore }

func (s ignoresTheLimit) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	request.Limit = 1 << 30
	return s.MemoryStore.AcquireProbe(ctx, name, request)
}

type keepsLeasesForever struct{ *circuitbreaker.MemoryStore }

func (s keepsLeasesForever) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	request.LeaseDuration = 24 * time.Hour
	return s.MemoryStore.AcquireProbe(ctx, name, request)
}

type neverOpens struct{ *circuitbreaker.MemoryStore }

func (s neverOpens) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	report.Thresholds.FailureThreshold = 1 << 40
	return s.MemoryStore.Report(ctx, name, report)
}

type keepsOldBuckets struct{ *circuitbreaker.MemoryStore }

func (s keepsOldBuckets) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	report.WindowSize = 24 * time.Hour
	return s.MemoryStore.Report(ctx, name, report)
}

type restoresEachSnapshot struct{ *circuitbreaker.MemoryStore }

func (s restoresEachSnapshot) Restore(ctx context.Context, name string, snapshot circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	s.Delete(name)
	return s.MemoryStore.Restore(ctx, name, snapshot)
}

type ignoresRestore struct{ *circuitbreaker.MemoryStore }

func (s ignoresRestore) Restore(ctx context.Context, name string, _ circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	return s.Load(ctx, name)
}

type ignoresTheContext struct{ *circuitbreaker.MemoryStore }

func (s ignoresTheContext) Load(_ context.Context, name string) (circuitbreaker.Snapshot, error) {
	return s.MemoryStore.Load(context.Background(), name)
}

type sharesAllNames struct{ *circuitbreaker.MemoryStore }

func (s sharesAllNames) Load(ctx context.Context, _ string) (circuitbreaker.Snapshot, error) {
	return s.MemoryStore.Load(ctx, "shared")
}

func (s sharesAllNames) Report(ctx context.Context, _ string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	return s.MemoryStore.Report(ctx, "shared", report)
}

type withoutStoreTime struct{ *circuitbreaker.MemoryStore }

func (s withoutStoreTime) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	snapshot, err := s.MemoryStore.Load(ctx, name)
	snapshot.StoreTime = time.Time{}
	return snapshot, err
}

type inventsAFingerprint struct{ *circuitbreaker.MemoryStore }

func (s inventsAFingerprint) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	snapshot, err := s.MemoryStore.Load(ctx, name)
	if snapshot.Fingerprint == "" {
		snapshot.Fingerprint = "invented"
	}
	return snapshot, err
}

type returnsTheFingerprintOfTheCaller struct{ *circuitbreaker.MemoryStore }

func (s returnsTheFingerprintOfTheCaller) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	snapshot, err := s.MemoryStore.Report(ctx, name, report)
	snapshot.Fingerprint = report.Fingerprint
	return snapshot, err
}

type savesTheFingerprintOfAnIgnoredReport struct{ *circuitbreaker.MemoryStore }

func (s savesTheFingerprintOfAnIgnoredReport) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	current, _ := s.Load(ctx, name)
	accepted := report
	accepted.Generation, accepted.Calls, accepted.Failures, accepted.SlowCalls = current.Generation, 0, 0, 0
	_, _ = s.MemoryStore.Report(ctx, name, accepted)
	return s.MemoryStore.Report(ctx, name, report)
}

type ignoresTheFingerprintOfAProbe struct{ *circuitbreaker.MemoryStore }

func (s ignoresTheFingerprintOfAProbe) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	current, _ := s.Load(ctx, name)
	request.Fingerprint = current.Fingerprint
	return s.MemoryStore.AcquireProbe(ctx, name, request)
}

type unavailable struct{}

var errDown = errors.New("store down")

func (unavailable) Load(context.Context, string) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (unavailable) Report(context.Context, string, circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (unavailable) AcquireProbe(context.Context, string, circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	return circuitbreaker.ProbeGrant{}, errDown
}

func (unavailable) CompleteProbe(context.Context, string, circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

func (unavailable) Restore(context.Context, string, circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	return circuitbreaker.Snapshot{}, errDown
}

type ignoresTheIdentity struct{ *circuitbreaker.MemoryStore }

func (s ignoresTheIdentity) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	report.Sequence = 0
	return s.MemoryStore.Report(ctx, name, report)
}

type sharesOneSequence struct{ *circuitbreaker.MemoryStore }

func (s sharesOneSequence) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	report.Instance = 0
	return s.MemoryStore.Report(ctx, name, report)
}

type numbersEachReport struct{ *circuitbreaker.MemoryStore }

func (s numbersEachReport) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	report.Sequence = max(report.Sequence, 1)
	return s.MemoryStore.Report(ctx, name, report)
}

type savesTheFingerprintOfARepeatedReport struct{ *circuitbreaker.MemoryStore }

func (s savesTheFingerprintOfARepeatedReport) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	empty := report
	empty.Sequence, empty.Calls, empty.Failures, empty.SlowCalls = 0, 0, 0, 0
	_, _ = s.MemoryStore.Report(ctx, name, empty)
	return s.MemoryStore.Report(ctx, name, report)
}

func failedChecks(newStore func() circuitbreaker.Store, patience time.Duration) []string {
	var failed []string
	for _, c := range checks {
		if len(c.problems(newStore(), patience)) > 0 {
			failed = append(failed, c.name)
		}
	}
	return failed
}

func TestTheSuiteAcceptsTheReference(t *testing.T) {
	if failed := failedChecks(func() circuitbreaker.Store { return memory() }, 5*time.Second); len(failed) != 0 {
		t.Fatalf("QA-8 the suite rejects the in-memory store: %v", failed)
	}
}

func TestEachFingerprintCheckRejectsABrokenStore(t *testing.T) {
	broken := map[string]func() circuitbreaker.Store{
		"C13 a breaker without state has no fingerprint":                                                  func() circuitbreaker.Store { return inventsAFingerprint{memory()} },
		"C13 a report saves the fingerprint of the caller and returns the fingerprint before it":          func() circuitbreaker.Store { return returnsTheFingerprintOfTheCaller{memory()} },
		"C13 an operation that the store ignores does not change the fingerprint":                         func() circuitbreaker.Store { return savesTheFingerprintOfAnIgnoredReport{memory()} },
		"C13 a probe operation saves the fingerprint of the caller and returns the fingerprint before it": func() circuitbreaker.Store { return ignoresTheFingerprintOfAProbe{memory()} },
		"C13 restore does not change the fingerprint":                                                     func() circuitbreaker.Store { return restoresEachSnapshot{memory()} },
		"C3 the store behaves as the reference for random sequences of operations":                        func() circuitbreaker.Store { return ignoresTheFingerprintOfAProbe{memory()} },
	}
	for check, newStore := range broken {
		t.Run("QA-8 DS-9 a broken store fails the check: "+check, func(t *testing.T) {
			t.Parallel()
			if failed := failedChecks(newStore, 150*time.Millisecond); !slices.Contains(failed, check) {
				t.Fatalf("failed checks = %q, want %q in them", failed, check)
			}
		})
	}
}

func TestEachIdentityCheckRejectsABrokenStore(t *testing.T) {
	broken := map[string]func() circuitbreaker.Store{
		"C14 a report that the store applied before adds nothing":                        func() circuitbreaker.Store { return ignoresTheIdentity{memory()} },
		"C14 each instance has its own sequence":                                         func() circuitbreaker.Store { return sharesOneSequence{memory()} },
		"C14 a report without a sequence is added each time":                             func() circuitbreaker.Store { return numbersEachReport{memory()} },
		"C14 C13 a report that the store applied before does not change the fingerprint": func() circuitbreaker.Store { return savesTheFingerprintOfARepeatedReport{memory()} },
		"C14 the sequence of an instance continues after a transition":                   func() circuitbreaker.Store { return ignoresTheIdentity{memory()} },
		"C3 the store behaves as the reference for random sequences of operations":       func() circuitbreaker.Store { return ignoresTheIdentity{memory()} },
	}
	for check, newStore := range broken {
		t.Run("QA-8 DS-9 a broken store fails the check: "+check, func(t *testing.T) {
			t.Parallel()
			if failed := failedChecks(newStore, 150*time.Millisecond); !slices.Contains(failed, check) {
				t.Fatalf("failed checks = %q, want %q in them", failed, check)
			}
		})
	}
}

func TestTheSuiteRejectsBrokenStores(t *testing.T) {
	broken := map[string]func() circuitbreaker.Store{
		"accepts a report with each generation":       func() circuitbreaker.Store { return acceptsEachGeneration{memory()} },
		"accepts a probe result with each generation": func() circuitbreaker.Store { return acceptsEachProbeResult{memory()} },
		"grants each probe":                           func() circuitbreaker.Store { return grantsEachProbe{memory()} },
		"ignores the probe limit":                     func() circuitbreaker.Store { return ignoresTheLimit{memory()} },
		"keeps leases for too long":                   func() circuitbreaker.Store { return keepsLeasesForever{memory()} },
		"never opens":                                 func() circuitbreaker.Store { return neverOpens{memory()} },
		"keeps old buckets":                           func() circuitbreaker.Store { return keepsOldBuckets{memory()} },
		"restores each snapshot":                      func() circuitbreaker.Store { return restoresEachSnapshot{memory()} },
		"ignores restore":                             func() circuitbreaker.Store { return ignoresRestore{memory()} },
		"ignores the context":                         func() circuitbreaker.Store { return ignoresTheContext{memory()} },
		"shares all names":                            func() circuitbreaker.Store { return sharesAllNames{memory()} },
		"returns no store time":                       func() circuitbreaker.Store { return withoutStoreTime{memory()} },
		"is not available":                            func() circuitbreaker.Store { return unavailable{} },
	}
	for name, newStore := range broken {
		t.Run("QA-8 DS-6 the suite rejects a store that "+name, func(t *testing.T) {
			t.Parallel()
			if failed := failedChecks(newStore, 150*time.Millisecond); len(failed) == 0 {
				t.Fatal("no check failed")
			}
		})
	}
}
