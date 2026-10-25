package observer

import (
	"errors"
	"testing"
)

// The watch loop publishes the WHOLE population and lets each subscriber's
// Filter narrow it. If its own options dropped launched sessions, an
// includeSpawned subscriber — and `tt sessions --all` answered over the socket
// — could never see one, and nothing else would fail.
func TestTheWatchLoopPublishesLaunchedSessions(t *testing.T) {
	opts := (&WatchLoop{}).snapshotOptions(nil)
	if !opts.Filter.IncludeSpawned {
		t.Fatal("the published snapshot drops launched sessions; no subscriber could ask for them back")
	}
	if !opts.IncludeDead || !opts.IncludeUnknownLiveness {
		t.Fatalf("the published snapshot is no longer the full set: %+v", opts)
	}
}

func intRef(v int) *int    { return &v }
func boolRef(v bool) *bool { return &v }

// A pid two live sessions both claim cannot say which one launched the child,
// and a guess is not positive evidence.
func TestAPidTwoSessionsClaimNamesNeitherAsLauncher(t *testing.T) {
	sessions := []Session{
		{SessionID: "a", PID: intRef(100), ProcAlive: boolRef(true), Live: boolRef(true)},
		{SessionID: "b", PID: intRef(100), ProcAlive: boolRef(true), Live: boolRef(true)},
		{SessionID: "child", PID: intRef(400), ProcAlive: boolRef(true), Live: boolRef(true)},
	}
	tree := map[int]int{400: 100, 100: 1}
	markSpawned(sessions, func(pid int) (int, error) {
		if p, ok := tree[pid]; ok {
			return p, nil
		}
		return 0, errors.New("no such pid")
	})
	if got := sessions[2].SpawnedBy; got != nil {
		t.Fatalf("spawnedBy = %s on an ambiguous launcher pid", *got)
	}
}

// A walk through a tree that never reaches pid 1 stops at the bound, and the
// memo means a looping tree costs one lookup per distinct pid, not one per hop.
func TestTheWalkIsBoundedAndAsksAboutEachPidOnce(t *testing.T) {
	sessions := []Session{
		{SessionID: "launcher", PID: intRef(100), ProcAlive: boolRef(true), Live: boolRef(true)},
		{SessionID: "child", PID: intRef(400), ProcAlive: boolRef(true), Live: boolRef(true)},
	}
	calls := map[int]int{}
	markSpawned(sessions, func(pid int) (int, error) {
		calls[pid]++
		// 400 -> 401 -> 402 -> 401 -> … : a loop that never meets the launcher.
		switch pid {
		case 400:
			return 401, nil
		case 401:
			return 402, nil
		case 402:
			return 401, nil
		}
		return 0, errors.New("no such pid")
	})
	if got := sessions[1].SpawnedBy; got != nil {
		t.Fatalf("spawnedBy = %s from a loop that never reached the launcher", *got)
	}
	for pid, n := range calls {
		if n != 1 {
			t.Errorf("pid %d was looked up %d times in one rebuild", pid, n)
		}
	}
}

// A value left over from a previous rebuild never survives: the field is a
// function of this rebuild's tree alone.
func TestAStaleSpawnedByIsClearedWhenTheTreeNoLongerSaysSo(t *testing.T) {
	stale := "launcher"
	sessions := []Session{
		{SessionID: "child", PID: intRef(400), ProcAlive: boolRef(true), Live: boolRef(true), SpawnedBy: &stale},
	}
	markSpawned(sessions, func(int) (int, error) { return 0, errors.New("gone") })
	if sessions[0].SpawnedBy != nil {
		t.Fatalf("spawnedBy = %s survived a rebuild whose tree did not support it", *sessions[0].SpawnedBy)
	}
}
