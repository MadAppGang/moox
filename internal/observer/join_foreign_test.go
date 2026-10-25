package observer_test

// moox §4.5's foreign-server rule. The registry's tmux ref names no socket, so
// with two tmux servers on one machine a session of the OTHER server is either
// reported dead (its pane is not in our listing) or, when both servers have a
// session of the same name, matched to the wrong pane. Process ancestry decides
// instead, and these are the table's rows.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/registryfs"
	"github.com/MadAppGang/moox/internal/observer/fakes"
)

// pidPanes is a pane gate that also returns pane pids (observer.PanePIDLister).
type pidPanes struct {
	ids  []string
	pids []int
}

func (p pidPanes) ListPanes(ctx context.Context) (map[string]bool, error) {
	m, _, err := p.ListPanePIDs(ctx)
	return m, err
}

func (p pidPanes) ListPanePIDs(context.Context) (map[string]bool, []int, error) {
	m := map[string]bool{}
	for _, id := range p.ids {
		m[id] = true
	}
	return m, append([]int(nil), p.pids...), nil
}

// members is a fake MembershipSource: owned pids, recycled pids, or an error.
type members struct {
	owned    map[int]bool
	recycled map[int]bool
	ownsErr  map[int]error
	err      error
	gotPIDs  []int
}

func (m *members) Membership(_ context.Context, panePIDs []int) (observer.Membership, error) {
	m.gotPIDs = panePIDs
	if m.err != nil {
		return nil, m.err
	}
	return m, nil
}

func (m *members) Owns(pid int) (bool, error) {
	if err := m.ownsErr[pid]; err != nil {
		return false, err
	}
	return m.owned[pid], nil
}

func (m *members) Recycled(pid int) bool { return m.recycled[pid] }

