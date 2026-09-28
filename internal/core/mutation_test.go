package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/probavi/probavi/internal/evidence"
)

// mutation_test.go holds the cases a mutation run found missing: each one
// is a change to the code that every other test accepted. They are here
// together because that is what they have in common — a gap in what the
// suite asserts rather than a gap in what it covers.

// TestEveryShapeRejectionEarnsTheDegradedRecord: the backstop behind
// evidence-schema.md §7 fires when the store refused a record for what it
// contains rather than because writing failed, and three sentinels say
// that. Only one of them had a case. The other two are the ones a real
// drill reaches — a record over the size limit, and a number the canonical
// form cannot represent — and a backstop that recognised one sentinel in
// three would leave a drill that ran with no evidence at all, which is the
// outcome it exists to prevent.
func TestEveryShapeRejectionEarnsTheDegradedRecord(t *testing.T) {
	for name, sentinel := range map[string]error{
		"a record over the size limit":     evidence.ErrRecordTooLarge,
		"a number the record cannot carry": evidence.ErrNotInteger,
	} {
		t.Run(name, func(t *testing.T) {
			fa := &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true}
			d, _ := newDrill(t, fa, &fakeProvider{sbx: &fakeSandbox{execValue: "1"}})
			counting := &countingStore{err: fmt.Errorf("%w: nope", sentinel)}
			d.Store = counting

			if _, err := d.Run(context.Background()); err == nil {
				t.Fatal("Run reported success although every append was refused")
			}
			if counting.calls != 2 {
				t.Errorf("Append called %d times, want 2: one composed, one degraded", counting.calls)
			}
		})
	}
}

// TestACreateFailureDoesNotOverwriteAVerdictThatAlreadyHasOne: a sandbox
// that will not start is recorded as a sandbox error, and that rewrite is
// deliberately narrow — it only replaces the code that means "we do not
// know". A drill that ran out of wall clock while creating its sandbox has
// a verdict already, and it is not the sandbox's. Widening the guard puts a
// signed record on the log blaming the provider for the clock.
func TestACreateFailureDoesNotOverwriteAVerdictThatAlreadyHasOne(t *testing.T) {
	fa := &fakeAdapter{probe: testProbe()}
	fp := &fakeProvider{createErr: errors.New("no capacity")}
	d, _ := newDrill(t, fa, fp)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rec, err := d.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Error == nil || rec.Error.Code != "timeout" {
		t.Errorf("record error = %+v, want timeout — the clock ran out, the provider did not misbehave", rec.Error)
	}
}

// TestTheHostIDIsTheHostsAndTheFallbackIsOnlyAFallback: env.host_id is how
// an auditor tells two drill hosts apart in one log, and it is a hash, so a
// wrong one looks exactly like a right one. The name is only replaced when
// the host cannot be asked, and nothing asserted which way round that was —
// a reversed guard gives every host in the fleet the same id, and the
// records stay valid, signed and indistinguishable.
func TestTheHostIDIsTheHostsAndTheFallbackIsOnlyAFallback(t *testing.T) {
	id := func(name string, err error) string {
		d := &Drill{Hostname: func() (string, error) { return name, err }}
		return d.hostID()
	}
	if id("alpha", nil) == id("beta", nil) {
		t.Error("two hosts share one id; a log from both cannot be told apart")
	}
	if got, want := id("", errors.New("no hostname")), id("unknown-host", nil); got != want {
		t.Errorf("host id with no hostname = %q, want the unknown-host id %q", got, want)
	}
	if id("alpha", nil) == id("unknown-host", nil) {
		t.Error("a host that answered got the fallback id")
	}
}

// runWithLog runs a drill and returns what it told its logger.
func runWithLog(t *testing.T, d *Drill) string {
	t.Helper()
	buf := &bytes.Buffer{}
	d.Logger = slog.New(slog.NewTextHandler(buf, nil))
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return buf.String()
}

// TestTheCleanupPathsSayWhatTheyCouldNotClean: nothing downstream changes
// when a sweep, a teardown or a destroy fails. The drill's verdict is
// already decided and the record is already composed, so the log line is
// the whole of what an operator gets.
//
// Cleanup is the one place where that is enough on purpose. A sandbox that
// will not go away holds production data until the next run's sweep reaches
// it, and a sweep that itself failed is how that stops happening — so a
// silent failure here is how a leak becomes permanent without anyone
// deciding that it should.
func TestTheCleanupPathsSayWhatTheyCouldNotClean(t *testing.T) {
	newFakes := func() (*fakeAdapter, *fakeProvider) {
		return &fakeAdapter{probe: testProbe(), provRes: testProvision(), healthy: true},
			&fakeProvider{sbx: &fakeSandbox{execValue: "1"}}
	}

	t.Run("a sweep that failed", func(t *testing.T) {
		fa, fp := newFakes()
		fp.sweepErr = errors.New("docker unreachable")
		d, _ := newDrill(t, fa, fp)
		if out := runWithLog(t, d); !strings.Contains(out, "orphan sweep failed") {
			t.Errorf("logged %q, want the failed sweep named", out)
		}
	})
	t.Run("a sweep that removed one orphan", func(t *testing.T) {
		fa, fp := newFakes()
		fp.sweptIDs = []string{"probavi-sbx-stale"}
		d, _ := newDrill(t, fa, fp)
		out := runWithLog(t, d)
		for _, want := range []string{"swept orphan sandboxes", "probavi-sbx-stale"} {
			if !strings.Contains(out, want) {
				t.Errorf("logged %q, want it to carry %q", out, want)
			}
		}
	})
	t.Run("a sweep that found nothing", func(t *testing.T) {
		fa, fp := newFakes()
		d, _ := newDrill(t, fa, fp)
		if out := runWithLog(t, d); strings.Contains(out, "swept orphan sandboxes") {
			t.Errorf("logged %q, want silence when there was nothing to sweep", out)
		}
	})
	t.Run("a teardown that failed", func(t *testing.T) {
		fa, fp := newFakes()
		fa.teardownErr = errors.New("adapter refused")
		d, _ := newDrill(t, fa, fp)
		if out := runWithLog(t, d); !strings.Contains(out, "adapter teardown failed") {
			t.Errorf("logged %q, want the failed teardown named", out)
		}
	})
	t.Run("a sandbox that would not be destroyed", func(t *testing.T) {
		fa, fp := newFakes()
		fp.sbx.destroyErr = errors.New("container busy")
		d, _ := newDrill(t, fa, fp)
		if out := runWithLog(t, d); !strings.Contains(out, "sandbox destroy failed") {
			t.Errorf("logged %q, want the failed destroy named", out)
		}
	})
}
