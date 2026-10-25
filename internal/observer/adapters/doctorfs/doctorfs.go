// Package doctorfs is §9.5's doctor: it reads ONE LIVE EXAMPLE of each format
// tt depends on and reports, per assumption, whether that assumption still
// holds.
//
// # Read-only, and bounded
//
// Every read here is an open-for-read, a stat or a bounded tail. The package
// contains no write call of any kind, which the §9.4(a) package-keyed audit
// enforces for the whole module — doctorfs is not one of the two packages that
// audit exempts, so a write added here fails `go test ./internal/observer/audit`
// before it can reach a user's `~/.claude/`.
//
// Bounded matters as much as read-only. The transcript population on this
// machine is 630 MB across 4 937 files; the doctor reads the TAIL of one
// recently-written transcript and STATS the rest. A diagnostic that costs a
// minute is a diagnostic nobody runs.
//
// # Why this is an adapter and not part of the core
//
// The core's ports return typed records with the keys this build knows about.
// The doctor's entire job is to notice keys this build does NOT know about, so
// it must see the raw objects — which is precisely the vocabulary a port is
// forbidden to return. So it reads for itself, in the adapter layer, and hands
// the core's own DoctorReport vocabulary back out.
package doctorfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// RegistryUniversalKeys is the 19 keys present on EVERY interactive registry
// record measured on this machine (34 of 34 at P10, 37 of 37 at §0.1). A key
// leaving this set is a `fail`: something in tt reads it.
var RegistryUniversalKeys = []string{
	"cwd", "entrypoint", "kind", "messagingSocketPath", "name", "nameSince",
	"nameSource", "peerFeatures", "peerProtocol", "pid", "pidDomain",
	"procStart", "sessionId", "startedAt", "status", "statusUpdatedAt",
	"tmux", "updatedAt", "version",
}

// RegistryOptionalKeys are the keys observed on SOME records. Their absence is
// never a finding; their presence on a record is what the liveness and wait
// vocabularies are built on.
var RegistryOptionalKeys = []string{"formerNames", "waitingFor"}

// JobsUniversalKeys is §0.2's key union over every `state.json` at depth 1.
// §0.2 calls this "20 universal keys" and then lists 19; the list is the
// measured thing and the count was a miscount, so the list is what this check
// uses and the doctor reports the number it actually counts.
var JobsUniversalKeys = []string{
	"backend", "children", "createdAt", "cwd", "daemonShort", "detail",
	"firstTerminalAt", "intent", "linkScanOffset", "linkScanPath", "output",
	"providerEnv", "respawnFlags", "resumeSessionId", "sessionId", "state",
	"template", "tempo", "updatedAt",
}

// AgentsInteractiveKeys and AgentsBackgroundKeys are §0.1's two measured key
// sets for `claude agents --json`. The split is structural: an interactive
// entry has a pid and a `status`, a background entry has an `id` and a `state`.
var (
	AgentsInteractiveKeys = []string{"cwd", "kind", "name", "pid", "sessionId", "startedAt", "status"}
	AgentsBackgroundKeys  = []string{"cwd", "id", "kind", "name", "sessionId", "startedAt", "state"}
)

// UntimestampedRecordTypes is §0.4(d): the eight transcript record types
// observed to carry no `timestamp`. The 64 KB timing tail skips them to find
// the newest record that has one, and a NEW untimestamped type appearing at the
// tail would silently widen how far back that scan must reach.
var UntimestampedRecordTypes = []string{
	"agent-name", "ai-title", "atis-latch", "cost-state",
	"file-history-snapshot", "last-prompt", "mode", "permission-mode",
}

// KnownRegistryStatuses / KnownWaitingFor / KnownJobStates are the literals
// this build maps. An unmapped value is passed through as `unknown` at runtime,
// which is the designed degradation — the doctor is where that degradation
// becomes visible instead of silent.
var (
	KnownRegistryStatuses = []string{"busy", "idle", "shell", "waiting"}
	KnownWaitingFor       = []string{"input needed", "permission prompt"}
	KnownJobStates        = []string{"blocked", "done", "failed", "working"}
	KnownTempo            = []string{"blocked", "done", "failed", "idle", "working"}
)

// tmuxRefPattern is the same anchored, colon-safe expression registryfs parses
// with. Duplicated deliberately: the doctor's job is to check the format
// against what the parser expects, and importing the parser's own opinion of a
// malformed reference would make the check agree with itself by construction.
var tmuxRefPattern = regexp.MustCompile(`^(.+):(@\d+)\.(%\d+)$`)

// timingTailBytes is the 64 KB the production timing tail reads. The doctor
// reads the same amount so that "the newest timestamped record is N bytes back"
// is a statement about the production bound and not about a bigger one.
const timingTailBytes = 64 << 10

// enrichTailBytes is the 200 KB colour/error pass.
const enrichTailBytes = 200 << 10

// Config is what the doctor reads and what it reads it with.
type Config struct {
	Paths observer.Paths
	// StateDir is ~/.local/state/tt — read for the running server's pid file.
	// The doctor never creates it.
	StateDir string
	// Agents is the corroboration source, or nil. Nil is not an error: the
	// two checks that need a listing report `skip`, which is a different claim
	// from `ok`.
	Agents observer.AgentsSource
	// RetentionMs is the configured transcript retention window, so the floor
	// check compares against what the observer actually enforces rather than
	// against a constant repeated here.
	RetentionMs int64
	// Now is the run's single instant. Passed in for the same reason every
	// other derived number in this observer is: two clock reads inside one
	// report can disagree, and a diagnostic that contradicts itself is worse
	// than no diagnostic.
	Now time.Time
	// ProcStats reads cumulative CPU and RSS for a pid. A seam, so the pid-file
	// check is testable without a live server.
	ProcStats func(pid int) (cpu string, rssKB int64, err error)
}

// Run executes every check and returns the report. It never returns an error:
// a check that cannot run says so, in the report, with a verdict of its own.
// An error return would collapse twenty-two independent answers into one.
func Run(ctx context.Context, cfg Config) observer.DoctorReport {
	started := time.Now()
	if cfg.ProcStats == nil {
		cfg.ProcStats = psStats
	}

	d := &doctor{cfg: cfg}
	checks := []func(context.Context) observer.DoctorCheck{
		d.registryKeys,
		d.registryStatusValues,
		d.registryWaitingFor,
		d.registryTmuxShape,
		d.registryMtimeTracksStatus,
		d.registryVersionSpread,
		d.jobsKeys,
		d.jobsStateValues,
		d.jobsDirMtimeTracksRecord,
		d.jobsUpdatedAtNotEarlier,
		d.jobsTimelineShape,
		d.jobsLinkScanPath,
		d.jobsSignalKeys,
		d.transcriptRetentionFloor,
		d.transcriptDeepPopulation,
		d.transcriptUntimestampedTypes,
		d.transcriptAgentColorShape,
		d.transcriptAPIErrorKey,
		d.subagentMetaFiles,
		d.agentsKeys,
		d.terminalStateGate,
		d.observeServer,
	}

	report := observer.DoctorReport{
		TakenAt: observer.TruncateTime(cfg.Now),
		Paths:   cfg.Paths,
		Checks:  make([]observer.DoctorCheck, 0, len(checks)),
	}
	for _, fn := range checks {
		report.Checks = append(report.Checks, fn(ctx))
	}
	report.DurationMs = time.Since(started).Milliseconds()
	return report
}

