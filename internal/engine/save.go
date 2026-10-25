package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/statedir"
)

// SaveRequest is one save (§6.2).
type SaveRequest struct {
	Reason string
	// Direct: the caller saves in process (`moox save --direct`, or no
	// daemon). It changes nothing in the algorithm; it is logged.
	Direct bool
	// Contents: re-capture pane contents (explicit saves, backups, the
	// restart save). A set @moox-want-contents (the detach hook) does too.
	Contents bool
	// Force skips the pristine guard (request flag f).
	Force bool
	// ExcludePane is dropped from the snapshot, its layout leaf removed
	// (§8.5 step 4, in process only).
	ExcludePane string
	// ShutdownOwner lets a save proceed while @moox-restoring is
	// shutdown:<this pid> (§8.5).
	ShutdownOwner int
}

// SaveResult is what one save did.
type SaveResult struct {
	Published bool
	// Pristine: the server holds only its bootstrap pane and moox-last
	// describes more (A15); nothing was written.
	Pristine bool
	// File is the published snapshot's basename ("" unless Published).
	File string
	// SnapshotFP is the fingerprint of the snapshot this save built.
	SnapshotFP      string
	Panes, Windows  int
	Sids            int
	AttachedClients int
	// ContentsCaptured: this save re-captured pane contents.
	ContentsCaptured bool
	// Execs counts the forks this save made (tmux and ps), B25.
	Execs int
	Took  time.Duration
}

// Unchanged reports a save that found nothing to publish.
func (r SaveResult) Unchanged() bool { return !r.Published && !r.Pristine }

// counting wraps the ports to count execs (SaveResult.Execs).
type countingTmux struct {
	r tmuxRunner
	n *int
}

func (c countingTmux) Run(ctx context.Context, args ...string) ([]byte, error) {
	*c.n++
	return c.r.Run(ctx, args...)
}

type countingProcs struct {
	p procLister
	n *int
}

func (c countingProcs) Table(ctx context.Context) ([]byte, error) {
	*c.n++
	return c.p.Table(ctx)
}

func (c countingProcs) Jobs(ctx context.Context) ([]byte, error) {
	*c.n++
	return c.p.Jobs(ctx)
}

func (c countingProcs) Env(ctx context.Context, pid int) ([]byte, error) {
	*c.n++
	return c.p.Env(ctx, pid)
}

func (c countingProcs) Alive(pid int) bool { return c.p.Alive(pid) }

func (e *Engine) record() store.Record {
	return store.Record{Pid: os.Getpid(), Epoch: e.now().Unix()}
}

