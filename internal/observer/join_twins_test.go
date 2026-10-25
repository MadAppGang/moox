package observer

import (
	"testing"
	"time"
)

var twinNow = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

func twinTime(ago time.Duration) *time.Time {
	t := twinNow.Add(-ago)
	return &t
}

func twinJob(state, tempo string) JobRecord {
	return JobRecord{
		Dir: "857ef435", Path: "/jobs/857ef435/state.json", DaemonShort: "857ef435",
		SessionID: "857ef435-aa77-46f9-bb28-ca9c5374be9d",
		State:     state, Tempo: tempo, Name: "job name", NameSource: "user",
		UpdatedAt: twinTime(33 * time.Hour),
	}
}

func twinReg(status, waitingFor string) RegistryRecord {
	return RegistryRecord{
		Path: "/sessions/65863.json", PID: 65863,
		SessionID: "857ef435-aa77-46f9-bb28-ca9c5374be9d",
		Status:    status, WaitingFor: waitingFor, StatusUpdatedAt: twinTime(33 * time.Hour),
		Name: "Herds setup with TUI", CWD: "/home/user/dotfiles",
		Kind: RegistryKindBackground, JobID: "857ef435",
		MessagingSocketPath: "/tmp/cc-socks/65863.sock",
	}
}

func TestMapJobRecordReadsTempoAsWellAsState(t *testing.T) {
	cases := []struct {
		state, tempo string
		want         Status
	}{
		// The case that was wrong: stopped mid-turn on a question, state
		// untouched by the permission bridge.
		{"working", "blocked", StatusBlocked},
		{"working", "active", StatusWorking},
		{"working", "idle", StatusWorking},
		{"working", "", StatusWorking},
		{"blocked", "idle", StatusBlocked},
		// A settled job is settled whatever its tempo says.
		{"done", "blocked", StatusDone},
		{"failed", "blocked", StatusFailed},
	}
	for _, c := range cases {
		if got, _ := MapJobRecord(c.state, c.tempo); got != c.want {
			t.Errorf("MapJobRecord(%q, %q) = %s, want %s", c.state, c.tempo, got, c.want)
		}
	}
}

func TestTwinStatusFollowsTheVendorsRuleForTheMergedPair(t *testing.T) {
	alive, dead := boolPtr(true), boolPtr(false)
	cases := []struct {
		name       string
		job        JobRecord
		reg        RegistryRecord
		proc       *bool
		want       Status
		wantSource string
		wantReason WaitReason
	}{
		{"a busy process is working whatever the job last said",
			twinJob("blocked", "blocked"), twinReg("busy", ""), alive, StatusWorking, SourceRegistry, WaitReasonNone},
		// 857ef435, measured: the job reads working/blocked and the process
		// waits on AskUserQuestion. Blocked, with the process's reason.
		{"stopped mid-turn on a question",
			twinJob("working", "blocked"), twinReg("waiting", "input needed"), alive, StatusBlocked, SourceJobs, WaitReasonInputNeeded},
		// c318c7d5, measured: the turn ended on an ask the classifier caught;
		// the process is idle, which cannot say "asked you something".
		{"a turn that ended on an ask",
			twinJob("blocked", "blocked"), twinReg("idle", ""), alive, StatusBlocked, SourceJobs, WaitReasonNone},
		{"the process waits while the job still says working",
			twinJob("working", "active"), twinReg("waiting", "permission prompt"), alive, StatusBlocked, SourceRegistry, WaitReasonPermissionPrompt},
		{"neither asks for the user",
			twinJob("working", "idle"), twinReg("idle", ""), alive, StatusWorking, SourceJobs, WaitReasonNone},
		// The departure from the vendor: settled, but the process lives on and
		// is resumed from there. It is idle, not ended.
		{"done while the process is alive",
			twinJob("done", "idle"), twinReg("idle", ""), alive, StatusIdle, SourceRegistry, WaitReasonNone},
		{"done while the process waits",
			twinJob("done", "idle"), twinReg("waiting", "input needed"), alive, StatusBlocked, SourceRegistry, WaitReasonInputNeeded},
		{"done with no word on the process",
			twinJob("done", "idle"), twinReg("idle", ""), nil, StatusDone, SourceJobs, WaitReasonNone},
		{"a settled state the tempo still calls active is not settled",
			twinJob("failed", "active"), twinReg("idle", ""), alive, StatusWorking, SourceJobs, WaitReasonNone},
		// A dead pid means the registry record outlived its process: the job
		// speaks alone, and "busy" from a corpse is not believed.
		{"a dead process is not believed",
			twinJob("working", "blocked"), twinReg("busy", ""), dead, StatusBlocked, SourceJobs, WaitReasonNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := twinStatus(c.job, c.reg, c.proc)
			if v.status != c.want || v.source != c.wantSource || v.reason != c.wantReason {
				t.Errorf("twinStatus = (%s, %s, %s), want (%s, %s, %s)",
					v.status, v.source, v.reason, c.want, c.wantSource, c.wantReason)
			}
		})
	}
}

