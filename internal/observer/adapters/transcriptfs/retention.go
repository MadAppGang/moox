package transcriptfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// SessionTranscriptGlob is NORMATIVE and NON-RECURSIVE, and that is the whole
// point of it having a name.
//
// §3.5 rule 4 answers live: null for a job record that is older than the
// vendor's transcript retention window and whose transcript is gone — the
// claim cannot be corroborated by a file that cannot exist. The rule's
// threshold is only defensible because the population it was measured over is
// the population `linkScanPath` actually names: depth-2 SESSION transcripts,
// 7/7 on the real records.
//
// Measured on this machine, twice, a day apart:
//
//	projects/*/*.jsonl            3 181 files   oldest  30.11 d   past 30 d:   2
//	projects/**/*.jsonl (deep)    4 981 files   oldest 105.68 d   past 30 d: 335
//
// The cleanup policy does not reap `subagents/` at all. So a `filepath.WalkDir`
// or a `**` re-derivation returns 105 days and SILENTLY DISABLES rule 4 for
// three months — r2 H4 regressing, from a change that looks like a tidy-up. The
// glob lives in a named constant so that a test can assert its shape and so
// that changing it is a deliberate act with a comment to argue with.
const SessionTranscriptGlob = "*/*.jsonl"

// Floor is the measured retention floor over the depth-2 session-transcript
// population, and the counts that make it checkable rather than merely
// asserted.
type Floor struct {
	// Files is how many transcripts the non-recursive glob matched.
	Files int
	// OldestAgeMs is the age of the oldest of them. Zero when Files is 0.
	OldestAgeMs int64
	// OldestPath names it, so a doctor run can show its work.
	OldestPath string
	// PastRetention is how many are older than the retention window passed in.
	// A healthy machine reports 0 or a handful aging out right now; a large
	// number means the vendor's cleanup is not running and rule 4's premise
	// has changed.
	PastRetention int
	// StatErrors counts matches that could not be statted. They are skipped
	// rather than failing the derivation.
	StatErrors int
}

// DeriveFloor measures the retention floor over SessionTranscriptGlob.
//
// It is a MEASUREMENT, not the threshold. The threshold in force comes from
// RetentionMs (the vendor's own setting, defaulting to 30 days); this function
// is how §9.5's doctor shows that the setting is actually being enforced, and
// how a test proves the derivation is scoped to the right population.
//
// It stats matched files only. It never opens one, and it never descends: an
// fs.Glob over a two-segment pattern cannot reach depth 3, which is what makes
// the subagent population structurally unreachable from here rather than
// merely excluded by a filter someone could relax.
func (s *Source) DeriveFloor(ctx context.Context, now time.Time, retentionMs int64) (Floor, error) {
	matches, err := fs.Glob(s.fsys, SessionTranscriptGlob)
	if err != nil {
		return Floor{}, fmt.Errorf("transcriptfs: globbing %s: %w", SessionTranscriptGlob, err)
	}

	var floor Floor
	floor.Files = len(matches)
	for _, m := range matches {
		if err := ctx.Err(); err != nil {
			return floor, err
		}
		info, err := fs.Stat(s.fsys, m)
		if err != nil {
			floor.StatErrors++
			continue
		}
		ageMs := now.Sub(observer.TruncateTime(info.ModTime())).Milliseconds()
		if ageMs > floor.OldestAgeMs {
			floor.OldestAgeMs = ageMs
			floor.OldestPath = m
		}
		if retentionMs > 0 && ageMs > retentionMs {
			floor.PastRetention++
		}
	}
	return floor, nil
}

// SettingsFile is the vendor's settings file, read ONCE at startup and
// read-only. Named so the audit and the composition root cite the same string.
const SettingsFile = "settings.json"

// cleanupPeriodDaysKey is the vendor's own retention setting.
const cleanupPeriodDaysKey = "cleanupPeriodDays"

// RetentionMs reads `cleanupPeriodDays` from settings.json and returns it in
// milliseconds. The fs.FS is rooted at the CLAUDE ROOT, not at projects: this
// is the one thing the transcript concern needs from outside its own tree, and
// passing it a separate read-only fs.FS is cheaper than a second adapter.
//
// The second return is false when the key is absent, unreadable, or not a
// positive number — in which case the caller keeps
// observer.DefaultTranscriptRetentionMs, which is the vendor's own documented
// default of 30 days. Measured on this machine: the key is ABSENT, so the
// default is in force, and the 30.11-day floor confirms it.
//
// A zero or negative value is rejected rather than honoured. `cleanupPeriodDays:
// 0` would make every background record "beyond retention" and answer live:
// null for the whole population, which is a wrong answer derived from a setting
// we do not understand.
func RetentionMs(claudeRoot fs.FS) (int64, bool) {
	if claudeRoot == nil {
		return 0, false
	}
	// An absent file and an unreadable one lead to the same place — the caller
	// keeps the default — so they are not distinguished. Settings are read once
	// at startup and a failure here is not worth a degraded mode.
	data, err := fs.ReadFile(claudeRoot, SettingsFile)
	if err != nil {
		return 0, false
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, false
	}
	v, ok := raw[cleanupPeriodDaysKey]
	if !ok {
		return 0, false
	}
	// A QUOTED number is refused rather than accepted. encoding/json will
	// happily decode `"30"` into a json.Number, so a type check on the decode
	// is not enough: the leading byte has to be a digit or a minus sign. A
	// vendor that starts writing this key as a string has changed the format,
	// and guessing at a changed format is what this codebase keeps measuring
	// itself to have got wrong.
	if len(v) == 0 || (v[0] != '-' && (v[0] < '0' || v[0] > '9')) {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, false
	}
	days, err := n.Int64()
	if err != nil || days <= 0 {
		return 0, false
	}
	return days * 24 * 60 * 60 * 1000, true
}
