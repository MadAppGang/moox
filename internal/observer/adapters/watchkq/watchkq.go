package watchkq

import (
	"errors"
	"sort"

	"github.com/MadAppGang/moox/internal/observer"
)

// ErrUnsupported is returned by New on a platform with no kqueue, and by the
// nowatch build's Watch. The core treats it as "this process cannot push" and
// runs on-demand (§6.6): a degraded observer is a correct observer that answers
// every query by re-reading, it simply has nothing to notify from.
var ErrUnsupported = errors.New("watchkq: kqueue is not available on this platform")

// The §6.4 descriptor guards. Measured `ulimit -n` on the reference machine is
// 1 048 576 against 65 live targets, so these stop being constraints and become
// guards against pathology — a jobs tree with ten thousand directories must not
// be able to exhaust the process's descriptors, and must not be able to starve
// the registry watch while doing it.
const (
	// MaxFilesCeiling is the upper bound on per-FILE watches (registry records
	// plus job records). The effective cap is min(this, RLIMIT_NOFILE/4).
	MaxFilesCeiling = 1024
	// MaxJobDirs is a SEPARATE cap so a pathological jobs tree cannot starve
	// the registry watch. 14 on the reference machine.
	MaxJobDirs = 64
	// MaxJobTranscripts is bounded by MaxJobDirs anyway; 5 on the reference
	// machine.
	MaxJobTranscripts = 64
)

// Caps is the effective cap set. Zero fields are replaced by the constants
// above, so the zero Caps is the production policy rather than "arm nothing".
type Caps struct {
	MaxFiles          int
	MaxJobDirs        int
	MaxJobTranscripts int
}

func (c Caps) withDefaults() Caps {
	if c.MaxFiles <= 0 {
		c.MaxFiles = MaxFilesCeiling
	}
	if c.MaxJobDirs <= 0 {
		c.MaxJobDirs = MaxJobDirs
	}
	if c.MaxJobTranscripts <= 0 {
		c.MaxJobTranscripts = MaxJobTranscripts
	}
	return c
}

// armOrder is §6.4's priority order, stated once, as data.
//
//	"job directories are armed before job state.json files, which are armed
//	 before job transcripts — because a job directory watch is the only one
//	 that can see a record appear (C1), while the other two are redundancy and
//	 enrichment respectively."
//
// The three stable directories come first and are never capped: releasing one
// of them would make a whole family undiscoverable, and there are exactly
// three. Registry files come before job files because the registry is rewritten
// IN PLACE, so its per-file watch is the only thing that can see a status
// change at all, while a job file watch is belt-and-braces for a mechanism
// measurement says is a replace.
var armOrder = []observer.TargetKind{
	observer.TargetClaudeRoot,
	observer.TargetRegistryDir,
	observer.TargetJobsRoot,
	observer.TargetJobDir,
	observer.TargetRegistryFile,
	observer.TargetJobFile,
	observer.TargetJobTranscript,
}

// prioritise applies the caps in §6.4's priority order and reports which
// family, if any, was cut short.
//
// Two properties matter beyond the ordering, and both are about E5 rather than
// about cost:
//
//   - the choice is DETERMINISTIC (each family is sorted by path before it is
//     capped), because an unstable choice would release and re-arm a different
//     subset on every rebuild, and every release discards queued events;
//   - duplicates are collapsed, because the arm-only waves deliberately pass
//     growing supersets and the same path may arrive twice.
func prioritise(targets []observer.WatchTarget, caps Caps) (accepted []observer.WatchTarget, degraded string) {
	caps = caps.withDefaults()

	byKind := make(map[observer.TargetKind][]observer.WatchTarget, len(armOrder))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.Path == "" || seen[t.Path] {
			continue
		}
		seen[t.Path] = true
		byKind[t.Kind] = append(byKind[t.Kind], t)
	}
	for kind := range byKind {
		list := byKind[kind]
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
		byKind[kind] = list
	}

	filesLeft := caps.MaxFiles
	accepted = make([]observer.WatchTarget, 0, len(targets))
	for _, kind := range armOrder {
		list := byKind[kind]
		switch kind {
		case observer.TargetJobDir:
			if len(list) > caps.MaxJobDirs {
				list = list[:caps.MaxJobDirs]
				// job-dir-cap wins any later degradation: a job directory
				// watch is the only target that can observe a record
				// appearing, so losing one is the only unrecoverable loss.
				degraded = observer.DegradedJobDirCap
			}
		case observer.TargetRegistryFile, observer.TargetJobFile:
			if len(list) > filesLeft {
				list = list[:max(filesLeft, 0)]
				if degraded == "" {
					degraded = observer.DegradedFileWatchCap
				}
			}
			filesLeft -= len(list)
		case observer.TargetJobTranscript:
			if len(list) > caps.MaxJobTranscripts {
				list = list[:caps.MaxJobTranscripts]
				if degraded == "" {
					degraded = observer.DegradedFileWatchCap
				}
			}
		}
		accepted = append(accepted, list...)
	}
	return accepted, degraded
}