// Save runs one save (§6.2): lock → one tmux read → one ps → recordings →
// registry → sidmap → contents → validate → pristine and clobber guards →
// publish → post → retention. 3–5 forks in the common case, independent of
// pane count (B25).
func (e *Engine) Save(ctx context.Context, r SaveRequest) (res SaveResult, err error) {
	start := e.now()
	tmux := countingTmux{e.tmux, &res.Execs}
	procs := countingProcs{e.procs, &res.Execs}
	defer func() { res.Took = e.now().Sub(start) }()

	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return res, err
	}
	defer func() { _ = st.Close() }()

	// 1. Lock (B22): non-blocking; a held lock is ErrBusy.
	e.step("lock")
	l, err := st.TryLock(e.record())
	if err != nil {
		return res, err
	}
	defer l.Release()

	// 2. One tmux read (B15, B25, F3).
	e.step("read")
	nonce := newNonce()
	out, err := tmux.Run(ctx, capture.ReadArgs(nonce)...)
	if err != nil {
		return res, fmt.Errorf("engine: save read: %w", err)
	}
	rd := capture.ParseRead(out, nonce)
	if !rd.HaveServer {
		return res, errors.New("engine: save read: no server record")
	}
	e.reread(ctx, tmux, &rd, nonce)
	e.readLocalAutoRename(ctx, tmux, &rd, nonce)
	res.AttachedClients = len(rd.Clients)
	if rd.Server.Active != "1" {
		return res, ErrNotActive
	}
	if err := e.checkRestoring(ctx, tmux, st, l, rd.Server.Restoring, r.ShutdownOwner); err != nil {
		return res, err
	}
	if e.afterRead != nil {
		e.afterRead()
	}

	// 3. One ps (B25).
	e.step("ps")
	psOut, err := procs.Table(ctx)
	if err != nil {
		return res, fmt.Errorf("engine: ps: %w", err)
	}
	table := capture.ParsePS(psOut)
	live := capture.Filter(rd)

	// 4. Recordings (B13, B15): by pane id, never by position.
	e.step("recordings")
	recs := e.recordings(live)

	// 5. Registry, with the GC of dead and recycled entries (X12).
	e.step("registry")
	reg := e.registry(rd.Server.ByPidDir, table, procs)

	// 6. sidmap (B6–B10), then the claudish models (B12), one single-pid ps
	// eww each.
	e.step("sidmap")
	sids := sidmap.Map(live.SidmapPanes(), table.Sidmap(), reg)
	models := map[int]string{}
	for _, pid := range capture.ClaudishModelPids(sids, table) {
		if envOut, err := procs.Env(ctx, pid); err == nil {
			models[pid] = capture.ModelFromEnv(envOut)
		}
	}

	prev, prevErr := st.Last()
	if errors.Is(prevErr, ErrNewerFormat) {
		// A newer moox owns this workspace: publishing over it would drop
		// the pristine and clobber guards' predecessor (G1-H4).
		e.logf("NEWER-FORMAT save refused, moox-last kept: %v", prevErr)
		return res, prevErr
	}
	var prevSnap *snapshot.Snapshot
	if prevErr == nil {
		prevSnap = prev.Snapshot
	}

	// 7. Roles and fields (B11–B14, B16, B17).
	snap, rep := capture.Build(capture.Input{
		Live: live, Server: rd.Server, Clients: rd.Clients, GlobalAuto: rd.GlobalAutoRename, PS: table, SIDs: sids,
		Recordings: recs, Models: models, Prev: prevSnap,
		Socket:   snapshot.Socket{Name: nameOf(e.paths.Socket), Key: e.paths.Key},
		Producer: e.opts.Producer, CreatedAt: start, ExcludePane: r.ExcludePane,
	})
	for _, d := range rep.Dups {
		e.logf("DUP-SESSION %s (%s) holds %s, already owned by %s: saved with no sid", d.Target, d.PaneID, d.SID, d.Owner)
	}
	for _, w := range rep.Warnings {
		e.logf("%s", w)
	}

	// 8. Contents: re-captured when the request or the detach hook says so;
	// otherwise the newest contents file is reused for the panes it holds,
	// but only within the same server life (R2-C7).
	e.step("contents")
	captureOn := capture.WantsContents(rd.Server.CaptureContents)
	res.ContentsCaptured = captureOn && (r.Contents || rd.Server.WantContents == "1")
	jsonName, contentsName := st.NewNames(start)
	var contentsBytes []byte
	server := snapshot.Server{Pid: rd.Server.Pid, StartTime: rd.Server.StartTime}
	switch {
	case res.ContentsCaptured:
		entries := e.captureContents(ctx, tmux, nonce, capture.ContentsPanes(snap), capture.CaptureLines(rd.Server.CaptureLines))
		contentsBytes, err = snapshot.EncodeContents(server, entries)
		if err != nil {
			return res, err
		}
		markContents(snap, entries)
		snap.Contents = &snapshot.Contents{File: contentsName, SHA256: snapshot.SHA256(contentsBytes),
			Panes: len(entries), CapturedAt: start.Format(time.RFC3339), Server: &server}
	case captureOn:
		e.reuseContents(st, snap, server, prevSnap)
	}

	res.Panes, res.Windows, res.Sids = len(snap.Panes), len(snap.Windows), len(snap.Sids())

	// 9. Validate (B18): an invalid snapshot is never published (B19).
	e.step("validate")
	if err := snapshot.Validate(snap); err != nil {
		e.logf("INVALID save not published, moox-last kept: %v", err)
		return res, err
	}

	// 9a. Pristine guard (A15).
	e.step("guard")
	if !r.Force && capture.Pristine(snap) && prevSnap != nil && len(prevSnap.Windows) > 1 {
		res.Pristine = true
		e.logf("PRISTINE the server holds only its bootstrap pane; moox-last (%s, %d windows) kept", prev.Name, len(prevSnap.Windows))
		return res, nil
	}
	// 10. Clobber guard (B20).
	if c := store.CheckClobber(prevSnap, snap, store.FreezeHeld(e.paths.FreezeDir, e.claudeRoot)); c.Blocked {
		_ = st.LogBlocked(c, start)
		e.logf("BLOCKED clobber guard: last had %d sids, new 0, freeze store accounts for %d", c.Prev, c.Accounted)
		_, _ = tmux.Run(ctx, "set-option", "-g", "@moox_status", "save blocked")
		return res, ErrBlocked
	}

	// 11. Publish (I2, R21, RR2/RR3), unless nothing differs (S8).
	e.step("publish")
	res.SnapshotFP = snapshot.Fingerprint(snap)
	if prevSnap != nil && snapshot.Fingerprint(prevSnap) == res.SnapshotFP {
		if prev.Healed {
			if err := st.Heal(l, prev.Name); err == nil {
				e.logf("HEALED moox-last %q -> %s", prev.Pointer, prev.Name)
			}
		}
		if res.ContentsCaptured {
			_, _ = tmux.Run(ctx, "set-option", "-gu", "@moox-want-contents")
		}
		return res, nil
	}
	if !res.ContentsCaptured {
		contentsName = ""
	}
	if err := st.Publish(l, snap, jsonName, contentsName, contentsBytes); err != nil {
		return res, err
	}
	res.Published, res.File = true, jsonName
	if prev.Healed {
		e.logf("HEALED moox-last %q -> %s (by publishing)", prev.Pointer, jsonName)
	}
	e.logf("SAVE published %s reason=%s panes=%d windows=%d sids=%d contents=%v direct=%v",
		jsonName, r.Reason, res.Panes, res.Windows, res.Sids, res.ContentsCaptured, r.Direct)

	// 12. Post: one chained call; saves never touch @moox-warning (A16).
	e.step("post")
	post := [][]string{
		{"set-option", "-g", "@moox_status", "saved " + start.Format("15:04")},
		{"set-option", "-g", "@moox_save_seq", strconv.Itoa(rd.Server.SaveSeq + 1)},
	}
	if res.ContentsCaptured {
		post = append(post, []string{"set-option", "-gu", "@moox-want-contents"})
	}
	if _, err := tmux.Run(ctx, chain(post...)...); err != nil {
		e.logf("POST status update failed: %v", err)
	}
	e.step("retention")
	if removed, err := st.Retain(l, store.ParsePolicy(rd.Server.Retention), start); err != nil {
		e.logf("RETENTION %v", err)
	} else if len(removed) > 0 {
		e.logf("RETENTION removed %d file(s)", len(removed))
	}
	return res, nil
}