// doctor carries the config plus the reads that more than one check needs, so
// the registry directory is read once rather than six times.
type doctor struct {
	cfg Config

	regOnce  bool
	regRecs  []rawRecord
	regErr   error
	jobsOnce bool
	jobRecs  []rawRecord
	jobsErr  error

	agentsOnce bool
	agentsRes  observer.AgentsResult
	agentsErr  error
}

// rawRecord is one JSON object as it is ON DISK: its keys, undecoded, plus the
// file's own stat. Undecoded is the point — a typed struct would drop exactly
// the keys this package exists to notice.
type rawRecord struct {
	Path   string
	Dir    string
	Keys   map[string]json.RawMessage
	ModEnd time.Time
}

// str returns a string-valued key, or "".
func (r rawRecord) str(key string) string {
	raw, ok := r.Keys[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// num returns a number-valued key as float64, and whether it was one.
func (r rawRecord) num(key string) (float64, bool) {
	raw, ok := r.Keys[key]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

// stamp returns a key as a time, accepting both shapes the vendor uses: an
// RFC3339 string and epoch milliseconds. Both are live in these formats
// (`statusUpdatedAt` is a string, `startedAt` is millis) and a checker that
// accepted only one would report a working field as broken.
func (r rawRecord) stamp(key string) (time.Time, bool) {
	raw, ok := r.Keys[key]
	if !ok {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	}
	var ms float64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(int64(ms)).UTC(), true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// shared reads
// ---------------------------------------------------------------------------

// registry reads every ~/.claude/sessions/*.json once. `.key` siblings are
// never opened — measured fact: there is consistently one more of them than
// there are records, and nothing in tt has any business reading them.
func (d *doctor) registry() ([]rawRecord, error) {
	if d.regOnce {
		return d.regRecs, d.regErr
	}
	d.regOnce = true
	d.regRecs, d.regErr = readObjects(d.cfg.Paths.Registry, ".json")
	return d.regRecs, d.regErr
}

// jobs reads every ~/.claude/jobs/<shortId>/state.json at DEPTH 1 only. The
// depth bound is M13: this machine carries decoy state.json files at depths 5
// and 8 under one job's tmp/, and a recursive walk would read them.
func (d *doctor) jobs() ([]rawRecord, error) {
	if d.jobsOnce {
		return d.jobRecs, d.jobsErr
	}
	d.jobsOnce = true

	entries, err := os.ReadDir(d.cfg.Paths.Jobs)
	if err != nil {
		d.jobsErr = err
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(d.cfg.Paths.Jobs, e.Name(), "state.json")
		rec, err := readObject(path)
		if err != nil {
			continue
		}
		rec.Dir = e.Name()
		d.jobRecs = append(d.jobRecs, rec)
	}
	sort.Slice(d.jobRecs, func(i, j int) bool { return d.jobRecs[i].Dir < d.jobRecs[j].Dir })
	return d.jobRecs, nil
}

// agents takes ONE corroboration listing per report, shared by the two checks
// that need it. Two spawns per doctor run would double the only cost this tool
// has.
func (d *doctor) agents(ctx context.Context) (observer.AgentsResult, error) {
	if d.agentsOnce {
		return d.agentsRes, d.agentsErr
	}
	d.agentsOnce = true
	if d.cfg.Agents == nil {
		d.agentsErr = fmt.Errorf("no corroboration source is wired into this build")
		return d.agentsRes, d.agentsErr
	}
	d.agentsRes, d.agentsErr = d.cfg.Agents.List(ctx)
	return d.agentsRes, d.agentsErr
}

// readObjects reads every file with the given extension directly under dir.
func readObjects(dir, ext string) ([]rawRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]rawRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ext {
			continue
		}
		rec, err := readObject(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// readObject reads one JSON object, keeping its keys undecoded.
func readObject(path string) (rawRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rawRecord{}, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return rawRecord{}, err
	}
	rec := rawRecord{Path: path, Keys: keys}
	if st, err := os.Stat(path); err == nil {
		rec.ModEnd = st.ModTime().UTC()
	}
	return rec, nil
}

// keyCounts tallies how many records carry each key.
func keyCounts(recs []rawRecord) map[string]int {
	out := map[string]int{}
	for _, r := range recs {
		for k := range r.Keys {
			out[k]++
		}
	}
	return out
}

// universalKeys is the subset of keys present on EVERY record.
func universalKeys(recs []rawRecord) map[string]int {
	counts := keyCounts(recs)
	out := map[string]int{}
	for k, n := range counts {
		if n == len(recs) {
			out[k] = n
		}
	}
	return out
}

// valueCounts tallies the distinct values of one string key.
func valueCounts(recs []rawRecord, key string) map[string]int {
	out := map[string]int{}
	for _, r := range recs {
		if _, ok := r.Keys[key]; !ok {
			continue
		}
		out[r.str(key)]++
	}
	return out
}

// sortedPairs renders a count map as "value ×n" lines, most frequent first, so
// two runs of the doctor produce comparable text.
func sortedPairs(m map[string]int) []string {
	type kv struct {
		k string
		n int
	}
	pairs := make([]kv, 0, len(m))
	for k, n := range m {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].k < pairs[j].k
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		name := p.k
		if name == "" {
			name = `""`
		}
		out = append(out, fmt.Sprintf("%s ×%d", name, p.n))
	}
	return out
}

// unknownValues returns the observed values not in the known set.
func unknownValues(observed map[string]int, known []string) []string {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	var out []string
	for v := range observed {
		if !set[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// check is the constructor every check ends with, so no check can forget its
// citation or return a verdict with no evidence behind it.
func check(id, title, cite string, status observer.DoctorStatus, detail string, evidence ...string) observer.DoctorCheck {
	return observer.DoctorCheck{
		ID: id, Title: title, Cite: cite,
		Status: status, Detail: detail, Evidence: evidence,
	}
}

// skipped is the answer for a tree that is not there. "Claude has never run
// here" is a fact about the machine, not a defect in it.
func skipped(id, title, cite, why string) observer.DoctorCheck {
	return check(id, title, cite, observer.DoctorSkip, why)
}

// ---------------------------------------------------------------------------
// registry checks
// ---------------------------------------------------------------------------

func (d *doctor) registryKeys(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-keys"
		title = "every interactive registry record still carries the 19 keys this build reads"
		cite  = "§0.1, §9.5"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, fmt.Sprintf("no registry records read from %s (%v)", d.cfg.Paths.Registry, err))
	}

	diff := observer.DiffKeys(RegistryUniversalKeys, universalKeys(recs))
	counts := keyCounts(recs)
	optional := []string{}
	for _, k := range RegistryOptionalKeys {
		optional = append(optional, fmt.Sprintf("%s on %d/%d", k, counts[k], len(recs)))
	}
	// A key present on SOME records is not missing from the format, it is
	// optional — so the "added" set is computed against both lists.
	added := observer.DiffKeys(append(append([]string{}, RegistryUniversalKeys...), RegistryOptionalKeys...), counts).Added

	evidence := []string{
		fmt.Sprintf("%d records under %s", len(recs), d.cfg.Paths.Registry),
		fmt.Sprintf("universal keys: %d", len(universalKeys(recs))),
		"documented optionals: " + strings.Join(optional, ", "),
	}
	for _, k := range added {
		evidence = append(evidence, fmt.Sprintf("NEW optional key %q on %d/%d records", k, counts[k], len(recs)))
	}

	switch {
	case len(diff.Missing) > 0:
		return check(id, title, cite, observer.DoctorFail,
			"keys this build reads are gone: "+strings.Join(diff.Missing, ", "),
			evidence...)
	case len(added) > 0:
		return check(id, title, cite, observer.DoctorWarn,
			"the format grew keys this build does not read: "+strings.Join(added, ", ")+
				" — nothing breaks, but the record now says more than tt reports",
			evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "all 19 present on every record", evidence...)
}

func (d *doctor) registryStatusValues(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-status-values"
		title = "every `status` literal is one this build maps"
		cite  = "§3.3, §9.5"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no registry records to read")
	}
	observed := valueCounts(recs, "status")
	unknown := unknownValues(observed, KnownRegistryStatuses)
	evidence := append([]string{"observed: " + strings.Join(sortedPairs(observed), ", ")},
		"mapped: "+strings.Join(KnownRegistryStatuses, ", "))
	if len(unknown) > 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"unmapped status values are being passed through as `unknown`: "+strings.Join(unknown, ", "),
			evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "every value maps", evidence...)
}

func (d *doctor) registryWaitingFor(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-waiting-for"
		title = "`waitingFor` still appears on waiting records, with a value this build maps"
		cite  = "§0.3, §3.3"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no registry records to read")
	}

	waiting, carrying := 0, 0
	for _, r := range recs {
		if r.str("status") != "waiting" {
			continue
		}
		waiting++
		if _, ok := r.Keys["waitingFor"]; ok {
			carrying++
		}
	}
	observed := valueCounts(recs, "waitingFor")
	unknown := unknownValues(observed, KnownWaitingFor)
	evidence := []string{
		fmt.Sprintf("%d waiting records, %d carrying waitingFor", waiting, carrying),
		"values: " + strings.Join(sortedPairs(observed), ", "),
	}

	switch {
	case waiting == 0:
		// Not a pass. §0.3's finding is only reproducible while a session is
		// actually waiting, and reporting ok here would claim an observation
		// nobody made.
		return check(id, title, cite, observer.DoctorSkip,
			"no session is waiting right now, so the field's presence cannot be observed", evidence...)
	case carrying < waiting:
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("%d of %d waiting records carry no waitingFor; those sessions report waitReason `unknown`",
				waiting-carrying, waiting), evidence...)
	case len(unknown) > 0:
		return check(id, title, cite, observer.DoctorWarn,
			"unmapped waitingFor values: "+strings.Join(unknown, ", ")+" — they report as `unknown`", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "present and mapped on every waiting record", evidence...)
}

func (d *doctor) registryTmuxShape(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-tmux-shape"
		title = "`tmux` still matches session:@window.%pane, sigils included"
		cite  = "fact 7, §8.3 item 8, AC8"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no registry records to read")
	}

	var present, matched int
	var bad []string
	for _, r := range recs {
		ref := r.str("tmux")
		if ref == "" {
			continue
		}
		present++
		if tmuxRefPattern.MatchString(ref) {
			matched++
			continue
		}
		if len(bad) < 3 {
			bad = append(bad, ref)
		}
	}
	evidence := []string{
		fmt.Sprintf("%d of %d records carry a reference; %d match the anchored pattern", present, len(recs), matched),
		"pattern: " + tmuxRefPattern.String(),
	}
	switch {
	case present == 0:
		return check(id, title, cite, observer.DoctorSkip, "no record carries a tmux reference", evidence...)
	case matched < present:
		return check(id, title, cite, observer.DoctorFail,
			fmt.Sprintf("%d references do not parse, so the pane gate cannot vouch for those sessions: %s",
				present-matched, strings.Join(bad, " ")), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "every reference parses", evidence...)
}

func (d *doctor) registryMtimeTracksStatus(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-mtime-tracks-status"
		title = "a registry file's mtime still moves with `statusUpdatedAt`"
		cite  = "fact 2, §6.3"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no registry records to read")
	}

	// THE CHECK THE WHOLE WATCH RESTS ON. The registry is rewritten IN PLACE,
	// so the kqueue watch hears a status change as a NOTE_WRITE on the file's
	// own vnode. If Claude ever switched to write-elsewhere-and-rename, the
	// per-file watch would go deaf on the first publish and tt would report
	// stale statuses with no error anywhere — which is why a DIVERGENCE here is
	// a fail and not a warning.
	//
	// The comparison is deliberately coarse (60 s): the file also carries
	// `updatedAt`, which moves for reasons other than a status change, so an
	// exact equality would report every ordinary write as a defect.
	const toleranceSec = 60
	var compared, within int
	var worst time.Duration
	for _, r := range recs {
		at, ok := r.stamp("statusUpdatedAt")
		if !ok || r.ModEnd.IsZero() {
			continue
		}
		compared++
		delta := r.ModEnd.Sub(at)
		if delta < 0 {
			delta = -delta
		}
		if delta <= toleranceSec*time.Second {
			within++
		}
		if delta > worst {
			worst = delta
		}
	}
	evidence := []string{
		fmt.Sprintf("%d records compared; %d within %ds of their statusUpdatedAt", compared, within, toleranceSec),
		fmt.Sprintf("largest gap: %s", worst.Round(time.Second)),
	}
	switch {
	case compared == 0:
		return skipped(id, title, cite, "no record carries both a statusUpdatedAt and a readable mtime")
	case within == 0:
		return check(id, title, cite, observer.DoctorFail,
			"no file's mtime tracks its status timestamp any more; the in-place-write assumption the watch rests on is gone",
			evidence...)
	case within < compared:
		// Expected and benign: `updatedAt` moves on writes that are not status
		// changes, so a file can legitimately be newer than its status.
		return check(id, title, cite, observer.DoctorOK,
			fmt.Sprintf("%d of %d track closely; the rest are later, which is `updatedAt` moving without a status change",
				within, compared), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "every file's mtime tracks its status timestamp", evidence...)
}

func (d *doctor) registryVersionSpread(context.Context) observer.DoctorCheck {
	const (
		id    = "registry-version-spread"
		title = "`version` is per-session and heterogeneous, so a format change lands gradually"
		cite  = "§0.1, §8.3 item 2"
	)
	recs, err := d.registry()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no registry records to read")
	}
	observed := valueCounts(recs, "version")
	evidence := []string{
		fmt.Sprintf("%d distinct version values across %d live records", len(observed), len(recs)),
		strings.Join(sortedPairs(observed), ", "),
	}
	// This check never fails. Its value is the RECORD it leaves: a format
	// change arriving session-by-session is visible here weeks before it is
	// visible anywhere else, and only if somebody wrote the spread down each
	// time.
	return check(id, title, cite, observer.DoctorOK,
		"recorded for comparison with the next run — a widening spread is a format change in flight",
		evidence...)
}

// ---------------------------------------------------------------------------
// jobs checks
// ---------------------------------------------------------------------------

func (d *doctor) jobsKeys(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-keys"
		title = "every background job record still carries the universal keys this build reads"
		cite  = "§0.2, §9.5"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, fmt.Sprintf("no job records under %s (%v)", d.cfg.Paths.Jobs, err))
	}

	universal := universalKeys(recs)
	diff := observer.DiffKeys(JobsUniversalKeys, universal)
	counts := keyCounts(recs)
	added := observer.DiffKeys(JobsUniversalKeys, universal).Added

	evidence := []string{
		fmt.Sprintf("%d depth-1 state.json records; %d universal keys (§0.2 lists %d)",
			len(recs), len(universal), len(JobsUniversalKeys)),
		"optional keys: " + strings.Join(sortedPairs(optionalCounts(counts, len(recs))), ", "),
	}
	if len(diff.Missing) > 0 {
		return check(id, title, cite, observer.DoctorFail,
			"universal keys are gone: "+strings.Join(diff.Missing, ", "), evidence...)
	}
	if len(added) > 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"new universal keys this build does not read: "+strings.Join(added, ", "), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		fmt.Sprintf("all %d present on every record", len(JobsUniversalKeys)), evidence...)
}

