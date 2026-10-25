package observer

import "time"

// This file merges a background session's TWO records into one session.
//
// A background session is served by a process of its own (`claude bg-spare`,
// a child of `claude bg-pty-host`), and that process registers itself in
// ~/.claude/sessions/ exactly as an interactive one does — with "kind":"bg"
// and the id of the job it serves in "jobId". The same process writes the
// job's state.json. So the registry record and the job record describe ONE
// session, and the join used to draw it twice, often with statuses that
// disagreed (measured 2026-09-23: 857ef435 as interactive/waiting AND
// background/working; c318c7d5 as interactive/idle AND background/blocked).
//
// M8's cross-kind rule refused to merge an id seen as both kinds because "a
// merge would have to pick one status from two different vocabularies". That
// rule was written for a different shape — an interactive RESUME of a job's
// id, which really is two things — and it still applies there: only a record
// that says kind "bg" AND names the job is merged. The vocabulary objection is
// answered by the vendor itself: `claude agents --json` merges exactly this
// pair, by jobId, with the rule twinStatus implements, and the output stays in
// the background vocabulary.

// backgroundTwins pairs job records with the registry record of the process
// serving them, keyed by sessionId. A registry record is a twin only when it
// says kind "bg", names a jobId, and a job record with that short id carries
// the same sessionId — jobId is the vendor's join key and the sessionId
// equality is §3.2's, so both have to agree. Registry records are in pid order,
// so when two processes claim one job the first wins, as §3.2's duplicate rule
// has it.
func backgroundTwins(in buildInput) map[string]RegistryRecord {
	if len(in.registry.Records) == 0 || len(in.jobs.Records) == 0 {
		return nil
	}
	jobByShort := make(map[string]string, len(in.jobs.Records))
	for _, j := range in.jobs.Records {
		short := j.DaemonShort
		if short == "" {
			short = j.Dir
		}
		if short != "" {
			jobByShort[short] = j.SessionID
		}
	}
	var twins map[string]RegistryRecord
	for _, rec := range in.registry.Records {
		if rec.Kind != RegistryKindBackground || rec.JobID == "" {
			continue
		}
		if jobByShort[rec.JobID] != rec.SessionID {
			continue
		}
		if _, taken := twins[rec.SessionID]; taken {
			continue
		}
		if twins == nil {
			twins = map[string]RegistryRecord{}
		}
		twins[rec.SessionID] = rec
	}
	return twins
}

// twinVerdict is the merged status of a job and the process serving it.
type twinVerdict struct {
	status  Status
	reason  WaitReason
	source  string
	raw     string
	since   *time.Time
	waiting string
}

// twinStatus is the vendor's rule for the pair (`claude agents --json`,
// printAgentsJson's y(), identical in 2.1.276, 2.1.278 and 2.1.280), in the
// background vocabulary:
//
//  1. the process is busy                          -> working
//  2. the job has settled (done/failed, tempo not
//     "active")                                    -> that state
//  3. the job is blocked (state or tempo), or the
//     process is waiting                           -> blocked
//  4. otherwise                                    -> working
//
// One deliberate departure: a settled job whose process is still ALIVE is not
// ended. The vendor keeps such a session listed, and it is resumed from there
// (c318c7d5 sat in done for 2.4 h with its pid alive, then carried on). It
// reads as the process says — idle, or blocked when it is waiting.
//
// procAlive is the registry pid's gate. A pid known to be dead makes the
// registry record stale (its exit handler never ran), so the job speaks alone.
func twinStatus(job JobRecord, reg RegistryRecord, procAlive *bool) twinVerdict {
	jobStatus, _ := MapJobRecord(job.State, job.Tempo)
	fromJob := twinVerdict{status: jobStatus, source: SourceJobs, raw: job.State, since: job.UpdatedAt}

	if procAlive != nil && !*procAlive {
		return fromJob
	}
	regStatus, regReason := MapRegistryStatus(reg.Status, reg.WaitingFor)
	fromReg := func(s Status) twinVerdict {
		v := twinVerdict{status: s, source: SourceRegistry, raw: reg.Status, since: reg.StatusUpdatedAt}
		if regStatus == StatusWaiting {
			v.reason, v.waiting = regReason, reg.WaitingFor
		}
		return v
	}

	switch {
	case regStatus == StatusBusy:
		return fromReg(StatusWorking)
	case jobStatus.IsTerminal() && job.Tempo != "active":
		if procAlive != nil && *procAlive {
			if regStatus == StatusWaiting {
				return fromReg(StatusBlocked)
			}
			return fromReg(StatusIdle)
		}
		return fromJob
	case jobStatus == StatusBlocked:
		if regStatus == StatusWaiting {
			fromJob.reason, fromJob.waiting = regReason, reg.WaitingFor
		}
		fromJob.status = StatusBlocked
		return fromJob
	case regStatus == StatusWaiting:
		return fromReg(StatusBlocked)
	default:
		fromJob.status = StatusWorking
		return fromJob
	}
}

// twinsDisagree reports whether the two records, read on their own, would put
// the session in different places: one asking for the user and the other not,
// or one busy and the other not working. That is what status-conflict is for —
// the merged answer is still reported, and the advisory says it was a choice.
func twinsDisagree(job JobRecord, reg RegistryRecord) bool {
	j, _ := MapJobRecord(job.State, job.Tempo)
	r, _ := MapRegistryStatus(reg.Status, reg.WaitingFor)
	return j.IsAttention() != r.IsAttention() || (r == StatusBusy) != (j == StatusWorking)
}

// overlayTwin copies onto a merged background session what only the process's
// registry record knows, and what §3.4's tie-break gives the registry when it
// is present (name, cwd, start). The job keeps everything else.
func overlayTwin(s *Session, reg RegistryRecord, procAlive *bool) {
	pid := reg.PID
	s.PID = &pid
	s.ProcAlive = procAlive
	s.MessagingSocketPath = stringPtrOrNil(reg.MessagingSocketPath)
	if reg.Name != "" {
		s.Name = reg.Name
	}
	if reg.NameSource != "" {
		s.NameSource = reg.NameSource
	}
	s.FormerNames = emptyIfNil(reg.FormerNames)
	if reg.CWD != "" {
		s.CWD = reg.CWD
	}
	if reg.StartedAt != nil {
		s.StartedAt = reg.StartedAt
	}
	if reg.Version != "" {
		s.ClaudeVersion = reg.Version
	}
	s.Sources = []string{SourceRegistry, SourceJobs}
	s.ListedBy = []string{SourceRegistry, SourceJobs}
}