func record(pid int, sid, tmux string) []byte {
	return []byte(fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/home/user/p","startedAt":1789684811204,
"version":"2.1.273","kind":"interactive","entrypoint":"cli","tmux":%q,"name":"s%d","nameSource":"derived",
"updatedAt":1789699206204,"status":"idle","statusUpdatedAt":1789699206204}`, pid, sid, tmux, pid))
}

func foreignObserver(t *testing.T, recs map[int]string, panes pidPanes, m *members) *observer.Observer {
	t.Helper()
	fsys := fstest.MapFS{}
	proc := fakes.NewProc()
	for pid, tmux := range recs {
		fsys[fmt.Sprintf("%d.json", pid)] = &fstest.MapFile{
			Data: record(pid, fmt.Sprintf("00000000-0000-4000-8000-%012d", pid), tmux), Mode: 0o600,
		}
		proc.SetAlive(pid, true)
	}
	clock := fakes.NewClock(tRef)
	deps := observer.Deps{
		Registry:    registryfs.NewFS(fsys, "/fixtures/sessions", clock),
		Jobs:        &fakes.Jobs{},
		Transcripts: fakes.NewTranscripts(),
		Proc:        proc,
		Panes:       panes,
		Clock:       clock,
	}
	if m != nil {
		deps.Members = m
	}
	obs, err := observer.New(deps, observer.Config{Paths: observer.Paths{Registry: "/fixtures/sessions"}})
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func allSessions(t *testing.T, obs *observer.Observer) map[int]observer.Session {
	t.Helper()
	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	opts.IncludeUnknownLiveness = true
	opts.Filter.IncludeSpawned = true
	snap, err := obs.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]observer.Session{}
	for _, s := range snap.Sessions {
		if s.PID != nil {
			out[*s.PID] = s
		}
	}
	return out
}

// TestJoin_ForeignServerIsNotDead: a live Claude on ANOTHER server, whose pane is
// (correctly) absent from this server's listing, was reported dead (pane gone).
// Now it is foreign: never dead, and in no view of this server.
func TestJoin_ForeignServerIsNotDead(t *testing.T) {
	m := &members{owned: map[int]bool{101: true}}
	obs := foreignObserver(t,
		map[int]string{101: "work:@1.%1", 202: "scratch:@1.%1"},
		pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, m)

	got := allSessions(t, obs)
	if len(m.gotPIDs) != 1 || m.gotPIDs[0] != 5001 {
		t.Fatalf("the membership judge was given pane pids %v, want this listing's [5001]", m.gotPIDs)
	}
	ours, ok := got[101]
	if !ok || ours.Live == nil || !*ours.Live {
		t.Fatalf("our own session is not live: %+v", ours)
	}
	if _, ok := got[202]; ok {
		t.Fatalf("the other server's session is in this server's list: %+v", got[202])
	}

	// Without the rule (no MembershipSource), the same input reports it DEAD —
	// the defect being fixed, kept as the control.
	legacy := allSessions(t, foreignObserver(t,
		map[int]string{101: "work:@1.%1", 202: "scratch:@1.%1"},
		pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, nil))
	if s := legacy[202]; s.Live == nil || *s.Live {
		t.Fatalf("control: without membership the foreign session should read dead, got %+v", s)
	}
}

// TestJoin_SameSessionNameOtherServerIsForeign: both servers have a session
// "api" with a pane ref that reads the same. The pane-set check alone says
// "live, here"; ancestry says it is the other server's.
func TestJoin_SameSessionNameOtherServerIsForeign(t *testing.T) {
	m := &members{owned: map[int]bool{101: true}}
	obs := foreignObserver(t,
		map[int]string{101: "api:@1.%1", 202: "api:@1.%1"},
		pidPanes{ids: []string{"api:@1.%1"}, pids: []int{5001}}, m)
	got := allSessions(t, obs)
	if _, ok := got[101]; !ok {
		t.Fatal("our own api session is missing")
	}
	if s, ok := got[202]; ok {
		t.Fatalf("the other server's same-named session is listed here: %+v", s)
	}
}

// TestJoin_ForeignTable covers the rest of moox §4.5's table.
func TestJoin_ForeignTable(t *testing.T) {
	t.Run("owned but the ref is stale: live, paneAlive true, advisory", func(t *testing.T) {
		m := &members{owned: map[int]bool{101: true}}
		got := allSessions(t, foreignObserver(t, map[int]string{101: "work:@9.%99"},
			pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, m))
		s := got[101]
		if s.PaneAlive == nil || !*s.PaneAlive || s.Live == nil || !*s.Live {
			t.Fatalf("paneAlive %v live %v, want both true", s.PaneAlive, s.Live)
		}
		if !hasAdvisory(s, observer.AdvTmuxRefStale) {
			t.Fatalf("no %s advisory: %+v", observer.AdvTmuxRefStale, s.Advisories)
		}
	})
	t.Run("membership error: paneAlive nil plus an advisory, never dead", func(t *testing.T) {
		m := &members{ownsErr: map[int]error{101: errors.New("pid vanished")}}
		got := allSessions(t, foreignObserver(t, map[int]string{101: "work:@1.%1"},
			pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, m))
		s := got[101]
		if s.PaneAlive != nil {
			t.Fatalf("paneAlive = %v, want nil", *s.PaneAlive)
		}
		if s.Live != nil && !*s.Live {
			t.Fatal("a membership error reported the session dead")
		}
		if !hasAdvisory(s, observer.AdvLivenessUnknown) {
			t.Fatalf("no liveness-unknown advisory: %+v", s.Advisories)
		}
	})
	t.Run("ps unreadable: same as an error, for every session", func(t *testing.T) {
		m := &members{err: errors.New("ps failed")}
		got := allSessions(t, foreignObserver(t, map[int]string{101: "work:@1.%1"},
			pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, m))
		if s := got[101]; s.PaneAlive != nil || (s.Live != nil && !*s.Live) {
			t.Fatalf("paneAlive %v live %v; want nil and not dead", s.PaneAlive, s.Live)
		}
	})
	t.Run("a listing with no pane pids judges nothing", func(t *testing.T) {
		m := &members{}
		got := allSessions(t, foreignObserver(t, map[int]string{101: "work:@1.%1"},
			pidPanes{ids: []string{"work:@1.%1"}}, m))
		if s, ok := got[101]; !ok || s.Live == nil || !*s.Live {
			t.Fatalf("with no pane pids the session must be judged by the pane set alone: %+v", s)
		}
		if m.gotPIDs != nil {
			t.Fatal("the membership judge was consulted with no pane pids")
		}
	})
	t.Run("recycled pid: dead", func(t *testing.T) {
		m := &members{owned: map[int]bool{101: true}, recycled: map[int]bool{101: true}}
		got := allSessions(t, foreignObserver(t, map[int]string{101: "work:@1.%1"},
			pidPanes{ids: []string{"work:@1.%1"}, pids: []int{5001}}, m))
		s := got[101]
		if s.Live == nil || *s.Live || s.LiveReason != observer.LiveReasonPIDDead {
			t.Fatalf("live %v reason %q, want false / %q", s.Live, s.LiveReason, observer.LiveReasonPIDDead)
		}
	})
}