// optionalCounts is the keys present on some records but not all.
func optionalCounts(counts map[string]int, total int) map[string]int {
	out := map[string]int{}
	for k, n := range counts {
		if n < total {
			out[k] = n
		}
	}
	return out
}

func (d *doctor) jobsStateValues(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-state-values"
		title = "every `state` literal is one this build maps"
		cite  = "§3.3, §0.2"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}
	observed := valueCounts(recs, "state")
	unknown := unknownValues(observed, KnownJobStates)
	evidence := []string{
		"observed: " + strings.Join(sortedPairs(observed), ", "),
		"mapped: " + strings.Join(KnownJobStates, ", "),
	}
	if len(unknown) > 0 {
		// A fail, not a warn: the terminal-state gate is built on this
		// vocabulary, so an unmapped state makes a liveness verdict `null`
		// rather than merely less descriptive.
		return check(id, title, cite, observer.DoctorFail,
			"unmapped job states break the terminal gate: "+strings.Join(unknown, ", "), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "every value maps", evidence...)
}

func (d *doctor) jobsDirMtimeTracksRecord(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-dir-mtime-tracks-record"
		title = "a job directory's mtime still moves when its state.json is published"
		cite  = "§0.2(b), §6.3"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}

	// §0.2(b): the record is PUBLISHED BY REPLACEMENT, so the parent directory's
	// mtime moves with it and a per-file watch on the old inode goes deaf after
	// the first publish. The watch arms both; this check is what notices if
	// Claude switches to in-place writes, in which case the directory watch
	// stops firing and only the file watch carries the change.
	var compared, tracking int
	for _, r := range recs {
		st, err := os.Stat(filepath.Dir(r.Path))
		if err != nil || r.ModEnd.IsZero() {
			continue
		}
		compared++
		delta := st.ModTime().UTC().Sub(r.ModEnd)
		if delta >= 0 && delta < 5*time.Minute {
			tracking++
		}
	}
	evidence := []string{fmt.Sprintf("%d job directories compared; %d have an mtime at or just after their record's",
		compared, tracking)}
	switch {
	case compared == 0:
		return skipped(id, title, cite, "no job directory could be stat'd")
	case tracking == 0:
		return check(id, title, cite, observer.DoctorWarn,
			"no directory tracks its record any more, which suggests in-place record writes; "+
				"the per-file watch still covers it, the directory watch no longer does", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		fmt.Sprintf("%d of %d track, so replacement-publish still holds", tracking, compared), evidence...)
}

