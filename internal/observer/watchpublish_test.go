package observer_test

// The watch accounting on the published snapshot — `sources.<id>.watch`.
//
// P7's LIVE RUN found this missing: every fixture test passed with the block
// null, because the core builds the source reports and the core does not know
// what is armed. P5 returned the numbers and deliberately merged nothing; the
// socket is the first surface for which the block is part of the contract.
// These tests are the ones that would have caught it.

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// sourcedCore is a Snapshotter that reports the two source blocks a real core
// reports — which is what makes the merge observable at all.
type sourcedCore struct {
	at    time.Time
	omit  map[string]bool
	calls int
}

func (c *sourcedCore) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	c.calls++
	sources := map[string]observer.SourceReport{}
	for _, id := range []string{observer.SourceRegistry, observer.SourceJobs} {
		if c.omit[id] {
			continue
		}
		sources[id] = observer.SourceReport{
			ID:         id,
			Role:       "authoritative",
			OK:         true,
			Advisories: []observer.Advisory{},
			Counters:   map[string]int64{},
			Labels:     map[string]string{},
		}
	}
	return &observer.Snapshot{
		ObservedAt: c.at,
		Counts:     observer.NewCounts(),
		Sources:    sources,
		Warnings:   []observer.Advisory{},
		Sessions:   []observer.Session{},
		NotCapable: observer.DefaultNotCapable(),
	}, nil
}

// watchTree is a PathLister with one registry file and two job directories, so
// the arm counts in the published block are numbers a test can predict.
type watchTree struct {
	registry string
	jobs     string
}

func (w watchTree) ListDirs(_ context.Context, dir string) ([]string, error) {
	if dir == w.jobs {
		return []string{w.jobs + "/aaaa", w.jobs + "/bbbb"}, nil
	}
	return nil, nil
}

func (w watchTree) ListFiles(_ context.Context, dir, _ string) ([]string, error) {
	if dir == w.registry {
		return []string{w.registry + "/4242.json"}, nil
	}
	return nil, nil
}

func (w watchTree) Exists(context.Context, string) (bool, error) { return true, nil }

func newWatchLoopForPublish(t *testing.T, core observer.Snapshotter, watcher observer.Watcher) (*observer.WatchLoop, observer.Paths) {
	t.Helper()
	paths := observer.Paths{
		ClaudeRoot: "/fixture/.claude",
		Registry:   "/fixture/.claude/sessions",
		Jobs:       "/fixture/.claude/jobs",
		Projects:   "/fixture/.claude/projects",
	}
	loop, err := observer.NewWatchLoop(observer.WatchLoopConfig{
		Core:  core,
		Watch: watcher,
		Tree:  watchTree{registry: paths.Registry, jobs: paths.Jobs},
		Clock: fakes.NewClock(diffBase),
		Paths: paths,
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}
	return loop, paths
}

// The published snapshot carries the watch block, on both watched sources,
// with the arm counts of THIS rebuild.
func TestThePublishedSnapshotCarriesTheWatchAccounting(t *testing.T) {
	core := &sourcedCore{at: diffBase}
	loop, _ := newWatchLoopForPublish(t, core, fakes.NewWatcher(8))

	snap, _, err := loop.Rebuild(context.Background(), "test")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	reg := snap.Sources[observer.SourceRegistry]
	if reg.Watch == nil {
		t.Fatal("sources.registry.watch is null on a published snapshot; a subscriber cannot see whether the server can push")
	}
	if reg.Watch.Mode == "" {
		t.Fatal("the watch block names no mode")
	}
	// The registry directory plus one record file.
	if reg.Watch.ArmedDirs != 1 || reg.Watch.ArmedFiles != 1 {
		t.Fatalf("registry watch = %+v, want 1 dir and 1 file", reg.Watch)
	}

	jobs := snap.Sources[observer.SourceJobs]
	if jobs.Watch == nil {
		t.Fatal("sources.jobs.watch is null")
	}
	// The jobs root plus its two directories.
	if jobs.Watch.ArmedDirs != 3 {
		t.Fatalf("jobs watch = %+v, want 3 armed dirs (the root and two job dirs)", jobs.Watch)
	}
	if jobs.Watch.ArmIterations < 1 {
		t.Fatalf("armIterations = %d, want at least one enumerate/arm round", jobs.Watch.ArmIterations)
	}

	// Two rebuilds: the second block describes the second rebuild, not a copy
	// of the first. The fixed point converges in one iteration once
	// everything is armed, which is the number that must move.
	second, _, err := loop.Rebuild(context.Background(), "test")
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if got, first := second.Sources[observer.SourceJobs].Watch.ArmIterations, jobs.Watch.ArmIterations; got >= first {
		t.Fatalf("armIterations went %d -> %d; the second rebuild's block is the first one's", first, got)
	}
	if snap.Sources[observer.SourceJobs].Watch.ArmIterations != jobs.Watch.ArmIterations {
		t.Fatal("publishing the second snapshot mutated the first one")
	}
}

// A source the core did not report does not grow a watch block. Inventing one
// would describe a read nobody performed — the same rule that keeps the tmux
// and transcript blocks conditional.
func TestASourceTheCoreDidNotReportGetsNoWatchBlock(t *testing.T) {
	core := &sourcedCore{at: diffBase, omit: map[string]bool{observer.SourceJobs: true}}
	loop, _ := newWatchLoopForPublish(t, core, fakes.NewWatcher(8))

	snap, _, err := loop.Rebuild(context.Background(), "test")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if _, present := snap.Sources[observer.SourceJobs]; present {
		t.Fatal("the jobs source was invented by the watch merge")
	}
	if snap.Sources[observer.SourceRegistry].Watch == nil {
		t.Fatal("the registry lost its watch block because another source was absent")
	}
}

// An unarmable target becomes a source-scoped advisory on the source whose
// tree it is in, and is promoted into the envelope's warnings. Without the
// merge these advisories had an emitter and no route to any consumer.
func TestAnUnarmableTargetIsReportedOnItsOwnSourceAndPromoted(t *testing.T) {
	watcher := fakes.NewWatcher(8)
	watcher.Report = observer.ArmReport{
		Failures: []observer.ArmFailure{{
			Path:  "/fixture/.claude/jobs/aaaa",
			Error: "too many open files",
		}},
	}
	core := &sourcedCore{at: diffBase}
	loop, _ := newWatchLoopForPublish(t, core, watcher)

	snap, _, err := loop.Rebuild(context.Background(), "test")
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	jobs := snap.Sources[observer.SourceJobs]
	if !hasAdvisoryCode(jobs.Advisories, observer.AdvWatchTargetMissing) {
		t.Fatalf("the jobs source carries %v, want a %s", jobs.Advisories, observer.AdvWatchTargetMissing)
	}
	reg := snap.Sources[observer.SourceRegistry]
	if hasAdvisoryCode(reg.Advisories, observer.AdvWatchTargetMissing) {
		t.Fatalf("a jobs-tree failure was reported on the registry: %v", reg.Advisories)
	}
	if !hasAdvisoryCode(snap.Warnings, observer.AdvWatchTargetMissing) {
		t.Fatalf("warnings = %v, want the promoted advisory", snap.Warnings)
	}
	if jobs.Watch == nil || jobs.Watch.ArmFailures == 0 {
		t.Fatalf("jobs watch = %+v, want the failure counted", jobs.Watch)
	}
}

func hasAdvisoryCode(list []observer.Advisory, code string) bool {
	for _, a := range list {
		if a.Code == code {
			return true
		}
	}
	return false
}
