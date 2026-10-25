package wire

import (
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// The doctor's JSON shape. It lives here for the same reason every other shape
// does: two surfaces emit it — `tt sessions --doctor --json` and the MCP
// `doctor` tool — and a DTO owned by either one is a DTO the other can drift
// from. §5.1's AST audit enforces it by banning json struct tags outside this
// package.
//
// omitempty appears nowhere, as everywhere else here. A check with no evidence
// and a check whose evidence array is absent are different claims, and only one
// of them is possible.

// DoctorReport is one diagnosis.
type DoctorReport struct {
	Schema int    `json:"schema"`
	TT     string `json:"tt"`
	// TakenAt is when the run started, RFC3339 with milliseconds, UTC.
	TakenAt    string `json:"takenAt"`
	DurationMs int64  `json:"durationMs"`
	// Verdict is the worst check in the run: ok, skip, warn or fail. A caller
	// that wants one answer reads this and nothing else.
	Verdict string `json:"verdict"`
	// Tally counts every verdict, including the zeroes, so a reader never has
	// to infer "no failures" from an absent key.
	Tally  DoctorTally   `json:"tally"`
	Paths  DoctorPaths   `json:"paths"`
	Checks []DoctorCheck `json:"checks"`
}

// DoctorTally is the count per verdict.
type DoctorTally struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// DoctorPaths names what was read, so a report filed as an artifact says what
// machine state it describes.
type DoctorPaths struct {
	ClaudeRoot string `json:"claudeRoot"`
	Registry   string `json:"registry"`
	Jobs       string `json:"jobs"`
	Projects   string `json:"projects"`
}

// DoctorCheck is one assumption and its verdict.
type DoctorCheck struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Cite     string   `json:"cite"`
	Status   string   `json:"status"`
	Detail   string   `json:"detail"`
	Evidence []string `json:"evidence"`
}

// NewDoctorReport encodes a report. `now` is unused by design — the report
// carries its own instant — and is absent from the signature for that reason:
// a second clock read here could disagree with the one the checks decided on.
func NewDoctorReport(r observer.DoctorReport, o Options) DoctorReport {
	tally := r.Tally()
	out := DoctorReport{
		Schema:     Schema,
		TT:         o.TT,
		TakenAt:    formatDoctorTime(r.TakenAt),
		DurationMs: r.DurationMs,
		Verdict:    string(r.Worst()),
		Tally: DoctorTally{
			OK:   tally[observer.DoctorOK],
			Warn: tally[observer.DoctorWarn],
			Fail: tally[observer.DoctorFail],
			Skip: tally[observer.DoctorSkip],
		},
		Paths: DoctorPaths{
			ClaudeRoot: r.Paths.ClaudeRoot,
			Registry:   r.Paths.Registry,
			Jobs:       r.Paths.Jobs,
			Projects:   r.Paths.Projects,
		},
		Checks: make([]DoctorCheck, 0, len(r.Checks)),
	}
	for _, c := range r.Checks {
		evidence := c.Evidence
		if evidence == nil {
			// An empty array rather than null: every check has evidence even
			// when it has none, and a consumer should not need two code paths
			// to iterate it.
			evidence = []string{}
		}
		out.Checks = append(out.Checks, DoctorCheck{
			ID:       c.ID,
			Title:    c.Title,
			Cite:     c.Cite,
			Status:   string(c.Status),
			Detail:   c.Detail,
			Evidence: evidence,
		})
	}
	return out
}

// formatDoctorTime renders the run's instant in the one format this package
// uses. A zero time renders as the empty string rather than as year 1.
func formatDoctorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return observer.FormatTime(t)
}