func (d *doctor) jobsUpdatedAtNotEarlier(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-updatedat-not-earlier-than-timeline"
		title = "`updatedAt` is never EARLIER than the last timeline entry"
		cite  = "§0.2(c), §3.4"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}

	// The error has ONE DIRECTION and that is the whole point. `updatedAt` may
	// be LATE (measured up to 20 days), which under-reports how long a job has
	// been blocked — a delay. If it ever went EARLY it would over-report, and an
	// over-reported blockedFor fires a notification immediately and permanently.
	var compared, later, equal, earlier int
	var worstEarly time.Duration
	for _, r := range recs {
		at, ok := r.stamp("updatedAt")
		if !ok {
			continue
		}
		last, ok := lastTimelineAt(filepath.Join(filepath.Dir(r.Path), "timeline.jsonl"))
		if !ok {
			continue
		}
		compared++
		switch delta := at.Sub(last); {
		case delta > time.Millisecond:
			later++
		case delta < -time.Millisecond:
			earlier++
			if -delta > worstEarly {
				worstEarly = -delta
			}
		default:
			equal++
		}
	}
	evidence := []string{fmt.Sprintf("%d records compared: %d equal, %d later, %d EARLIER",
		compared, equal, later, earlier)}
	switch {
	case compared == 0:
		return skipped(id, title, cite, "no record has both an updatedAt and a readable timeline")
	case earlier > 0:
		return check(id, title, cite, observer.DoctorFail,
			fmt.Sprintf("%d records are earlier than their own timeline by up to %s; statusSince now over-reports, "+
				"which fires an attention notification that never clears", earlier, worstEarly.Round(time.Second)),
			evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		"the error direction is intact: late, never early", evidence...)
}