// joinBoth runs the join the way snapshot.go does, minus the reads.
func joinBoth(in buildInput) []Session {
	prev := newPreviousState()
	interactive, next := joinRegistry(in, prev, DefaultPolicy())
	background := joinJobs(in, prev, &next, DefaultPolicy())
	return mergeSessions(interactive, background)
}

func twinInput(reg RegistryRecord, job JobRecord, alive bool) buildInput {
	return buildInput{
		now:      twinNow,
		registry: RegistryResult{Records: []RegistryRecord{reg}},
		jobs:     JobsResult{Records: []JobRecord{job}},
		panes:    map[string]bool{},
		procGate: func(int) (bool, error) { return alive, nil },
	}
}

func TestABackgroundProcessRecordMergesIntoItsJobAsOneSession(t *testing.T) {
	all := joinBoth(twinInput(twinReg("waiting", "input needed"), twinJob("working", "blocked"), true))
	if len(all) != 1 {
		for _, s := range all {
			t.Logf("%s %s %s", s.SessionID, s.Kind, s.Status)
		}
		t.Fatalf("%d sessions, want 1: the process record and the job record are one session", len(all))
	}
	s := all[0]
	if s.Kind != KindBackground {
		t.Errorf("kind = %s, want background", s.Kind)
	}
	if s.Status != StatusBlocked || !s.Attention {
		t.Errorf("status = %s attention = %v, want blocked and attention", s.Status, s.Attention)
	}
	if s.PID == nil || *s.PID != 65863 {
		t.Errorf("pid = %v, want the process's 65863", s.PID)
	}
	if s.Live == nil || !*s.Live || s.LiveReason != LiveReasonProcOnly {
		t.Errorf("live = %v (%s), want true by the running process", s.Live, s.LiveReason)
	}
	if s.Name != "Herds setup with TUI" || s.CWD != "/home/user/dotfiles" {
		t.Errorf("name/cwd = %q/%q, want the registry's under §3.4's tie-break", s.Name, s.CWD)
	}
	if s.RawWaitingFor == nil || *s.RawWaitingFor != "input needed" || s.WaitReason != WaitReasonInputNeeded {
		t.Errorf("waiting for %v (%s), want the process's input needed", s.RawWaitingFor, s.WaitReason)
	}
	if len(s.ListedBy) < 2 || s.ListedBy[0] != SourceRegistry || s.ListedBy[1] != SourceJobs {
		t.Errorf("listedBy = %v, want registry then jobs", s.ListedBy)
	}
	if _, has := findAdvisory(s, AdvSessionIDCrossKind); has {
		t.Errorf("a merged pair must not be reported as two kinds sharing an id")
	}
	if _, has := findAdvisory(s, AdvStatusConflict); has {
		t.Errorf("waiting and working/blocked agree once tempo is read; got status-conflict")
	}
}

func TestTwinsThatDisagreeSayWhoSaidWhat(t *testing.T) {
	all := joinBoth(twinInput(twinReg("idle", ""), twinJob("blocked", "blocked"), true))
	if len(all) != 1 {
		t.Fatalf("%d sessions, want 1", len(all))
	}
	a, has := findAdvisory(all[0], AdvStatusConflict)
	if !has {
		t.Fatalf("no status-conflict; got %v", all[0].Advisories)
	}
	raw, _ := a.Fields["rawValues"].([]string)
	if len(raw) != 2 || raw[0] != "idle" || raw[1] != "blocked/blocked" {
		t.Errorf("rawValues = %v, want [idle blocked/blocked]", a.Fields["rawValues"])
	}
}

func TestAProcessRecordThatIsNotATwinStaysItsOwnSession(t *testing.T) {
	cases := map[string]func(*RegistryRecord){
		// M8's own case: an interactive resume of a job's id really is two
		// things, and still draws two rows.
		"an interactive record with the job's session id": func(r *RegistryRecord) {
			r.Kind, r.JobID = "interactive", ""
		},
		"a bg record naming another job": func(r *RegistryRecord) { r.JobID = "deadbeef" },
		"a bg record naming no job":      func(r *RegistryRecord) { r.JobID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			reg := twinReg("waiting", "input needed")
			mutate(&reg)
			all := joinBoth(twinInput(reg, twinJob("working", "blocked"), true))
			if len(all) != 2 {
				t.Fatalf("%d sessions, want 2", len(all))
			}
		})
	}
}

func TestADeadProcessLeavesTheJobToSpeakForItself(t *testing.T) {
	all := joinBoth(twinInput(twinReg("busy", ""), twinJob("working", "blocked"), false))
	if len(all) != 1 {
		t.Fatalf("%d sessions, want 1", len(all))
	}
	s := all[0]
	if s.Status != StatusBlocked {
		t.Errorf("status = %s, want the job's blocked: a dead process's busy is stale", s.Status)
	}
	if s.LiveReason == LiveReasonProcOnly {
		t.Errorf("a dead pid vouched for the session")
	}
}

func findAdvisory(s Session, code string) (Advisory, bool) {
	for _, a := range s.Advisories {
		if a.Code == code {
			return a, true
		}
	}
	return Advisory{}, false
}
