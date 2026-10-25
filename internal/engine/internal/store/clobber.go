package store

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/statedir"
)

// The clobber guard (B20, R24, R25): when the previous snapshot has ≥3
// session ids, the new one has 0, and the freeze store does not account for
// EVERY previous sid BY ID, the save is blocked and moox-last kept. A freeze
// of every Claude window empties the live sids legitimately; the store then
// holds them all and the guard stays quiet.
//
// Every failure degrades to the SAFE side, as in pre_save.sh: an unreadable,
// missing or unparsable store accounts for nothing, which keeps the guard
// armed.

// ClobberMin is the previous-snapshot sid count that arms the guard.
const ClobberMin = 3

// Clobber is the guard's finding.
type Clobber struct {
	Blocked   bool
	Prev      int // the previous snapshot's distinct sids
	New       int
	Accounted int // previous sids the freeze store holds
}

// CheckClobber evaluates the guard. held is the freeze store's sid set.
func CheckClobber(prev, next *snapshot.Snapshot, held map[string]bool) Clobber {
	if prev == nil {
		return Clobber{}
	}
	ps := prev.Sids()
	c := Clobber{Prev: len(ps), New: len(next.Sids())}
	for _, sid := range ps {
		if held[sid] {
			c.Accounted++
		}
	}
	c.Blocked = c.Prev >= ClobberMin && c.New == 0 && c.Accounted < c.Prev
	return c
}

// FreezeHeld reads the session ids the freeze store holds: every
// `;CLAUDE_SID=<id>` token in <freezeDir>/*.state, the same directory the
// shell's cc_store_ns_dir resolves (§4.2). Read-only.
func FreezeHeld(freezeDir, claudeRoot string) map[string]bool {
	held := map[string]bool{}
	d, err := statedir.OpenExisting(freezeDir, claudeRoot)
	if err != nil {
		return held
	}
	defer func() { _ = d.Close() }()
	names, err := d.ReadDirNames()
	if err != nil {
		return held
	}
	for _, n := range names {
		if !strings.HasSuffix(n, ".state") {
			continue
		}
		b, _, err := d.ReadFile(n)
		if err != nil {
			continue
		}
		for _, tok := range strings.Fields(string(b)) {
			if sid, ok := strings.CutPrefix(tok, ";CLAUDE_SID="); ok && sid != "" {
				held[sid] = true
			}
		}
	}
	return held
}

// LogBlocked appends the BLOCKED line to log/clobber-guard.log.
func (s *Store) LogBlocked(c Clobber, now time.Time) error {
	line := fmt.Sprintf("[%s] BLOCKED near-total-wipe save: last had %d CLAUDE_SID, new had %d, the freeze store "+
		"accounts for %d of them (%d unaccounted) — keeping good snapshot",
		now.Format("2006-01-02 15:04:05"), c.Prev, c.New, c.Accounted, c.Prev-c.Accounted)
	err := s.AppendLog(ClobberLogName, line)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