// lastTimelineAt returns the `at` of the last parseable line of a
// timeline.jsonl. Nothing at runtime reads this file (§4.6) — the doctor does,
// because it is the only independent witness to `updatedAt`'s direction.
func lastTimelineAt(path string) (time.Time, bool) {
	lines, err := tailLines(path, timingTailBytes)
	if err != nil {
		return time.Time{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var entry map[string]json.RawMessage
		if json.Unmarshal([]byte(lines[i]), &entry) != nil {
			continue
		}
		var at string
		if json.Unmarshal(entry["at"], &at) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func (d *doctor) jobsTimelineShape(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-timeline-shape"
		title = "timeline.jsonl still carries its four keys"
		cite  = "§0.2, §4.6"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}

	want := []string{"at", "detail", "state", "text"}
	counts := map[string]int{}
	entries, files := 0, 0
	for _, r := range recs {
		lines, err := tailLines(filepath.Join(filepath.Dir(r.Path), "timeline.jsonl"), timingTailBytes)
		if err != nil || len(lines) == 0 {
			continue
		}
		files++
		for _, line := range lines {
			var entry map[string]json.RawMessage
			if json.Unmarshal([]byte(line), &entry) != nil {
				continue
			}
			entries++
			for k := range entry {
				counts[k]++
			}
		}
	}
	if files == 0 {
		return skipped(id, title, cite, "no timeline.jsonl could be read")
	}

	diff := observer.DiffKeys(want, counts)
	evidence := []string{
		fmt.Sprintf("%d entries over %d timelines (tail-bounded)", entries, files),
		"keys: " + strings.Join(sortedPairs(counts), ", "),
	}
	// Nothing at runtime reads this file, so nothing here can break tt. It is
	// checked anyway because it is the witness the check above depends on.
	if len(diff.Missing) > 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"the timeline lost keys: "+strings.Join(diff.Missing, ", ")+
				" — no runtime path reads it, but the updatedAt-direction check does", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "all four present", evidence...)
}

func (d *doctor) jobsLinkScanPath(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-linkscanpath"
		title = "`linkScanPath` names a resolvable, depth-2 session transcript"
		cite  = "§0.4(a), D1"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}

	projects := filepath.Clean(d.cfg.Paths.Projects)
	var named, exists, depth2, outside int
	var missingNames []string
	for _, r := range recs {
		p := r.str("linkScanPath")
		if p == "" {
			continue
		}
		named++
		if rel, err := filepath.Rel(projects, filepath.Clean(p)); err == nil && !strings.HasPrefix(rel, "..") {
			if len(strings.Split(rel, string(filepath.Separator))) == 2 {
				depth2++
			}
		} else {
			outside++
		}
		if _, err := os.Stat(p); err == nil {
			exists++
			continue
		}
		if len(missingNames) < 3 {
			missingNames = append(missingNames, r.Dir)
		}
	}
	evidence := []string{
		fmt.Sprintf("%d of %d records name a transcript; %d targets exist; %d are depth-2 under %s",
			named, len(recs), exists, depth2, projects),
	}
	if len(missingNames) > 0 {
		evidence = append(evidence, "missing targets (expected past the retention floor): "+strings.Join(missingNames, ", "))
	}
	switch {
	case named == 0:
		return check(id, title, cite, observer.DoctorFail,
			"no record names a transcript; background timing has no source and every job's statusSince "+
				"falls back to the record alone", evidence...)
	case named < len(recs):
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("%d records carry no linkScanPath, so those jobs get no transcript-derived timing",
				len(recs)-named), evidence...)
	case outside > 0 || depth2 < named:
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("%d paths are not depth-2 inside the projects root; OpenPath refuses those with transcript-absent",
				named-depth2), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "every record names a depth-2 transcript path", evidence...)
}

func (d *doctor) jobsSignalKeys(context.Context) observer.DoctorCheck {
	const (
		id    = "jobs-signal-keys"
		title = "the liveness signals §3.5 reads still exist: reapedMidWorkAt, tempo, needs/suggestedReply"
		cite  = "§0.2(f), §3.5, §3.6"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}

	counts := keyCounts(recs)
	tempo := valueCounts(recs, "tempo")
	unknownTempo := unknownValues(tempo, KnownTempo)

	// The candidate discriminator §3.6 declines to build on, recorded every run
	// so the transition gets caught if it ever happens while anyone is looking:
	// which records carry needs/suggestedReply, against their state and tempo.
	var pairs []string
	for _, r := range recs {
		_, hasNeeds := r.Keys["needs"]
		_, hasReply := r.Keys["suggestedReply"]
		if !hasNeeds && !hasReply {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s state=%s tempo=%s needs=%t suggestedReply=%t",
			r.Dir, r.str("state"), r.str("tempo"), hasNeeds, hasReply))
	}

	evidence := []string{
		fmt.Sprintf("reapedMidWorkAt on %d/%d, tempo on %d/%d, needs on %d/%d, suggestedReply on %d/%d",
			counts["reapedMidWorkAt"], len(recs), counts["tempo"], len(recs),
			counts["needs"], len(recs), counts["suggestedReply"], len(recs)),
		"tempo values: " + strings.Join(sortedPairs(tempo), ", "),
	}
	evidence = append(evidence, pairs...)

	switch {
	case counts["tempo"] < len(recs):
		return check(id, title, cite, observer.DoctorWarn,
			"tempo is no longer universal, so §3.5's disagreement signal is unavailable on some records",
			evidence...)
	case len(unknownTempo) > 0:
		return check(id, title, cite, observer.DoctorWarn,
			"unrecognised tempo values: "+strings.Join(unknownTempo, ", "), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		"tempo is universal and its values are known; the rest are optional by design", evidence...)
}

// ---------------------------------------------------------------------------
// transcript checks
// ---------------------------------------------------------------------------

// transcriptStat is one transcript file and its mtime.
type transcriptStat struct {
	Path string
	Mod  time.Time
	Size int64
}

// shallowTranscripts is the NON-RECURSIVE population: projects/*/*.jsonl, and
// nothing deeper. The glob is the check — §3.5 rule 4's retention floor is
// derived from this set, and a recursive walk quietly folds in the
// subagents/ tree, which is not reaped at all.
func (d *doctor) shallowTranscripts() []transcriptStat {
	matches, err := filepath.Glob(filepath.Join(d.cfg.Paths.Projects, "*", "*.jsonl"))
	if err != nil {
		return nil
	}
	out := make([]transcriptStat, 0, len(matches))
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, transcriptStat{Path: m, Mod: st.ModTime().UTC(), Size: st.Size()})
	}
	return out
}

func (d *doctor) transcriptRetentionFloor(context.Context) observer.DoctorCheck {
	const (
		id    = "transcript-retention-floor"
		title = "the oldest projects/*/*.jsonl is still near the configured retention window"
		cite  = "§0.4(b), §3.5 rule 4"
	)
	files := d.shallowTranscripts()
	if len(files) == 0 {
		return skipped(id, title, cite, "no transcripts under "+d.cfg.Paths.Projects)
	}

	oldest := files[0]
	for _, f := range files {
		if f.Mod.Before(oldest.Mod) {
			oldest = f
		}
	}
	floorDays := d.cfg.Now.Sub(oldest.Mod).Hours() / 24
	configuredDays := float64(d.cfg.RetentionMs) / float64(24*time.Hour/time.Millisecond)

	evidence := []string{
		fmt.Sprintf("%d files; floor %.2f days; configured %.2f days", len(files), floorDays, configuredDays),
		"oldest: " + oldest.Path,
	}
	// §3.5 rule 4 reads "a missing transcript for a record older than the
	// retention window is not evidence of death". If the real floor moves a day
	// or more below the configured window, that rule starts calling live
	// records unfalsifiable too early — hence a day-scale tolerance rather than
	// an exact one, since the floor drifts by hours within a day as files age.
	switch {
	case configuredDays <= 0:
		return check(id, title, cite, observer.DoctorWarn,
			"no retention window is configured, so rule 4 has nothing to compare against", evidence...)
	case floorDays < configuredDays-1:
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("transcripts are being reaped %.2f days earlier than the configured window; "+
				"beyond-retention verdicts will start arriving early", configuredDays-floorDays), evidence...)
	case floorDays > configuredDays+1:
		return check(id, title, cite, observer.DoctorOK,
			fmt.Sprintf("the floor is %.2f days beyond the window — reaping is behind, which costs nothing",
				floorDays-configuredDays), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "the floor matches the configured window", evidence...)
}

