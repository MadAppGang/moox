package observer_test

// P10's half of the wedged-CLI story: the degradation gap `FIX: wedged
// agents-cli` found, declared out of scope there, and handed here.
//
// The gap, precisely. With `claude agents --json` wedged, the fix made the
// source report honest — `{ok:false, lastError:"claude agents --json: timed out
// after 5s: context deadline exceeded"}` — but `warnings` stayed EMPTY. The
// advisory that had explained the bounded wait (corroboration-wait-expired) is
// cleared the moment the real failure lands, because a result DID arrive; the
// `ok:false` that replaces it was promoted nowhere. A socket consumer watching
// `warnings` therefore saw nothing at all while a whole source was down.
//
// It was deferred rather than patched because a per-rebuild advisory is exactly
// what P7's live run caught misbehaving: `beyond-transcript-retention` and
// `status-suspect` carried recordAgeMs/statusAgeMs, so three of 37 sessions
// emitted session_changed on EVERY rebuild and forever. So the second test here
// is the one that matters — the advisory must be byte-identical across
// rebuilds, which is only true because it carries no clock-derived field.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// findAdvisory returns the named advisory from a list, or ok=false.
func findAdvisory(in []observer.Advisory, code string) (observer.Advisory, bool) {
	for _, a := range in {
		if a.Code == code {
			return a, true
		}
	}
	return observer.Advisory{}, false
}

// TestAFailedSourceIsPromotedIntoWarnings is the fix. The spawn RUNS AND FAILS
// — the measured shapes are `claude` absent from PATH (exit 127) and output
// that will not parse — which is the state that carried no advisory at all.
func TestAFailedSourceIsPromotedIntoWarnings(t *testing.T) {
	h := newWedge(t, true)
	h.set.Agents.Err = errors.New("claude agents --json: exit status 127")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := h.obs.Snapshot(ctx, observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	src, ok := snap.Sources[observer.SourceAgentsCLI]
	if !ok {
		t.Fatal("the agents-cli source block is missing")
	}
	if src.OK {
		t.Fatal("the source reports ok:true after a failed spawn")
	}

	adv, found := findAdvisory(snap.Warnings, observer.AdvSourceNotOK)
	if !found {
		t.Fatalf("warnings do not carry %s; a consumer watching warnings still "+
			"learns nothing about a dead source. warnings=%+v",
			observer.AdvSourceNotOK, snap.Warnings)
	}
	if got := adv.Fields["source"]; got != observer.SourceAgentsCLI {
		t.Errorf("advisory names source %v, want %s", got, observer.SourceAgentsCLI)
	}
	if got, _ := adv.Fields["error"].(string); got == "" {
		t.Error("the advisory carries no error text, which is the one thing it is for")
	}

	// And it is on the source block too, not only in the promoted copy, since
	// that is where a consumer walking `sources` looks.
	if _, onSource := findAdvisory(src.Advisories, observer.AdvSourceNotOK); !onSource {
		t.Error("the advisory was promoted into warnings but is absent from the source block")
	}

	// The authoritative answer is untouched: this is a corroboration source.
	if len(snap.Sessions) == 0 {
		t.Fatal("a failed corroboration source emptied the snapshot")
	}
}

// TestTheFailedSourceAdvisoryIsDiffSafe is the reason this took a phase rather
// than a line. Two rebuilds 90 minutes apart on the fake clock must produce an
// IDENTICAL advisory, or the socket republishes the same outage forever.
func TestTheFailedSourceAdvisoryIsDiffSafe(t *testing.T) {
	h := newWedge(t, true)
	h.set.Agents.Err = errors.New("claude agents --json: exit status 127")

	rebuild := func() observer.Advisory {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap, err := h.obs.Snapshot(ctx, observer.SnapshotOptions{
			IncludeUnknownLiveness: true,
		})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		adv, ok := findAdvisory(snap.Warnings, observer.AdvSourceNotOK)
		if !ok {
			t.Fatalf("warnings lost %s between rebuilds", observer.AdvSourceNotOK)
		}
		return adv
	}

	first := rebuild()
	h.set.Clock.Advance(90 * time.Minute)
	second := rebuild()

	if len(first.Fields) != len(second.Fields) {
		t.Fatalf("the advisory changed shape across rebuilds: %+v then %+v",
			first.Fields, second.Fields)
	}
	for k, v := range first.Fields {
		if second.Fields[k] != v {
			t.Errorf("field %q moved between rebuilds: %v -> %v; a clock-derived "+
				"field here republishes the same outage on every event, which is "+
				"P7's session-churn defect", k, v, second.Fields[k])
		}
	}
}

// TestAMoreSpecificSourceAdvisoryWins keeps the two explanations from doubling
// up. While the spawn is still hanging, corroboration-wait-expired is the
// accurate account and source-not-ok must stay away.
func TestAMoreSpecificSourceAdvisoryWins(t *testing.T) {
	h := newWedge(t, true)
	release := h.wedge()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := h.obs.Snapshot(ctx, observer.DefaultSnapshotOptions())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := findAdvisory(snap.Warnings, observer.AdvCorroborationWaitExpired); !ok {
		t.Fatalf("the wedge no longer raises %s; this test is testing nothing",
			observer.AdvCorroborationWaitExpired)
	}
	if _, ok := findAdvisory(snap.Warnings, observer.AdvSourceNotOK); ok {
		t.Errorf("both %s and %s are present; one outage, one advisory",
			observer.AdvCorroborationWaitExpired, observer.AdvSourceNotOK)
	}
}
