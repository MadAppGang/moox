package watchkq_test

// §6.4's descriptor caps and their PRIORITY ORDER.
//
// The order is stated in the architecture so it is not left to the
// implementation: "job directories are armed before job state.json files,
// which are armed before job transcripts — because a job directory watch is the
// only one that can see a record appear (C1), while the other two are
// redundancy and enrichment respectively."
//
// These tests drive it through Reconcile rather than through an unexported
// helper, so they assert the behaviour a caller gets on either build.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/watchkq"
)

// realTree builds a scratch tree with n job directories (each with a
// state.json), m registry records and k transcripts, and returns the full
// desired target set for it.
func realTree(t *testing.T, jobDirs, regFiles, transcripts int) []observer.WatchTarget {
	t.Helper()
	root := t.TempDir()
	reg := filepath.Join(root, "sessions")
	jobs := filepath.Join(root, "jobs")
	for _, d := range []string{reg, jobs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	targets := []observer.WatchTarget{
		{Kind: observer.TargetRegistryDir, Path: reg},
		{Kind: observer.TargetJobsRoot, Path: jobs},
	}
	for i := 0; i < jobDirs; i++ {
		dir := filepath.Join(jobs, fmt.Sprintf("job%04d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(dir, "state.json")
		if err := os.WriteFile(state, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		targets = append(targets,
			observer.WatchTarget{Kind: observer.TargetJobDir, Path: dir},
			observer.WatchTarget{Kind: observer.TargetJobFile, Path: state},
		)
	}
	for i := 0; i < regFiles; i++ {
		rec := filepath.Join(reg, fmt.Sprintf("%d.json", 1000+i))
		if err := os.WriteFile(rec, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, observer.WatchTarget{Kind: observer.TargetRegistryFile, Path: rec})
	}
	for i := 0; i < transcripts; i++ {
		tr := filepath.Join(root, fmt.Sprintf("t%04d.jsonl", i))
		if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, observer.WatchTarget{Kind: observer.TargetJobTranscript, Path: tr})
	}
	return targets
}

func armWith(t *testing.T, caps watchkq.Caps, targets []observer.WatchTarget) observer.ArmReport {
	t.Helper()
	w, err := watchkq.New(caps)
	if err != nil {
		t.Skipf("no watcher on this platform: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	rep, err := w.Reconcile(context.Background(), targets)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return rep
}

func TestTheJobDirectoryCapNeverStarvesTheRegistryWatch(t *testing.T) {
	targets := realTree(t, 8, 4, 0)
	rep := armWith(t, watchkq.Caps{MaxJobDirs: 3}, targets)

	if got := rep.ByKind[observer.TargetJobDir]; got != 3 {
		t.Errorf("armed %d job directories, want the cap of 3", got)
	}
	if got := rep.ByKind[observer.TargetRegistryFile]; got != 4 {
		t.Errorf("armed %d registry records, want all 4 — a pathological jobs tree must not "+
			"be able to starve the registry watch, which is why the cap is separate", got)
	}
	if got := rep.ByKind[observer.TargetRegistryDir]; got != 1 {
		t.Errorf("the registry DIRECTORY watch was dropped (%d): above a cap the directory "+
			"watch is always kept", got)
	}
	if got := rep.ByKind[observer.TargetJobsRoot]; got != 1 {
		t.Errorf("the jobs root watch was dropped (%d)", got)
	}
	if rep.Degraded != observer.DegradedJobDirCap {
		t.Errorf("Degraded = %q, want %q so sources.<id>.watch.degraded appears in EVERY response",
			rep.Degraded, observer.DegradedJobDirCap)
	}
}

func TestWhenTheFileCapBitesJobDirectoriesAreArmedBeforeJobRecords(t *testing.T) {
	// Six job directories, six job records, two registry records; room for
	// three per-file watches only. Priority: the six directories survive
	// untouched, the registry's two records come before job records, and the
	// job records take what is left.
	targets := realTree(t, 6, 2, 0)
	rep := armWith(t, watchkq.Caps{MaxFiles: 3}, targets)

	if got := rep.ByKind[observer.TargetJobDir]; got != 6 {
		t.Errorf("armed %d job directories, want all 6: a job directory watch is the only "+
			"target that can see a record appear (C1), so it outranks every per-file watch", got)
	}
	if got := rep.ByKind[observer.TargetRegistryFile]; got != 2 {
		t.Errorf("armed %d registry records, want both: the registry is rewritten IN PLACE, "+
			"so its per-file watch is the only thing that can see a status change at all", got)
	}
	if got := rep.ByKind[observer.TargetJobFile]; got != 1 {
		t.Errorf("armed %d job records, want the 1 remaining file budget", got)
	}
	if rep.Degraded != observer.DegradedFileWatchCap {
		t.Errorf("Degraded = %q, want %q", rep.Degraded, observer.DegradedFileWatchCap)
	}
}

func TestTranscriptsAreTheFirstFamilySacrificed(t *testing.T) {
	targets := realTree(t, 2, 1, 5)
	rep := armWith(t, watchkq.Caps{MaxJobTranscripts: 2}, targets)

	if got := rep.ByKind[observer.TargetJobTranscript]; got != 2 {
		t.Errorf("armed %d transcripts, want the cap of 2", got)
	}
	if got := rep.ByKind[observer.TargetJobDir]; got != 2 {
		t.Errorf("armed %d job directories, want 2 — a transcript cap must not touch them", got)
	}
	if got := rep.ByKind[observer.TargetJobFile]; got != 2 {
		t.Errorf("armed %d job records, want 2", got)
	}
	if rep.Degraded != observer.DegradedFileWatchCap {
		t.Errorf("Degraded = %q, want %q", rep.Degraded, observer.DegradedFileWatchCap)
	}
}

func TestTheDefaultCapsAreAboveTheMeasuredSetAndBoundedByTheDescriptorLimit(t *testing.T) {
	caps := watchkq.DefaultCaps()
	// 65 targets measured on the reference machine: 1+37 registry, 1+14+7 jobs,
	// 5 transcripts. The caps are guards against pathology, not budgets.
	if caps.MaxJobDirs < 14 {
		t.Errorf("MaxJobDirs = %d, below the 14 measured today", caps.MaxJobDirs)
	}
	if caps.MaxJobTranscripts < 5 {
		t.Errorf("MaxJobTranscripts = %d, below the 5 measured today", caps.MaxJobTranscripts)
	}
	if caps.MaxFiles < 44 {
		t.Errorf("MaxFiles = %d, below the 44 per-file targets measured today", caps.MaxFiles)
	}
	if caps.MaxFiles > watchkq.MaxFilesCeiling {
		t.Errorf("MaxFiles = %d exceeds the %d ceiling", caps.MaxFiles, watchkq.MaxFilesCeiling)
	}
}

func TestAnUncappedSetIsNotDegraded(t *testing.T) {
	rep := armWith(t, watchkq.Caps{}, realTree(t, 14, 37, 5))
	if rep.Degraded != "" {
		t.Errorf("Degraded = %q on a set the size of the live machine's; the caps are "+
			"guards, not budgets", rep.Degraded)
	}
	if rep.ByKind[observer.TargetJobDir] != 14 {
		t.Errorf("armed %d of 14 job directories", rep.ByKind[observer.TargetJobDir])
	}
}

func TestDuplicateTargetsAreCollapsedSoTheArmOnlyWavesCanPassSupersets(t *testing.T) {
	dir := t.TempDir()
	same := observer.WatchTarget{Kind: observer.TargetRegistryDir, Path: dir}
	rep := armWith(t, watchkq.Caps{}, []observer.WatchTarget{same, same, same})
	if rep.Armed != 1 || rep.ByKind[observer.TargetRegistryDir] != 1 {
		t.Fatalf("a set with three copies of one target armed %d and counted %d; the waves "+
			"pass growing supersets and the same path arrives more than once by design",
			rep.Armed, rep.ByKind[observer.TargetRegistryDir])
	}
}