func (d *doctor) transcriptDeepPopulation(context.Context) observer.DoctorCheck {
	const (
		id    = "transcript-deep-population"
		title = "the RECURSIVE transcript population is still visibly different from the shallow one"
		cite  = "§0.4(b), r3 M3"
	)
	shallow := d.shallowTranscripts()
	if len(shallow) == 0 {
		return skipped(id, title, cite, "no transcripts to compare")
	}

	// The companion check, and it is not decoration. The floor above is sound
	// ONLY because the glob is non-recursive: `subagents/` is evidently not
	// reaped at all, so a recursive re-derivation gives a wildly older floor. As
	// long as the two numbers differ, a future implementer's recursive mistake
	// is VISIBLE. If they ever converge, that mistake becomes invisible — so
	// convergence is the finding here, which is the opposite of what a reader
	// expects and the reason this check exists at all.
	var deepCount, beyond int
	var deepOldest time.Time
	cutoff := d.cfg.Now.Add(-time.Duration(d.cfg.RetentionMs) * time.Millisecond)
	_ = filepath.WalkDir(d.cfg.Paths.Projects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil //nolint:nilerr // an unreadable subtree is not a finding
		}
		st, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		deepCount++
		mod := st.ModTime().UTC()
		if deepOldest.IsZero() || mod.Before(deepOldest) {
			deepOldest = mod
		}
		if mod.Before(cutoff) {
			beyond++
		}
		return nil
	})
	if deepCount == 0 {
		return skipped(id, title, cite, "the recursive walk found nothing")
	}

	deepDays := d.cfg.Now.Sub(deepOldest).Hours() / 24
	shallowOldest := shallow[0].Mod
	for _, f := range shallow {
		if f.Mod.Before(shallowOldest) {
			shallowOldest = f.Mod
		}
	}
	shallowDays := d.cfg.Now.Sub(shallowOldest).Hours() / 24

	evidence := []string{
		fmt.Sprintf("shallow: %d files, floor %.2f days", len(shallow), shallowDays),
		fmt.Sprintf("recursive: %d files, floor %.2f days, %d past the retention window", deepCount, deepDays, beyond),
	}
	if deepDays <= shallowDays+1 {
		return check(id, title, cite, observer.DoctorWarn,
			"the two populations have converged, so a recursive re-derivation of the floor would now look correct; "+
				"the non-recursive glob must be asserted by test, not by inspection", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		fmt.Sprintf("the deeper population is %.2f days older and unreaped, so the glob's non-recursiveness stays visible",
			deepDays-shallowDays), evidence...)
}

// newestTranscript is the most recently written transcript, which is the "one
// live example" the record-shape checks read.
func (d *doctor) newestTranscript() (transcriptStat, bool) {
	files := d.shallowTranscripts()
	if len(files) == 0 {
		return transcriptStat{}, false
	}
	newest := files[0]
	for _, f := range files {
		if f.Mod.After(newest.Mod) {
			newest = f
		}
	}
	return newest, true
}

func (d *doctor) transcriptUntimestampedTypes(context.Context) observer.DoctorCheck {
	const (
		id    = "transcript-untimestamped-types"
		title = "no NEW untimestamped record type sits at the tail of a transcript"
		cite  = "§0.4(d), §3.4"
	)
	newest, ok := d.newestTranscript()
	if !ok {
		return skipped(id, title, cite, "no transcript to read")
	}
	lines, err := tailLines(newest.Path, timingTailBytes)
	if err != nil || len(lines) == 0 {
		return skipped(id, title, cite, fmt.Sprintf("could not read the tail of %s: %v", newest.Path, err))
	}

	// The timing tail scans BACKWARD for the newest record carrying a
	// `timestamp`, skipping EVERY record that has none — the production scanner
	// keys on the absence of the key, not on a list of type names, so a new
	// untimestamped type costs scan DISTANCE and nothing else.
	//
	// That is why the verdict below is decided by the distance and not by the
	// type names: the names are recorded as evidence (a type §0.4(d) does not
	// list is worth knowing about) while the thing that can actually break is
	// the 64 KB bound running out. Measured worst case anywhere on this machine:
	// 1 912 B, i.e. 33x headroom. The warning threshold is a quarter of the
	// bound, which is still 8x beyond anything ever observed.
	known := map[string]bool{}
	for _, t := range UntimestampedRecordTypes {
		known[t] = true
	}
	var newTypes []string
	bytesBack, recordsBack := -1, -1
	consumed := 0
	for i := len(lines) - 1; i >= 0; i-- {
		consumed += len(lines[i]) + 1
		var rec map[string]json.RawMessage
		if json.Unmarshal([]byte(lines[i]), &rec) != nil {
			continue
		}
		if _, has := rec["timestamp"]; has {
			if bytesBack < 0 {
				bytesBack = consumed
				recordsBack = len(lines) - i
			}
			continue
		}
		var typ string
		_ = json.Unmarshal(rec["type"], &typ)
		if typ != "" && !known[typ] && bytesBack < 0 {
			newTypes = append(newTypes, typ)
		}
	}

	evidence := []string{
		"read: " + newest.Path,
		fmt.Sprintf("%d lines in the 64 KB tail; newest timestamped record %d bytes / %d records back",
			len(lines), bytesBack, recordsBack),
		"known untimestamped types: " + strings.Join(UntimestampedRecordTypes, ", "),
	}
	if len(newTypes) > 0 {
		sort.Strings(newTypes)
		evidence = append(evidence,
			"untimestamped types at this tail that §0.4(d) does not list: "+strings.Join(newTypes, ", "))
	}

	const headroomWarnBytes = timingTailBytes / 4
	switch {
	case bytesBack < 0:
		return check(id, title, cite, observer.DoctorFail,
			"the 64 KB tail contains NO timestamped record at all; lastActivityAt is activity-since-truncated here",
			evidence...)
	case bytesBack > headroomWarnBytes:
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("the newest timestamp is %d bytes back, past a quarter of the %d-byte bound; "+
				"the tail's headroom is shrinking and lastActivityAt will start coming back empty",
				bytesBack, timingTailBytes), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		fmt.Sprintf("the newest timestamp is %d bytes back, against a %d-byte bound", bytesBack, timingTailBytes),
		evidence...)
}

