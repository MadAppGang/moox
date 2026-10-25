package doctorfs

import (
	"fmt"
	"io"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// RenderText writes the human form of a report.
//
// Plain ASCII markers rather than colour or symbols: this output is read in a
// terminal, pasted into an issue, and diffed against a previous run filed in
// validation/. Colour survives none of those three.
//
// Evidence is printed for EVERY check, including the ones that passed. A doctor
// whose passes are bare cannot be disbelieved, and the reason this exists is
// that everything else in tt degrades quietly — so the one surface that is
// supposed to shout shows its work even when it has nothing to shout about.
func RenderText(w io.Writer, r observer.DoctorReport) error {
	tally := r.Tally()
	var b strings.Builder

	fmt.Fprintf(&b, "tt doctor — %s\n", observer.FormatTime(r.TakenAt))
	fmt.Fprintf(&b, "%d checks in %d ms: %d ok, %d warn, %d fail, %d skipped\n",
		len(r.Checks), r.DurationMs,
		tally[observer.DoctorOK], tally[observer.DoctorWarn],
		tally[observer.DoctorFail], tally[observer.DoctorSkip])
	fmt.Fprintf(&b, "reading %s\n\n", r.Paths.ClaudeRoot)

	for _, c := range r.Checks {
		fmt.Fprintf(&b, "[%-4s] %s  (%s, %s)\n", strings.ToUpper(string(c.Status)), c.ID, c.Cite, c.Title)
		if c.Detail != "" {
			fmt.Fprintf(&b, "        %s\n", c.Detail)
		}
		for _, e := range c.Evidence {
			fmt.Fprintf(&b, "        · %s\n", e)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "verdict: %s\n", r.Worst())
	if tally[observer.DoctorFail] == 0 && tally[observer.DoctorWarn] == 0 {
		b.WriteString("every assumption this build reads is still true on this machine.\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// ExitCode maps a report onto a process exit status, so a script can gate on
// the doctor without parsing anything.
//
//	0  ok, or only skips — nothing this build depends on has changed
//	3  at least one warn — something degrades
//	4  at least one fail — something is broken
//
// 3 and 4 rather than 1 and 2: `tt sessions` already spends 1 for "nothing
// could be read" and 2 for a usage error, and one number meaning two things is
// how a script ends up treating a broken format as a typo.
func ExitCode(r observer.DoctorReport) int {
	switch r.Worst() {
	case observer.DoctorFail:
		return 4
	case observer.DoctorWarn:
		return 3
	default:
		return 0
	}
}