func chain(cmds ...[]string) []string {
	var out []string
	for i, c := range cmds {
		if i > 0 {
			out = append(out, ";")
		}
		out = append(out, c...)
	}
	return out
}

func nameOf(socket string) string {
	if i := strings.LastIndexByte(socket, '/'); i >= 0 {
		return socket[i+1:]
	}
	return socket
}

// reread re-reads, alone and without their free-text field, the rows whose
// field count was wrong (RR1, R13), and logs them.
func (e *Engine) reread(ctx context.Context, tmux tmuxRunner, rd *capture.Read, nonce string) {
	// A grouped session lists a shared pane/window once per member: re-read
	// each id once, or the appended rows duplicate and Validate refuses
	// every save (G1-M7).
	rd.BadPanes, rd.BadWindows = uniq(rd.BadPanes), uniq(rd.BadWindows)
	for _, id := range rd.BadPanes {
		out, err := tmux.Run(ctx, capture.RereadPaneArgs(nonce, id)...)
		if err != nil || !capture.ParseReread(rd, out, nonce) {
			e.logf("ROW-DROPPED pane %s: its fields do not split even without the title", id)
			continue
		}
		e.logf("ROW-REREAD pane %s: title held a separator or newline, saved without it", id)
	}
	for _, id := range rd.BadWindows {
		out, err := tmux.Run(ctx, capture.RereadWindowArgs(nonce, id)...)
		if err != nil || !capture.ParseReread(rd, out, nonce) {
			e.logf("ROW-DROPPED window %s: its fields do not split even without the name", id)
			continue
		}
		e.logf("ROW-REREAD window %s: name held a separator or newline, saved without it", id)
	}
}

