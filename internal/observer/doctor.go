package observer

import (
	"sort"
	"time"
)

// This file is §9.5's vocabulary: what a diagnosis IS, in the core's words.
// The reading is an adapter's job (adapters/doctorfs), the rendering is a
// surface's, and both speak in these types.
//
// # Why a doctor exists at all
//
// Every other design decision in this observer pulls towards silence: an
// unknown status is passed through, a missing directory is a fact rather than
// an error, a torn read keeps the previous value, and a source that cannot be
// read degrades to `live: null` instead of failing the answer. That is right
// for a status tool a user reads forty times a day, and it has one failure mode
// — in session-kit's words, "a whole history quietly stops working while every
// command still exits 0".
//
// The doctor is the counterweight. It is the one surface whose entire purpose
// is to SHOUT: it reads one live example of each format, compares it against
// what this build was written against, and says which assumptions still hold.
// Nothing else in tt ever reports "the shape changed" — it just quietly starts
// answering less.
//
// # The three verdicts, and the distinction that makes them worth having
//
//	ok    the assumption holds, with the evidence that shows it
//	warn  the assumption no longer holds and a FEATURE DEGRADES: something is
//	      less accurate, later or emptier than designed, and tt still works
//	fail  the assumption no longer holds and a FEATURE IS BROKEN: something
//	      this build states as a fact is now false
//
// A check that cannot distinguish "gone, feature broken" from "gone, feature
// degrades" is not worth running, because the user's next action differs
// completely between the two.

// DoctorStatus is one check's verdict.
type DoctorStatus string

const (
	// DoctorOK: the assumption holds.
	DoctorOK DoctorStatus = "ok"
	// DoctorWarn: it no longer holds and something degrades.
	DoctorWarn DoctorStatus = "warn"
	// DoctorFail: it no longer holds and something is broken.
	DoctorFail DoctorStatus = "fail"
	// DoctorSkip: the check could not be run at all — the tree it reads is
	// absent, or a source it needs is not wired. Deliberately NOT "ok": a check
	// that did not run has established nothing, and reporting it as a pass is
	// the exact deceit the doctor exists to prevent.
	DoctorSkip DoctorStatus = "skip"
)

// DoctorCheck is one assumption, its verdict and the evidence for it.
//
// Evidence is not decoration. A doctor that prints "ok" and nothing else is
// unfalsifiable: the reader cannot tell a check that looked from a check that
// returned early. Every check here carries the numbers it decided on, so a
// `warn` can be acted on and an `ok` can be disbelieved.
type DoctorCheck struct {
	// ID is stable and machine-readable — a script may grep for it.
	ID string
	// Title is the assumption, stated as the thing that must be true.
	Title string
	// Cite names the architecture section that depends on this check, so a
	// failure leads somewhere rather than merely being alarming.
	Cite   string
	Status DoctorStatus
	// Detail is one sentence: what was found, and what it costs.
	Detail string
	// Evidence is the measurement, one line each.
	Evidence []string
}

// DoctorReport is one run.
type DoctorReport struct {
	TakenAt    time.Time
	DurationMs int64
	Checks     []DoctorCheck
	// Paths names what was read, so a report filed in validation/ says what
	// machine state it describes.
	Paths Paths
}

// Worst is the report's overall verdict: the most severe check in it.
//
// Skip is deliberately BELOW warn in this ordering and above ok, because a
// machine where Claude has never run should not report a healthy diagnosis and
// should not report a broken one either.
func (r DoctorReport) Worst() DoctorStatus {
	worst := DoctorOK
	rank := map[DoctorStatus]int{DoctorOK: 0, DoctorSkip: 1, DoctorWarn: 2, DoctorFail: 3}
	for _, c := range r.Checks {
		if rank[c.Status] > rank[worst] {
			worst = c.Status
		}
	}
	return worst
}

// Tally counts the checks by verdict. Every verdict appears in the map, with a
// zero where it did not occur, for the same reason the omitempty ban exists:
// an absent key and a zero are different claims and a reader should not have to
// guess which one they are looking at.
func (r DoctorReport) Tally() map[DoctorStatus]int {
	out := map[DoctorStatus]int{DoctorOK: 0, DoctorWarn: 0, DoctorFail: 0, DoctorSkip: 0}
	for _, c := range r.Checks {
		out[c.Status]++
	}
	return out
}

// DoctorKeyDiff compares an observed key set against the set a build was
// written against. It is the shape most §9.5 checks reduce to, so it is
// computed once, here, rather than four times in the adapter.
//
// The asymmetry is the point and it is the reason this returns two slices
// rather than a bool: a key we expected and no longer see is a FAILURE (code
// reads it), while a key that appeared and we do not read is at most a warning
// (we are behind, not broken).
type DoctorKeyDiff struct {
	Missing []string
	Added   []string
}

// DiffKeys returns the keys of want that observed lacks, and the keys of
// observed that want does not name. Both are sorted, because a diff whose order
// depends on map iteration cannot be compared between two runs.
func DiffKeys(want []string, observed map[string]int) DoctorKeyDiff {
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}

	var d DoctorKeyDiff
	for _, k := range want {
		if _, ok := observed[k]; !ok {
			d.Missing = append(d.Missing, k)
		}
	}
	for k := range observed {
		if !wantSet[k] {
			d.Added = append(d.Added, k)
		}
	}
	sort.Strings(d.Missing)
	sort.Strings(d.Added)
	return d
}