func (d *doctor) transcriptAgentColorShape(context.Context) observer.DoctorCheck {
	const (
		id    = "transcript-agent-color-shape"
		title = "the agent colour is still a `type: \"agent-color\"` record"
		cite  = "fact 5, §8.3 item 3, AC6"
	)
	newest, ok := d.newestTranscript()
	if !ok {
		return skipped(id, title, cite, "no transcript to read")
	}
	lines, err := tailLines(newest.Path, enrichTailBytes)
	if err != nil {
		return skipped(id, title, cite, "could not read a transcript tail")
	}

	// The distinction that matters, measured: on one session 139 lines CONTAIN
	// the string `agentColor` while only 81 are `agent-color` records — a
	// substring grep overcounts by 72 %. So the check counts both and reports
	// the ratio, because the day they converge is the day the record type went
	// away and a grep-based reader would not notice.
	var records, mentions int
	for _, line := range lines {
		if strings.Contains(line, "agentColor") {
			mentions++
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(rec["type"], &typ)
		if typ == "agent-color" {
			records++
		}
	}
	evidence := []string{
		"read: " + newest.Path,
		fmt.Sprintf("%d agent-color records, %d lines mentioning agentColor, in the 200 KB tail", records, mentions),
	}
	if records == 0 && mentions > 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"the colour is mentioned but no agent-color record remains; enrichment reports colorFound:false",
			evidence...)
	}
	if records == 0 {
		return check(id, title, cite, observer.DoctorSkip,
			"this transcript's tail carries no colour record either way", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "the record type is intact", evidence...)
}

func (d *doctor) transcriptAPIErrorKey(context.Context) observer.DoctorCheck {
	const (
		id    = "transcript-api-error-key"
		title = "`isApiErrorMessage` still exists, and is still ~7x rarer than `is_error`"
		cite  = "fact 11, §8.3 item 5, AC5"
	)
	files := d.shallowTranscripts()
	if len(files) == 0 {
		return skipped(id, title, cite, "no transcript to read")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Mod.After(files[j].Mod) })

	// Counted over the tails of the five most recently written transcripts
	// rather than one, because the two keys are rare enough that a single tail
	// often carries neither, and "0 and 0" would be reported as a change.
	var apiErrors, isErrors, read int
	for _, f := range files {
		if read >= 5 {
			break
		}
		lines, err := tailLines(f.Path, enrichTailBytes)
		if err != nil {
			continue
		}
		read++
		for _, line := range lines {
			if strings.Contains(line, `"isApiErrorMessage"`) {
				apiErrors++
			}
			if strings.Contains(line, `"is_error"`) {
				isErrors++
			}
		}
	}
	evidence := []string{
		fmt.Sprintf("over %d recent transcript tails: isApiErrorMessage on %d lines, is_error on %d",
			read, apiErrors, isErrors),
	}
	if apiErrors == 0 && isErrors == 0 {
		return check(id, title, cite, observer.DoctorSkip,
			"neither key appears in these tails, which is the ordinary case for healthy sessions", evidence...)
	}
	if apiErrors == 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"is_error appears but isApiErrorMessage does not; if the key was renamed, apiErrors now reports 0 forever",
			evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "the key we count is present", evidence...)
}

func (d *doctor) subagentMetaFiles(context.Context) observer.DoctorCheck {
	const (
		id    = "subagent-meta-files"
		title = "subagent counts still come from <session>/subagents/*.meta.json"
		cite  = "fact 10, §8.3 item 6, AC4"
	)
	dirs, err := filepath.Glob(filepath.Join(d.cfg.Paths.Projects, "*", "*", "subagents"))
	if err != nil || len(dirs) == 0 {
		return skipped(id, title, cite, "no subagents/ directory under the projects tree")
	}
	var metas int
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".meta.json") {
				metas++
			}
		}
	}
	// THE METHOD IS THE FIXED THING, NOT THE NUMBER. This count has been
	// measured at 1 556, 1 503 and 1 757 on three consecutive readings, so no
	// test asserts a machine-wide total and this check never fails on the
	// count. It fails only if the LAYOUT goes away.
	evidence := []string{fmt.Sprintf("%d meta files across %d subagents/ directories", metas, len(dirs))}
	if metas == 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"the directories exist but hold no meta files; every session reports subagents:null", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "the layout is intact", evidence...)
}

// ---------------------------------------------------------------------------
// corroboration checks
// ---------------------------------------------------------------------------