// readLocalAutoRename asks the windows whose own automatic-rename the read
// cannot tell for it, in ONE more exec (I-5; B25 stays constant: one exec
// whatever the window count, and none when no window is ambiguous). A failed
// or partial answer leaves those windows to the read's inference.
func (e *Engine) readLocalAutoRename(ctx context.Context, tmux tmuxRunner, rd *capture.Read, nonce string) {
	ids := capture.AmbiguousAutoRename(*rd)
	if len(ids) == 0 {
		return
	}
	out, err := tmux.Run(ctx, capture.LocalAutoRenameArgs(nonce, ids)...)
	if n := capture.ParseLocalAutoRename(rd, out, nonce); err != nil || n != len(ids) {
		e.logf("AUTO-RENAME-LOCAL %d of %d windows answered (%v): the rest saved from the effective value", n, len(ids), err)
	}
}

func uniq(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// checkRestoring reads @moox-restoring (§5.4): a live pid is ErrRestoring
// (ErrShuttingDown for shutdown, unless it is the caller's own), a value that
// does not parse counts as restoring (fail closed), and a dead pid is cleared
// — conditionally, so a new value set meanwhile survives — and that dead
// restore's restoring-from becomes a pin (§6.2 crash safety).
func (e *Engine) checkRestoring(ctx context.Context, tmux tmuxRunner, st *store.Store, l *store.Lock, value string, owner int) error {
	rs, set, valid := capture.ParseRestoring(value)
	switch {
	case !set:
		return nil
	case !valid:
		return fmt.Errorf("%w (@moox-restoring %q does not parse)", ErrRestoring, value)
	case rs.Kind == "shutdown" && owner != 0 && rs.Pid == owner:
		return nil
	case e.procs.Alive(rs.Pid):
		if rs.Kind == "shutdown" {
			return ErrShuttingDown
		}
		return fmt.Errorf("%w (%s)", ErrRestoring, value)
	}
	if _, err := tmux.Run(ctx, capture.ClearRestoringArgs(value)...); err != nil {
		e.logf("RESTORING-CLEAR failed for dead %s: %v", value, err)
	}
	e.logf("RESTORING-CLEARED dead %s", value)
	if src, ok, err := st.RecoverRestoringFrom(l); ok {
		e.logf("PIN %s: its restore died before the verdict (err=%v)", src, err)
	}
	return nil
}

// recordings reads launch/<N> for live pane numbers only (B13, B15). An empty
// recording counts as absent; only the first line is kept (B1, R35).
func (e *Engine) recordings(live capture.Live) map[string]string {
	out := map[string]string{}
	d, err := statedir.OpenExisting(e.paths.Launch, e.claudeRoot)
	if err != nil {
		return out
	}
	defer func() { _ = d.Close() }()
	names, err := d.ReadDirNames()
	if err != nil {
		return out
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, p := range live.Panes {
		num := capture.PaneNumber(p.ID)
		if !have[num] {
			continue
		}
		b, _, err := d.ReadFile(num)
		if err != nil {
			continue
		}
		if line := relaunch.FirstLine(string(b)); strings.TrimSpace(line) != "" {
			out[p.ID] = line
		}
	}
	return out
}

// registry reads <by-pid dir>/<pid>.session-id and <pid>.start (§6.2 step 5;
// the dir by §4.2's one rule). A dead pid, or a live one whose start token
// differs from its lstart (a recycled pid, PR-M16), is GC'd; liveness is a
// property of the pid, not of a server, so another server's live entry is
// kept (X12). A live pid that started after the ps ran is skipped this save,
// never removed.
func (e *Engine) registry(option string, table capture.Table, procs procLister) map[int]string {
	reg := map[int]string{}
	d, err := statedir.OpenExisting(e.paths.ByPid(option), e.claudeRoot)
	if err != nil {
		return reg
	}
	defer func() { _ = d.Close() }()
	names, err := d.ReadDirNames()
	if err != nil {
		return reg
	}
	tokens := map[string]bool{}
	for _, n := range names {
		tokens[n] = true
	}
	gc := func(pid string, why string) {
		for _, n := range []string{pid + ".session-id", pid + ".start"} {
			if err := d.Remove(n, statedir.TypeRegular, nil); err != nil && !errors.Is(err, fs.ErrNotExist) {
				e.logf("REGISTRY-GC %s: %v", n, err)
			}
		}
		e.logf("REGISTRY-GC %s (%s)", pid, why)
	}
	for _, n := range names {
		ps, ok := strings.CutSuffix(n, ".session-id")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(ps)
		if err != nil || pid <= 0 {
			continue
		}
		if !procs.Alive(pid) {
			gc(ps, "dead")
			continue
		}
		row, inTable := table.Get(pid)
		if !inTable {
			continue
		}
		if tokens[ps+".start"] {
			if tb, _, err := d.ReadFile(ps + ".start"); err == nil {
				if tok := capture.StartToken(string(tb)); tok != "" && tok != row.Start {
					gc(ps, "recycled pid")
					continue
				}
			}
		}
		b, _, err := d.ReadFile(n)
		if err != nil {
			continue
		}
		if sid := strings.TrimSpace(relaunch.FirstLine(string(b))); sid != "" {
			reg[pid] = sid
		}
	}
	return reg
}

// captureContents runs the contents chain(s). A chain that stops early (a
// pane died between the read and the capture, E5) resumes after the last
// marker it printed; the failed pane gets no entry.
func (e *Engine) captureContents(ctx context.Context, tmux tmuxRunner, nonce string, ids []string, lines string) map[string][]byte {
	entries := map[string][]byte{}
	remaining := ids
	for attempt := 0; len(remaining) > 0 && attempt < 5; attempt++ {
		var next []string
		chains := capture.ContentsChains(nonce, remaining, lines)
		for i, c := range chains {
			out, err := tmux.Run(ctx, c.Args...)
			got, last := capture.ParseContents(out, nonce)
			for id, b := range got {
				entries[id] = b
			}
			if err == nil {
				continue
			}
			// the pane after the last marker is the one that failed
			failed := 0
			for j, id := range c.IDs {
				if id == last {
					failed = j + 1
				}
			}
			if failed >= len(c.IDs) { // the last pane's own capture failed
				failed = len(c.IDs) - 1
				delete(entries, c.IDs[failed])
			}
			e.logf("CONTENTS-SKIP %s: %v", c.IDs[failed], err)
			next = append(next, c.IDs[failed+1:]...)
			for _, later := range chains[i+1:] {
				next = append(next, later.IDs...)
			}
			break
		}
		remaining = next
	}
	return entries
}

func markContents(snap *snapshot.Snapshot, entries map[string][]byte) {
	for i := range snap.Panes {
		p := &snap.Panes[i]
		if p.Role == snapshot.RoleTTObserver || p.Role == snapshot.RoleFrozen {
			continue
		}
		_, p.Contents = entries[p.SavedID]
	}
}

// reuseContents points a structural save at the newest contents file when
// its header names THIS server life, marking only rows whose live pane id has
// an entry (§6.1, R2-C7).
func (e *Engine) reuseContents(st *store.Store, snap *snapshot.Snapshot, server snapshot.Server, prev *snapshot.Snapshot) {
	name, data, ok, err := st.NewestContents()
	if err != nil {
		e.logf("CONTENTS-REUSE %s unreadable, saved without contents: %v", name, err)
		return
	}
	if !ok {
		return
	}
	srv, entries, err := snapshot.DecodeContents(data)
	if err != nil {
		e.logf("CONTENTS-REUSE %s does not decode, saved without contents: %v", name, err)
		return
	}
	if !srv.Same(&server) {
		return
	}
	markContents(snap, entries)
	n := 0
	for _, p := range snap.Panes {
		if p.Contents {
			n++
		}
	}
	if n == 0 {
		return
	}
	captured := ""
	if prev != nil && prev.Contents != nil && prev.Contents.File == name {
		captured = prev.Contents.CapturedAt
	}
	snap.Contents = &snapshot.Contents{File: name, SHA256: snapshot.SHA256(data), Panes: n, CapturedAt: captured, Server: &srv}
}