func (d *doctor) agentsKeys(ctx context.Context) observer.DoctorCheck {
	const (
		id    = "agents-cli-shape"
		title = "`claude agents --json` still answers, with both populations and their two key sets"
		cite  = "§0.1, §0.3, D2"
	)
	res, err := d.agents(ctx)
	if err != nil || !res.OK {
		// D2: the vendor CLI is TRUSTED by default, so a failure here is a
		// degradation, not a defect in it or in us. Every liveness gate works
		// without it; what is lost is corroboration.
		//
		// This is also the check that MEASURED a wedged `claude agents --json`
		// on this machine during P10 — three of three probes exiting 124 at a
		// 12 s timeout — so the branch below is a path this tool genuinely
		// takes, not a defensive one nobody has seen.
		return check(id, title, cite, observer.DoctorWarn,
			"the listing could not be taken: "+agentsFailure(res, err)+
				"; background jobs report live:null with job-corroboration-stale until it answers again",
			fmt.Sprintf("command: %s; spawn %d ms; cached=%t", res.Command, res.SpawnMs, res.Cached),
			"the two authoritative gates — kill(pid,0) and tmux list-panes — do not depend on this source",
			"D2: the vendor CLI is trusted by default, so this is recorded as degradation and not as a defect")
	}

	var interactive, background, waiting int
	for _, e := range res.Entries {
		if e.PID != nil {
			interactive++
			if e.WaitingFor != "" {
				waiting++
			}
			continue
		}
		background++
	}
	evidence := []string{
		fmt.Sprintf("%d entries: %d interactive, %d background; %d carry waitingFor",
			len(res.Entries), interactive, background, waiting),
		fmt.Sprintf("command: %s; spawn %d ms; cached=%t", res.Command, res.SpawnMs, res.Cached),
		"interactive keys (§0.1): " + strings.Join(AgentsInteractiveKeys, ", "),
		"background keys (§0.1): " + strings.Join(AgentsBackgroundKeys, ", "),
	}
	if len(res.Entries) == 0 {
		return check(id, title, cite, observer.DoctorWarn,
			"the listing is empty while the registry is not; corroboration contributes nothing this run", evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		"both populations present", evidence...)
}

func (d *doctor) terminalStateGate(ctx context.Context) observer.DoctorCheck {
	const (
		id    = "terminal-state-gate"
		title = "every NON-terminal job record is listed by the CLI, and every terminal one is not"
		cite  = "§0.2(a), §3.5 rule 1"
	)
	recs, err := d.jobs()
	if err != nil || len(recs) == 0 {
		return skipped(id, title, cite, "no job records to read")
	}
	res, agentsErr := d.agents(ctx)
	if agentsErr != nil || !res.OK {
		return skipped(id, title, cite,
			"no corroboration listing, so the gate cannot be tested this run: "+agentsFailure(res, agentsErr))
	}
	// A stale listing cannot speak for the present, and the gate compares the
	// listing against records read a moment ago. Refusing here is what keeps
	// the standing falsification test from producing a false alarm.
	if res.ObservedAt.IsZero() {
		return skipped(id, title, cite, "the listing carries no observation instant")
	}

	listed := map[string]bool{}
	for _, e := range res.Entries {
		listed[e.SessionID] = true
	}

	var nonTerminal, terminal int
	var missing, lingering []string
	for _, r := range recs {
		state := r.str("state")
		isTerminal := state == "done" || state == "failed"
		id := r.str("sessionId")
		if isTerminal {
			terminal++
			if listed[id] {
				lingering = append(lingering, r.Dir+"/"+state)
			}
			continue
		}
		nonTerminal++
		if !listed[id] {
			missing = append(missing, r.Dir+"/"+state)
		}
	}

	evidence := []string{
		fmt.Sprintf("%d non-terminal records, %d terminal; listing carries %d entries",
			nonTerminal, terminal, len(res.Entries)),
	}
	if len(missing) > 0 {
		evidence = append(evidence, "non-terminal but unlisted: "+strings.Join(missing, ", "))
	}
	if len(lingering) > 0 {
		evidence = append(evidence, "terminal but listed: "+strings.Join(lingering, ", "))
	}

	// THE STANDING FALSIFICATION TEST for §3.5 rule 1, and the single check most
	// likely to catch a Claude behaviour change. Measured 7/7 on four samples.
	// A disagreement is a warn rather than a fail because the join already
	// handles it — it raises background-gate-disagreement and answers null
	// instead of guessing — but it means the rule is no longer a fact.
	if len(missing) > 0 || len(lingering) > 0 {
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("the gate disagreed on %d records; those sessions answer live:null with "+
				"background-gate-disagreement rather than a verdict", len(missing)+len(lingering)),
			evidence...)
	}
	return check(id, title, cite, observer.DoctorOK,
		fmt.Sprintf("%d/%d records agree with the listing", len(recs), len(recs)), evidence...)
}

// agentsFailure says WHY a listing is unusable, in one string.
//
// The distinction is not pedantry: a port-level error (the spawn never ran) and
// a result-level one (it ran and failed, or timed out) lead to different
// answers, and the live wedge produced the second while the first reads as the
// more obvious explanation. Reporting "<nil>" for the second, as a naive %v of
// the error does, is how a diagnosis says nothing at the moment it matters.
func agentsFailure(res observer.AgentsResult, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case res.Err != nil:
		return res.Err.Error()
	case !res.OK:
		return "the source reported not-ok with no error text (a spawn that was cancelled or never returned)"
	}
	return "no failure"
}

// ---------------------------------------------------------------------------
// the running server
// ---------------------------------------------------------------------------

func (d *doctor) observeServer(context.Context) observer.DoctorCheck {
	const (
		id    = "observe-server"
		title = "a running `tt observe` is accounted for: pid, age, cumulative CPU and RSS"
		cite  = "§5.3, §9.3(d)"
	)
	if d.cfg.StateDir == "" {
		return skipped(id, title, cite, "no state directory was configured")
	}
	path := filepath.Join(d.cfg.StateDir, sockserve.PidName)
	b, err := os.ReadFile(path)
	if err != nil {
		// Not a finding. `tt observe` is started on demand; not running is the
		// ordinary state of a machine that only uses `tt sessions`.
		return check(id, title, cite, observer.DoctorSkip,
			"no server is running (no pid file at "+path+")")
	}
	pf, err := wire.DecodePidFile(b)
	if err != nil {
		return check(id, title, cite, observer.DoctorWarn,
			"the pid file will not decode, so --stop cannot find the server: "+err.Error(),
			"path: "+path)
	}

	evidence := []string{
		fmt.Sprintf("pid %d, socket %s, started %s", pf.PID, pf.Socket, pf.StartedAt),
		"tt " + pf.TT,
	}
	if started, perr := time.Parse(time.RFC3339Nano, pf.StartedAt); perr == nil {
		evidence = append(evidence, fmt.Sprintf("age %s", d.cfg.Now.Sub(started).Round(time.Second)))
	}

	// §9.3(d)'s two numbers, on demand. The user's override — "one single
	// server written in Go with minimal resources, let it run forever" — rests
	// on the premise that a long-lived server stays cheap. This is where that
	// premise is checked without re-running the whole spot-check.
	cpu, rssKB, serr := d.cfg.ProcStats(pf.PID)
	if serr != nil {
		return check(id, title, cite, observer.DoctorWarn,
			"a pid file exists but the process does not answer; the socket is probably stale: "+serr.Error(),
			evidence...)
	}
	evidence = append(evidence, fmt.Sprintf("cumulative CPU %s, RSS %d KB (%.1f MB)",
		cpu, rssKB, float64(rssKB)/1024))

	const rssWarnKB = 200 << 10 // 200 MB
	if rssKB > rssWarnKB {
		return check(id, title, cite, observer.DoctorWarn,
			fmt.Sprintf("the server is holding %.1f MB, well above the ~10 MB a watch-driven observer needs",
				float64(rssKB)/1024), evidence...)
	}
	return check(id, title, cite, observer.DoctorOK, "running and cheap", evidence...)
}

// psStats reads cumulative CPU and RSS from ps(1), which is the same
// measurement §9.3(d) files in validation/ — deliberately the same instrument,
// so the doctor's number and the recorded one are comparable.
func psStats(pid int) (string, int64, error) {
	out, err := exec.Command("/bin/ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("ps -p %d returned %q", pid, strings.TrimSpace(string(out)))
	}
	rss, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("ps rss %q: %w", fields[1], err)
	}
	return fields[0], rss, nil
}

// ---------------------------------------------------------------------------
// the bounded tail
// ---------------------------------------------------------------------------

// tailLines reads at most n bytes from the END of a file and returns its
// complete lines, oldest first. The first (possibly partial) line is dropped
// unless the read reached the start of the file, because half a JSON object is
// not a record and parsing one would be the "torn read" class of bug.
//
// This is the same bounded-tail shape the production reader uses; it is
// reimplemented here rather than imported because transcriptfs reads through an
// fs.FS rooted at the projects directory and the doctor also tails
// timeline.jsonl under jobs/, which is a different root.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // read-only by construction; see the package comment
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	offset := int64(0)
	if size > int64(n) {
		offset = size - int64(n)
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, size-offset)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return nil, err
	}

	lines := strings.Split(string(buf[:read]), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out, nil
}
