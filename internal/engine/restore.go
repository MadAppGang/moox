package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/capture"
	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/internal/relaunch"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/internal/verdict"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Restore kinds (§6.4).
const (
	KindBoot    = "boot"
	KindRestore = "restore"
)

// The verdict types, re-exported from the facade.
type (
	BootVerdict   = verdict.Boot
	LaunchVerdict = verdict.Launch
)

// RestoreRequest is one restore (§6.4).
type RestoreRequest struct {
	// Path is the absolute snapshot path fixed by the requester; "" means
	// Last(), read under save.lock.
	Path string
	// Kind is KindBoot or KindRestore.
	Kind string
	// Stub (V3/V4, MOOX_STUB_COMMANDS=1): every relaunch becomes a harmless
	// stub; the plan and the verdicts keep the original.
	Stub bool
	// Nonce, for a request (§6.4a): also signal moox-restored-<nonce>.
	Nonce string
}

// RestoreResult is what Restore created. Launch delivers the LAUNCH verdict
// once, after which the restore has released save.lock and @moox-restoring
// (RESTORE-END); it is closed after that one value.
type RestoreResult struct {
	Plan    Plan
	Source  string            // the snapshot path restored from
	Created map[string]string // row key → new pane id
	Boot    BootVerdict
	Launch  <-chan LaunchVerdict
}

var (
	// ErrPopulated: the server holds more than its bootstrap (B47, R28).
	ErrPopulated = errors.New("engine: the server is populated; a restore never runs into it")
	// ErrLockTimeout: save.lock stayed held for the whole bounded wait (§5.4).
	ErrLockTimeout = errors.New("engine: save.lock still held after the restore's wait")
)

// The env names C2 delivers (§4.4).
const (
	envResume  = "MOOX_RESUME"
	envContent = "MOOX_RESTORE_CONTENT"
	envAck     = "MOOX_RESUME_ACK"
)

// Bounds of the restore.
const (
	restoreLockWait = 120 * time.Second
	scriptTimeout   = 30 * time.Second
	// chainLimit keeps one exec's argv under PV-ARGV's ~16 KiB.
	chainLimit = 12 << 10
	// contentsLimit bounds one contents file read at restore (P3: a file
	// over statedir's 8 MiB default needs its own reader).
	contentsLimit = store.ContentsLimit
	moxBootPrefix = "moox-boot-"
)

// restoreOptions are the server values one read gives the restore.
type restoreOptions struct {
	claudishCmd, claudeCmd, processes, strategyNvim string
	root, restoring, version, baseIndex             string
	pid                                             int
}

const optSep = "\x1f"

// readRestoreOptions is one display-message (no literal % in the template:
// display-message runs it through strftime, P3).
func (e *Engine) readRestoreOptions(ctx context.Context) (restoreOptions, error) {
	f := []string{"#{" + relaunch.OptClaudishCmd + "}", "#{" + relaunch.OptClaudeCmd + "}",
		"#{" + relaunch.OptProcesses + "}", "#{" + relaunch.OptStrategyNvim + "}", "#{@moox-root}",
		"#{@moox-restoring}", "#{version}", "#{base-index}", "#{pid}"}
	out, err := e.tmux.Run(ctx, "display-message", "-p", strings.Join(f, optSep))
	if err != nil {
		return restoreOptions{}, err
	}
	v := strings.Split(strings.TrimRight(string(out), "\n"), optSep)
	if len(v) != len(f) {
		return restoreOptions{}, fmt.Errorf("engine: restore options: %d fields, want %d", len(v), len(f))
	}
	pid, _ := strconv.Atoi(v[8])
	return restoreOptions{claudishCmd: v[0], claudeCmd: v[1], processes: v[2], strategyNvim: v[3], root: v[4],
		restoring: v[5], version: v[6], baseIndex: v[7], pid: pid}, nil
}

// source is the snapshot a restore reads, once (T13).
type source struct {
	snap     *snapshot.Snapshot
	path     string
	name     string // basename in the snapshot dir, "" if outside it
	contents map[string][]byte
	warning  string
}

func (e *Engine) loadSource(st *store.Store, path string) (source, error) {
	e.step("read-snapshot")
	if path == "" {
		last, err := st.Last()
		if err != nil {
			return source{}, err
		}
		path = st.Path(last.Name)
	}
	if !filepath.IsAbs(path) {
		return source{}, fmt.Errorf("engine: snapshot path %q is not absolute", path)
	}
	dirPath, base := filepath.Split(path)
	d, err := statedir.OpenExisting(filepath.Clean(dirPath), e.claudeRoot)
	if err != nil {
		return source{}, err
	}
	defer func() { _ = d.Close() }()
	b, _, err := d.ReadFile(base)
	if err != nil {
		return source{}, err
	}
	snap, err := snapshot.Decode(b)
	if err != nil {
		return source{}, err
	}
	if err := snapshot.Validate(snap); err != nil {
		return source{}, err
	}
	src := source{snap: snap, path: path}
	if filepath.Clean(dirPath) == filepath.Clean(st.SnapshotDir()) {
		if p, ok := store.ParseName(base); ok && !p.Contents {
			src.name = base
		}
	}
	src.contents, src.warning = readContents(d, snap)
	return src, nil
}

// contentsNameOK accepts a managed moox-<ts>.contents.gz, and for a
// snapshot outside the store (`moox import --out FILE`, G1-H6) any single
// path component ending in .contents.gz: the file is still read beside the
// snapshot, through statedir (no symlink followed), and must match the
// sha256 the snapshot recorded.
func contentsNameOK(name string) bool {
	if p, ok := store.ParseName(name); ok {
		return p.Contents
	}
	return name != "" && !strings.ContainsAny(name, "/\x00") && !strings.HasPrefix(name, ".") &&
		strings.HasSuffix(name, ".contents.gz")
}

// readContents is the contents file a snapshot names, verified by sha256 and
// server life; any failure drops contents only (RR14).
func readContents(d *statedir.Dir, snap *snapshot.Snapshot) (map[string][]byte, string) {
	if snap.Contents == nil || snap.Contents.File == "" {
		return nil, ""
	}
	if !contentsNameOK(snap.Contents.File) {
		return nil, fmt.Sprintf("CONTENTS-DROPPED %q is not a contents name", snap.Contents.File)
	}
	b, _, err := d.ReadFileLimit(snap.Contents.File, contentsLimit)
	if err != nil {
		return nil, fmt.Sprintf("CONTENTS-DROPPED %v", err)
	}
	if snapshot.SHA256(b) != snap.Contents.SHA256 {
		return nil, fmt.Sprintf("CONTENTS-DROPPED sha256 of %s does not match the snapshot", snap.Contents.File)
	}
	srv, entries, err := snapshot.DecodeContents(b)
	if err != nil {
		return nil, fmt.Sprintf("CONTENTS-DROPPED %v", err)
	}
	if snap.Contents.Server != nil && !snap.Contents.Server.Same(&srv) {
		return nil, "CONTENTS-DROPPED the contents header names another server life"
	}
	return entries, ""
}

// restorer is one restore in progress.
type restorer struct {
	e    *Engine
	ctx  context.Context
	plan Plan
	opts restoreOptions
	is38 bool
	// ver is the server's tmux: how it stores the names and titles a restore
	// sets (tmuxtarget names.go). sessArg is the -s argument of each
	// snapshot session (sessionArgs).
	ver     tmuxtarget.Version
	sessArg map[string]string
	// manual is a requested (not boot) restore: its bootstrap pane is
	// checked again under the lock (G1-2-H1).
	manual bool

	runDir, contentsDir, ackDir string

	created  map[string]string // row key → pane id
	rowPane  []string          // by row index
	winID    []string          // by window index
	sessID   map[string]string // session name → $sid
	warnings []string
	orphans  []string
}

// sessionArgs is the -s argument of every snapshot session: the name decoded
// (tmuxtarget.NameArg). tmux vis-encodes session names, so the name a save
// read back is already encoded, and passing it verbatim would grow every
// backslash by one per restore. Before 3.7 ':' and '.' also become '_', so
// two names of a snapshot taken on a newer tmux ("a:b", "a.b") can meet on
// one stored name there: the later gets a "-2" suffix instead of failing
// new-session with "duplicate session" (every row of it a miss).
func (r *restorer) sessionArgs(snap *snapshot.Snapshot) map[string]string {
	args := make(map[string]string, len(snap.Sessions))
	taken := map[string]bool{}
	for _, se := range snap.Sessions {
		base := tmuxtarget.NameArg(r.ver, se.Name)
		arg := base
		for k := 2; taken[r.stored(arg)]; k++ {
			arg = base + "-" + strconv.Itoa(k)
		}
		taken[r.stored(arg)] = true
		args[se.Name] = arg
		if s := r.stored(arg); s != se.Name {
			r.e.logf("SESSION-NAME %q is created as %q, which this tmux stores as %q", se.Name, arg, s)
		}
	}
	return args
}

// stored is the name this server gives a session created with -s arg.
func (r *restorer) stored(arg string) string { return tmuxtarget.StoredSessionName(r.ver, arg) }

// sessionArgOf is the -s argument of snapshot session name.
func (r *restorer) sessionArgOf(name string) string {
	if a, ok := r.sessArg[name]; ok {
		return a
	}
	return tmuxtarget.NameArg(r.ver, name)
}

// sessionName is the Lit a snapshot session is created with.
func (r *restorer) sessionName(name string) tmuxtarget.Lit {
	return tmuxtarget.Literal(r.sessionArgOf(name))
}

// storedName is what the server will call snapshot session name.
func (r *restorer) storedName(name string) string { return r.stored(r.sessionArgOf(name)) }

// windowName is the Lit a window name is set with: decoded, as tmux encodes
// window names again (NameArg).
func (r *restorer) windowName(name string) tmuxtarget.Lit {
	return tmuxtarget.Literal(tmuxtarget.NameArg(r.ver, name))
}

// titleArg is the Lit a pane title is set back with (TitleArg: verbatim
// where tmux stores titles verbatim, before 3.7).
func (r *restorer) titleArg(title string) tmuxtarget.Lit { return tmuxtarget.TitleArg(r.ver, title) }

func (r *restorer) warn(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	r.warnings = append(r.warnings, s)
	r.e.logf("%s", s)
}

// Restore runs one restore (§6.4). Only the daemon calls it in production
// (A3); tests call it on isolated servers. It returns after pane creation and
// the BOOT verdict; the LAUNCH verdict follows on RestoreResult.Launch, and
// save.lock is held until then.
func (e *Engine) Restore(ctx context.Context, req RestoreRequest) (res RestoreResult, err error) {
	if req.Kind == "" {
		req.Kind = KindRestore
	}
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return res, err
	}
	// 0. save.lock, held to RESTORE-END (§5.4): a bounded wait for an
	// in-flight save.
	e.step("lock")
	// The source a boot restore with no path would restore, read before the
	// wait (Last writes nothing): what a timed-out boot restore pins.
	bootSrc := ""
	if req.Kind == KindBoot && req.Path == "" {
		if last, lerr := st.Last(); lerr == nil {
			bootSrc = last.Name
		}
	}
	lctx, cancel := context.WithTimeout(ctx, e.restoreLockWait())
	l, err := st.LockWait(lctx, e.record())
	cancel()
	if err != nil {
		defer func() { _ = st.Close() }()
		if errors.Is(err, store.ErrBusy) {
			err = fmt.Errorf("%w: %v", ErrLockTimeout, err)
			if bootSrc != "" {
				e.pinAfterLock(ctx, st, bootSrc, err)
			}
		}
		return res, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			l.Release()
			_ = st.Close()
		}
	}()

	path := req.Path
	if path == "" && req.Kind == KindBoot {
		// D12: no valid moox snapshot yet → the boot import is published
		// first, under this lock, then restored from (§6.2, R2-H10).
		if _, lerr := st.Last(); lerr != nil && !errors.Is(lerr, ErrNewerFormat) {
			name, ierr := e.publishBootImport(ctx, st, l)
			switch {
			case ierr == nil:
				path = st.Path(name)
			case !errors.Is(ierr, errNoImport):
				e.logf("IMPORT-FAILED %s: %v", e.resurrectLast(), ierr)
			}
		}
	}
	src, err := e.loadSource(st, path)
	if err != nil {
		return res, err
	}
	res.Source = src.path
	if src.warning != "" {
		e.logf("%s", src.warning)
	}
	// A boot restore that fails from here on, before it creates anything,
	// pins its source: the saver resumes after it, and without a pin the
	// next save would supersede the workspace that never came back (G1-H3).
	pinIfBoot := func(why error) {
		if req.Kind == KindBoot {
			e.pinUnrestored(st, l, src, why)
		}
	}
	opts, err := e.readRestoreOptions(ctx)
	if err != nil {
		pinIfBoot(err)
		return res, fmt.Errorf("engine: restore read: %w", err)
	}
	restoring := opts.restoring
	if restoring == "" {
		restoring = fmt.Sprintf("%s:%d:%d", req.Kind, os.Getpid(), e.now().Unix())
		if _, err := e.tmux.Run(ctx, "set-option", "-g", "@moox-restoring", restoring); err != nil {
			pinIfBoot(err)
			return res, fmt.Errorf("engine: set @moox-restoring: %w", err)
		}
	}
	r := &restorer{e: e, ctx: ctx, opts: opts, manual: req.Kind != KindBoot, created: map[string]string{}, sessID: map[string]string{},
		ver: tmuxtarget.Version{Master: true}} // an unparsable #{version} reads as the newest tmux
	if v, err := tmuxtarget.ParseVersion(opts.version); err == nil {
		r.ver, r.is38 = v, v.AtLeast(3, 8)
	}
	r.sessArg = r.sessionArgs(src.snap)
	// 1. Bootstrap (T5, U3): the single pre-existing session, by its $id. A
	// populated server is refused before anything is written (B47).
	boot, err := r.bootstrap(src.snap)
	if err != nil {
		pinIfBoot(err)
		e.clearRestoring(restoring)
		return res, err
	}
	run, err := e.makeRunDir()
	if err != nil {
		pinIfBoot(err)
		e.clearRestoring(restoring)
		return res, err
	}
	r.runDir, r.contentsDir, r.ackDir = run.dir, run.contents, run.ack

	// 2. Plan (pure).
	ro := relaunch.Options{ClaudishCmd: opts.claudishCmd, ClaudeCmd: opts.claudeCmd, Processes: opts.processes,
		NvimSession: opts.strategyNvim != "off", Stub: req.Stub,
		SessionVim: func(cwd string) bool {
			_, err := os.Stat(filepath.Join(cwd, "Session.vim"))
			return err == nil
		}}
	banners := e.banners()
	plan, err := buildPlan(src.snap, planInputs{relaunch: ro,
		contents: func(id string) bool { _, ok := src.contents[id]; return ok },
		banner:   func(k string) bool { _, ok := banners[k]; return ok }})
	if err != nil {
		run.closeDirs()
		e.removeRunDir(run.name)
		pinIfBoot(err)
		e.clearRestoring(restoring)
		return res, err
	}
	// The attempt begins once nothing before pane creation can fail, so a
	// failed plan leaves no restoring-from behind (G1-M2).
	if src.name != "" {
		if err := st.BeginAttempt(l, src.name); err != nil {
			e.logf("PIN begin attempt %s: %v", src.name, err)
		}
	} else {
		e.logf("PIN-SKIPPED %s is outside the snapshot dir: the attempt is not pinned", src.path)
	}
	r.plan = plan
	res.Plan = plan
	for _, w := range plan.Warnings {
		e.logf("%s", w)
	}
	r.rowPane = make([]string, len(plan.Rows))
	r.winID = make([]string, len(plan.Windows))
	if err := r.writeContents(run.contentsD, src.contents, banners); err != nil {
		e.logf("CONTENTS-WRITE %v", err)
	}
	run.closeDirs()
	e.logf("RESTORE-START kind=%s source=%s rows=%d windows=%d sessions=%d dropped=%d stub=%v",
		req.Kind, src.path, len(plan.Rows), len(plan.Windows), len(plan.Sessions), len(plan.Dropped), req.Stub)

	// 3–6. Sessions in snapshot order, primaries before grouped members.
	for _, ps := range plan.Sessions {
		if ctx.Err() != nil {
			break
		}
		if ps.GroupedWith == "" {
			r.createSession(ps)
		}
	}
	for _, ps := range plan.Sessions {
		if ctx.Err() != nil {
			break
		}
		if ps.GroupedWith != "" {
			r.createGrouped(ps)
		}
	}
	// 7. Clients, 8. drop the bootstrap.
	r.clients(src.snap, boot)
	// 9. Tombstones, then the B43 scan.
	r.tombstones()
	r.staleIntents()

	// 10. BOOT verdict, then ledger seed and tab reconcile.
	res.Created = r.created
	bv := verdict.ComputeBoot(plan.bootRows(), r.created, r.orphans, r.warnings)
	res.Boot = bv
	e.logf("%s", bv.Line())
	_ = st.AppendLog("restore.log", e.now().Format(time.RFC3339)+" "+bv.Line())
	r.ledgerAndTabs()

	// 11. Results and the completion signals; @moox-restoring stays set.
	post := [][]string{{"set-option", "-g", "@moox-restore-result", bv.Result}}
	if req.Kind == KindBoot {
		post = append(post, []string{"set-option", "-g", "@moox-boot-result", bv.Result})
	}
	if bv.Result == verdict.Incomplete {
		post = append(post, []string{"set-option", "-g", "@claude-continuity-boot-warning",
			fmt.Sprintf("⚠ moox: restore INCOMPLETE — %d/%d resumed, %d missed — see %s", bv.Written, bv.Resumable,
				bv.Misses, filepath.Join(e.paths.Log, "restore.log"))})
		_ = st.AppendLog("restore-incomplete.log", fmt.Sprintf("[%s] INCOMPLETE written=%d resumable=%d misses=%d snapshot=%s",
			e.now().Format("2006-01-02 15:04:05"), bv.Written, bv.Resumable, bv.Misses, src.path))
	}
	post = append(post, []string{"wait-for", "-S", "moox-restored"})
	if req.Nonce != "" {
		post = append(post, []string{"wait-for", "-S", "moox-restored-" + req.Nonce})
	}
	if _, err := e.tmux.Run(ctx, tmuxtarget.Chain(post...)...); err != nil {
		e.logf("RESTORE-POST %v", err)
	}

	// 12. LAUNCH verdict in its own goroutine; it ends the restore.
	ch := make(chan LaunchVerdict, 1)
	res.Launch = ch
	handedOver = true
	rows := r.launchRows()
	go func() {
		defer close(ch)
		defer func() { _ = st.Close() }()
		defer l.Release()
		lv := e.launchVerdict(context.WithoutCancel(ctx), rows, r.ackDir)
		e.endRestore(st, l, src, bv, lv, restoring, run.name)
		ch <- lv
	}()
	return res, nil
}

func (e *Engine) restoreLockWait() time.Duration {
	if e.lockWait > 0 {
		return e.lockWait
	}
	return restoreLockWait
}

// endRestore is step 12 after the verdict: @moox-restoring off, the run dir
// removed, pins settled, @moox-warning set or cleared, RESTORE-END.
func (e *Engine) endRestore(st *store.Store, l *store.Lock, src source, bv BootVerdict, lv LaunchVerdict, restoring, run string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.logf("%s", lv.Line())
	_ = st.AppendLog("restore.log", e.now().Format(time.RFC3339)+" "+lv.Line())
	e.clearRestoring(restoring)
	e.removeRunDir(run)
	var cmds [][]string
	if lv.Result == verdict.Fail {
		cmds = append(cmds, []string{"set-option", "-g", "@claude-continuity-boot-warning",
			fmt.Sprintf("⚠ moox: LAUNCH FAIL — %d row(s) did not come back — see %s", lv.Failures,
				filepath.Join(e.paths.Log, "restore.log"))})
	}
	if src.name != "" {
		left, err := st.Settle(l, store.Attempt{Source: src.name, Boot: bv.Result, Misses: bv.Misses, Launch: lv.Result})
		if err != nil {
			e.logf("PIN settle: %v", err)
		} else {
			e.logf("PINS after restore of %s: %d pinned %v", src.name, len(left), left)
			if w := store.WarningText(left); w != "" {
				cmds = append(cmds, []string{"set-option", "-g", "@moox-warning", w})
			} else {
				cmds = append(cmds, []string{"set-option", "-gu", "@moox-warning"})
			}
		}
	}
	if len(cmds) > 0 {
		if _, err := e.tmux.Run(ctx, tmuxtarget.Chain(cmds...)...); err != nil {
			e.logf("RESTORE-END post: %v", err)
		}
	}
	e.logf("RESTORE-END")
}

// pinUnrestored pins the source of a boot restore that ended before it
// created anything (G1-H3) and re-sets @moox-warning, exactly as a non-PASS
// verdict would (A16): the snapshot is kept out of retention and the user is
// told, while saves resume on the small new workspace.
func (e *Engine) pinUnrestored(st *store.Store, l *store.Lock, src source, why error) {
	if src.name == "" {
		e.logf("PIN-SKIPPED %s is outside the snapshot dir: the failed boot restore is not pinned", src.path)
		return
	}
	if err := st.AddPin(l, src.name); err != nil {
		e.logf("PIN %s: %v", src.name, err)
		return
	}
	e.logf("PIN %s: the boot restore did not run (%v)", src.name, why)
	pins, err := st.Pins()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.tmux.Run(ctx, "set-option", "-g", "@moox-warning", store.WarningText(pins)); err != nil {
		e.logf("PIN warning: %v", err)
	}
}

// pinAfterLock is a boot restore's answer to its bounded save.lock wait
// timing out (G1-2-H2): it waits on, bounded only by ctx (the daemon
// stopping), and pins name under the lock. The daemon's saving stays held
// meanwhile, because Restore has not returned, and @moox-restoring keeps
// every other process's save refused; so no save can supersede the
// workspace that did not come back before it is pinned. Pins are only ever
// written under the lock.
func (e *Engine) pinAfterLock(ctx context.Context, st *store.Store, name string, why error) {
	e.logf("BOOT-LOCK-TIMEOUT %v: saving stays held until %s is pinned", why, name)
	l, err := st.LockWait(ctx, e.record())
	if err != nil {
		e.logf("PIN-SKIPPED %s: save.lock was never free (%v)", name, err)
		return
	}
	defer l.Release()
	e.pinUnrestored(st, l, source{name: name, path: st.Path(name)}, why)
}

// clearRestoring unsets @moox-restoring only if it still holds this
// restore's value (a value set meanwhile survives).
func (e *Engine) clearRestoring(value string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.tmux.Run(ctx, capture.ClearRestoringArgs(value)...); err != nil {
		e.logf("RESTORING-CLEAR %s: %v", value, err)
	}
}

// ---------------------------------------------------------------------------
// the run directory: restore/<run>/{contents,ack}/ (0700)
// ---------------------------------------------------------------------------

type runDirs struct {
	name                  string
	dir, contents, ack    string
	contentsD, ackD, runD *statedir.Dir
}

func (r runDirs) closeDirs() {
	for _, d := range []*statedir.Dir{r.contentsD, r.ackD, r.runD} {
		if d != nil {
			_ = d.Close()
		}
	}
}

func (e *Engine) makeRunDir() (runDirs, error) {
	state, err := statedir.Open(e.paths.State, e.claudeRoot)
	if err != nil {
		return runDirs{}, err
	}
	defer func() { _ = state.Close() }()
	rd, err := state.OpenSub("restore")
	if err != nil {
		return runDirs{}, err
	}
	defer func() { _ = rd.Close() }()
	name := fmt.Sprintf("%d-%s", e.now().Unix(), newNonce()[:6])
	run, err := rd.OpenSub(name)
	if err != nil {
		return runDirs{}, err
	}
	c, err := run.OpenSub("contents")
	if err != nil {
		_ = run.Close()
		return runDirs{}, err
	}
	a, err := run.OpenSub("ack")
	if err != nil {
		_ = run.Close()
		_ = c.Close()
		return runDirs{}, err
	}
	return runDirs{name: name, dir: run.Path(), contents: c.Path(), ack: a.Path(), contentsD: c, ackD: a, runD: run}, nil
}

func (e *Engine) removeRunDir(name string) {
	d, err := statedir.OpenExisting(filepath.Join(e.paths.State, "restore"), e.claudeRoot)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	if err := d.RemoveTree(name); err != nil {
		e.logf("RESTORE-SCRATCH remove %s: %v", name, err)
	}
}

func rowFile(i int) string { return "r" + strconv.Itoa(i) }

// banners reads the freeze store's <key>.banner files (B38): read-only.
func (e *Engine) banners() map[string][]byte {
	out := map[string][]byte{}
	d, err := statedir.OpenExisting(e.paths.FreezeDir, e.claudeRoot)
	if err != nil {
		return out
	}
	defer func() { _ = d.Close() }()
	names, err := d.ReadDirNames()
	if err != nil {
		return out
	}
	for _, n := range names {
		if k, ok := strings.CutSuffix(n, ".banner"); ok {
			if b, _, err := d.ReadFile(n); err == nil {
				out[k] = b
			}
		}
	}
	return out
}

// writeContents extracts each row's contents (or banner) into
// restore/<run>/contents/<row>; the hook prints and removes it (§4.4).
func (r *restorer) writeContents(d *statedir.Dir, entries map[string][]byte, banners map[string][]byte) error {
	var errs []error
	for i := range r.plan.Rows {
		row := &r.plan.Rows[i]
		if !row.Contents {
			continue
		}
		var b []byte
		if row.Pane.Role == snapshot.RoleFrozen {
			b = banners[row.Pane.FrozenKey]
		} else {
			b = entries[row.Pane.SavedID]
		}
		if err := d.WriteFile(rowFile(i), b, 0o600); err != nil {
			row.Contents = false
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// envFor is a row's -e list (C2).
func (r *restorer) envFor(i int) []tmuxtarget.EnvVar {
	row := r.plan.Rows[i]
	var env []tmuxtarget.EnvVar
	if row.Contents {
		env = append(env, tmuxtarget.EnvVar{Name: envContent, Value: filepath.Join(r.contentsDir, rowFile(i))})
	}
	if row.Resumes() {
		env = append(env, tmuxtarget.EnvVar{Name: envResume, Value: row.Cmd},
			tmuxtarget.EnvVar{Name: envAck, Value: filepath.Join(r.ackDir, rowFile(i))})
	}
	return env
}

// ---------------------------------------------------------------------------
// 1. bootstrap
// ---------------------------------------------------------------------------

type bootstrapSession struct {
	id, name string
	windows  int
}

// bootstrap finds the single pre-existing session; a server with more is
// populated (B47). A bootstrap named like a snapshot session is renamed
// moox-boot-<pid> first (E4).
func (r *restorer) bootstrap(snap *snapshot.Snapshot) (*bootstrapSession, error) {
	out, err := r.e.tmux.Run(r.ctx, "list-sessions", "-F", "#{session_id}"+optSep+"#{session_windows}"+optSep+"#{session_name}")
	if err != nil {
		if isNoSessions(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("engine: list-sessions: %w", err)
	}
	var found []bootstrapSession
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, optSep, 3)
		if len(f) != 3 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		found = append(found, bootstrapSession{id: f[0], windows: n, name: f[2]})
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, fmt.Errorf("%w (%d sessions)", ErrPopulated, len(found))
	}
	b := found[0]
	// B47 is a hard gate here too, under save.lock: ManualPreconditions ran
	// before the lock wait (up to 120 s), and a boot restore never ran them.
	// A bootstrap is one window with one pane; a single session the user
	// has already built on is populated, and is neither renamed (a renamed
	// session is filtered out of every later save) nor dropped (G1-H2).
	if b.windows > 1 {
		return nil, fmt.Errorf("%w (session %s has %d windows)", ErrPopulated, b.id, b.windows)
	}
	panes, err := r.e.tmux.Run(r.ctx, "list-panes", "-s", "-t", b.id, "-F", "#{pane_id}"+optSep+"#{pane_pid}"+optSep+"#{pane_current_command}")
	if err != nil {
		return nil, fmt.Errorf("engine: list-panes %s: %w", b.id, err)
	}
	lines := nonEmptyLines(string(panes))
	if n := len(lines); n > 1 {
		return nil, fmt.Errorf("%w (session %s has %d panes)", ErrPopulated, b.id, n)
	}
	// A manual restore checks the pane again, under the lock: a job started
	// in it during the lock wait would otherwise be renamed and later killed
	// with the bootstrap (G1-2-H1). A boot restore does not: the login shell
	// may still be in its rc files, and nothing ran in it yet.
	if r.manual && len(lines) == 1 {
		f := strings.SplitN(lines[0], optSep, 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("%w (session %s: unreadable pane %q)", ErrPopulated, b.id, lines[0])
		}
		pid, _ := strconv.Atoi(f[1])
		if err := r.e.idleShell(r.ctx, pid, f[2]); err != nil {
			return nil, err
		}
	}
	for _, se := range snap.Sessions {
		if r.storedName(se.Name) == b.name {
			newName := moxBootPrefix + strconv.Itoa(os.Getpid())
			if _, err := r.e.tmux.Run(r.ctx, tmuxtarget.RenameSession(b.id, tmuxtarget.Literal(newName))...); err != nil {
				return nil, fmt.Errorf("engine: rename the bootstrap %s: %w", b.id, err)
			}
			r.e.logf("BOOTSTRAP %s %q renamed %s (a snapshot session has its name)", b.id, b.name, newName)
			b.name = newName
			break
		}
	}
	return &b, nil
}

func isNoSessions(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no server running") || strings.Contains(s, "no sessions") ||
		strings.Contains(s, "error connecting to")
}

// ---------------------------------------------------------------------------
// 3. one primary session: exec A, A′, move, B, C, D, then layout and
// decoration (4, 5)
// ---------------------------------------------------------------------------

func (r *restorer) createSession(ps PlanSession) {
	e := r.e
	first := r.plan.Windows[ps.Windows[0]]
	firstRow := first.Rows[0]
	row := r.plan.Rows[firstRow]
	// exec A: the session's first window and first pane. Even one command
	// goes through Chain: tmux reads a trailing `;` in ANY argv element as a
	// separator (a session named "semi;" fails otherwise, E5).
	args := tmuxtarget.Chain(tmuxtarget.NewSession{Name: r.sessionName(ps.Name), Cwd: tmuxtarget.Literal(row.Pane.Cwd),
		Width: ps.Width, Height: ps.Height, Env: r.envFor(firstRow),
		PrintFormat: "#{session_id} #{window_id} #{pane_id}"}.Args())
	var sid, wid, pid string
	for attempt := 0; attempt < 2 && sid == ""; attempt++ {
		out, err := e.tmux.Run(r.ctx, args...)
		if f := strings.Fields(string(out)); len(f) == 3 {
			sid, wid, pid = f[0], f[1], f[2]
		} else {
			e.logf("CREATE-FAILED session %q (attempt %d): %v", ps.Name, attempt+1, err)
		}
	}
	if sid == "" {
		return // every row of the session is a miss (plan minus Created)
	}
	// exec A′, before anything else is created in S: clear the SESSION env
	// that -e wrote (E1, Q3), by $sid. Fail closed (R2-H1).
	cleared := false
	for attempt := 0; attempt < 2 && !cleared; attempt++ {
		_, err := e.tmux.Run(r.ctx, tmuxtarget.UnsetSessionEnv(sid, envResume, envContent, envAck)...)
		if err == nil {
			cleared = true
		} else {
			e.logf("ENV-CLEAR-FAILED %s %q (attempt %d): %v", sid, ps.Name, attempt+1, err)
		}
	}
	if !cleared {
		if _, err := e.tmux.Run(r.ctx, "kill-session", "-t", sid); err != nil {
			e.logf("ENV-CLEAR kill-session %s: %v", sid, err)
		}
		e.logf("FAIL-CLOSED session %q killed: its env could not be cleared; every row is a miss", ps.Name)
		return
	}
	r.sessID[ps.Name] = sid
	r.winID[ps.Windows[0]] = wid
	r.setPane(firstRow, pid)
	if first.Index != r.baseIndex() {
		if _, err := e.tmux.Run(r.ctx, "move-window", "-s", wid, "-t", sid+":"+strconv.Itoa(first.Index)); err != nil {
			r.warn("WINDOW-INDEX %s:%d kept the base index: %v", ps.Name, first.Index, err)
		}
	}

	// exec B: the session's other windows, one chain.
	var steps []step
	for _, wi := range ps.Windows[1:] {
		w := r.plan.Windows[wi]
		ri := w.Rows[0]
		wi := wi
		steps = append(steps, step{
			cmd: tmuxtarget.NewWindow{Target: sid + ":" + strconv.Itoa(w.Index), Name: r.windowName(w.Name),
				Cwd: tmuxtarget.Literal(r.plan.Rows[ri].Pane.Cwd), Env: r.envFor(ri),
				PrintFormat: "#{window_id} #{pane_id}"}.Args(),
			done: func(line string) bool {
				f := strings.Fields(line)
				if len(f) != 2 {
					return false
				}
				r.winID[wi] = f[0]
				r.setPane(ri, f[1])
				return true
			},
			what: fmt.Sprintf("window %s:%d", ps.Name, w.Index),
		})
	}
	r.runSteps(steps)

	// exec C: the extra panes of every window, in row order.
	steps = steps[:0]
	for _, wi := range ps.Windows {
		w := r.plan.Windows[wi]
		wid := r.winID[wi]
		if wid == "" || len(w.Rows) < 2 {
			continue
		}
		for _, ri := range w.Rows[1:] {
			ri := ri
			tiled := []string{"select-layout", "-t", wid, "tiled"}
			steps = append(steps, step{
				cmd: tmuxtarget.SplitWindow{Target: wid, Follow: true, Cwd: tmuxtarget.Literal(r.plan.Rows[ri].Pane.Cwd),
					Env: r.envFor(ri), PrintFormat: "#{pane_id}"}.Args(),
				after: [][]string{tiled},
				fix:   tiled,
				done: func(line string) bool {
					if !strings.HasPrefix(line, "%") {
						return false
					}
					r.setPane(ri, strings.TrimSpace(line))
					return true
				},
				what: "pane " + r.plan.Rows[ri].Key,
			})
		}
	}
	r.runSteps(steps)

	// exec D: decoration, with the ids A–C printed (R2-H6, B39, PR-H12).
	var deco [][]string
	for _, wi := range ps.Windows {
		for _, ri := range r.plan.Windows[wi].Rows {
			pid := r.rowPane[ri]
			if pid == "" {
				continue
			}
			p := r.plan.Rows[ri].Pane
			if p.Role == snapshot.RoleFrozen {
				deco = append(deco, []string{"set-option", "-p", "-t", pid, "allow-set-title", "off"},
					[]string{"set-option", "-p", "-t", pid, "allow-rename", "off"})
			}
			deco = append(deco, tmuxtarget.SelectPaneTitle(pid, r.titleArg(p.Title))) // I-4
		}
	}
	r.runBestEffort("DECORATION", deco)

	// 4. per window: the saved layout (gives the saved size, R3-C1), then
	// name, automatic-rename, active pane, zoom.
	var wdeco, zoom [][]string // zoom: one resize-pane -Z per zoomed window
	for _, wi := range ps.Windows {
		wid := r.winID[wi]
		if wid == "" {
			continue
		}
		w := r.plan.Windows[wi]
		r.applyLayout(w, wid)
		wdeco = append(wdeco, tmuxtarget.RenameWindow(wid, r.windowName(w.Name)))
		switch w.AutomaticRename {
		case snapshot.AutoRenameOn:
			wdeco = append(wdeco, []string{"set-option", "-w", "-t", wid, "automatic-rename", "on"})
		case snapshot.AutoRenameOff:
			wdeco = append(wdeco, []string{"set-option", "-w", "-t", wid, "automatic-rename", "off"})
		default:
			wdeco = append(wdeco, []string{"set-option", "-wu", "-t", wid, "automatic-rename"})
		}
		if w.ActiveRow >= 0 && r.rowPane[w.ActiveRow] != "" {
			wdeco = append(wdeco, []string{"select-pane", "-t", r.rowPane[w.ActiveRow]})
			if w.Zoomed {
				zoom = append(zoom, []string{"resize-pane", "-Z", "-t", r.rowPane[w.ActiveRow]})
			}
		}
	}
	r.runBestEffort("WINDOW-DECORATION", wdeco)
	// Zoom after select-pane (select-pane unzooms, PR-H12), one exec each: a
	// toggle is never re-run.
	for _, z := range zoom {
		if _, err := e.tmux.Run(r.ctx, z...); err != nil {
			e.logf("ZOOM %s: %v", z[len(z)-1], err)
		}
	}

	// 5. per session: last window, then active (T11); default-size unset.
	var sdeco [][]string
	if w := r.windowByIndex(ps, ps.LastWindow); w != "" {
		sdeco = append(sdeco, []string{"select-window", "-t", w})
	}
	if w := r.windowByIndex(ps, ps.ActiveWindow); w != "" {
		sdeco = append(sdeco, []string{"select-window", "-t", w})
	}
	sdeco = append(sdeco, []string{"set-option", "-u", "-t", sid, "default-size"})
	r.runBestEffort("SESSION-DECORATION", sdeco)
}

func (r *restorer) baseIndex() int {
	n, err := strconv.Atoi(strings.TrimSpace(r.opts.baseIndex))
	if err != nil {
		return 0
	}
	return n
}

func (r *restorer) windowByIndex(ps PlanSession, idx int) string {
	if idx < 0 {
		return ""
	}
	for _, wi := range ps.Windows {
		if r.plan.Windows[wi].Index == idx {
			return r.winID[wi]
		}
	}
	return ""
}

func (r *restorer) setPane(ri int, pid string) {
	r.rowPane[ri] = pid
	r.created[r.plan.Rows[ri].Key] = pid
}

// applyLayout: select-layout with the saved layout in this server's
// encoding (v2 for 3.8+, v1 otherwise), one retry in the other encoding, then
// tiled with LAYOUT-FALLBACK (RR13). Never resize-window (R3-C1).
func (r *restorer) applyLayout(w PlanWindow, wid string) {
	root, format, err := layout.Parse(w.Layout)
	var tries []string
	if err == nil {
		first, second := layout.V1, layout.V2
		if r.is38 {
			first, second = layout.V2, layout.V1
		}
		if format == first {
			tries = append(tries, w.Layout)
		} else {
			tries = append(tries, root.Encode(first))
		}
		if r.is38 || second == layout.V1 {
			alt := root.Encode(second)
			if second == format {
				alt = w.Layout
			}
			tries = append(tries, alt)
		}
	} else {
		tries = append(tries, w.Layout)
	}
	var last error
	for _, lay := range tries {
		if _, err := r.e.tmux.Run(r.ctx, "select-layout", "-t", wid, lay); err == nil {
			return
		} else {
			last = err
		}
	}
	if _, err := r.e.tmux.Run(r.ctx, "select-layout", "-t", wid, "tiled"); err != nil {
		r.e.logf("LAYOUT tiled %s: %v", wid, err)
	}
	r.warn("LAYOUT-FALLBACK %s:%d (%s) tiled: %v", w.Session, w.Index, wid, last)
}

// ---------------------------------------------------------------------------
// 6. grouped sessions
// ---------------------------------------------------------------------------

func (r *restorer) createGrouped(ps PlanSession) {
	primary := r.sessID[ps.GroupedWith]
	if primary == "" {
		r.warn("GROUPED-SKIPPED %q: its primary %q was not created", ps.Name, ps.GroupedWith)
		return
	}
	out, err := r.e.tmux.Run(r.ctx, tmuxtarget.Chain(tmuxtarget.NewSession{Name: r.sessionName(ps.Name), Group: primary,
		PrintFormat: "#{session_id}"}.Args())...)
	gid := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(gid, "$") {
		r.warn("GROUPED-FAILED %q: %v", ps.Name, err)
		return
	}
	r.sessID[ps.Name] = gid
	var cmds [][]string
	for _, idx := range []int{ps.LastWindow, ps.ActiveWindow} {
		if idx >= 0 {
			cmds = append(cmds, []string{"select-window", "-t", gid + ":" + strconv.Itoa(idx)})
		}
	}
	r.runBestEffort("GROUPED-DECORATION", cmds)
}

// ---------------------------------------------------------------------------
// 7. clients, 8. the bootstrap drop
// ---------------------------------------------------------------------------

func (r *restorer) clients(snap *snapshot.Snapshot, boot *bootstrapSession) {
	e := r.e
	target := ""
	last := ""
	if snap.Client != nil {
		target = r.sessID[snap.Client.Session]
		last = r.sessID[snap.Client.LastSession]
	}
	if target == "" {
		for _, ps := range r.plan.Sessions {
			if id := r.sessID[ps.Name]; id != "" {
				target = id
				break
			}
		}
	}
	out, _ := e.tmux.Run(r.ctx, "list-clients", "-F", "#{client_name}")
	var names []string
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if target != "" {
		var cmds [][]string
		for _, c := range names {
			if last != "" && last != target {
				cmds = append(cmds, tmuxtarget.SwitchClient(c, last))
			}
			cmds = append(cmds, tmuxtarget.SwitchClient(c, target))
		}
		if len(names) == 0 {
			cmds = append(cmds, []string{"set-option", "-g", "@moox-restore-target", target})
		}
		r.runBestEffort("CLIENTS", cmds)
	}

	// 8. Drop the bootstrap (T5, U3): not in the snapshot (always true once
	// renamed), one window with one pane, and at least one restored session.
	// No "no child processes" test here (R2-M2).
	if boot == nil || len(r.sessID) == 0 {
		return
	}
	for _, se := range snap.Sessions {
		if se.Name == boot.name {
			e.logf("BOOTSTRAP %s kept: a snapshot session has its name", boot.id)
			return
		}
	}
	panes, err := e.tmux.Run(r.ctx, "list-panes", "-s", "-t", boot.id, "-F", "#{pane_id}")
	if err != nil {
		e.logf("BOOTSTRAP %s: list-panes: %v", boot.id, err)
		return
	}
	if n := len(strings.Fields(string(panes))); boot.windows != 1 || n != 1 {
		e.logf("BOOTSTRAP %s kept: %d windows, %d panes", boot.id, boot.windows, n)
		return
	}
	var cmds [][]string
	if bc, err := e.tmux.Run(r.ctx, "list-clients", "-t", boot.id, "-F", "#{client_name}"); err == nil && target != "" {
		for _, c := range strings.Fields(string(bc)) {
			cmds = append(cmds, tmuxtarget.SwitchClient(c, target))
		}
	}
	cmds = append(cmds, []string{"kill-session", "-t", boot.id})
	if _, err := e.tmux.Run(r.ctx, tmuxtarget.Chain(cmds...)...); err != nil {
		e.logf("BOOTSTRAP %s drop: %v", boot.id, err)
		return
	}
	e.logf("BOOTSTRAP %s %q dropped", boot.id, boot.name)
}

// ---------------------------------------------------------------------------
// 9. tombstones (B38, R31), the B43 scan, 10. ledger seed and tab reconcile
// ---------------------------------------------------------------------------

func (r *restorer) script(name string) string {
	if r.opts.root == "" {
		return ""
	}
	return filepath.Join(r.opts.root, "plugins", "tmux-claude-continuity", "scripts", name)
}

func (r *restorer) tombstones() {
	if len(r.plan.Tombstones) == 0 {
		return
	}
	freeze := r.script("cc_freeze.sh")
	for _, ri := range r.plan.Tombstones {
		row := r.plan.Rows[ri]
		pid := r.rowPane[ri]
		if pid == "" {
			continue // already a miss: no pane
		}
		key := row.Pane.FrozenKey
		if key == "" || freeze == "" {
			r.warn("FROZEN-RECLAIM-SKIPPED %s (%s): key %q, script %q", row.Key, pid, key, freeze)
			continue
		}
		wid := r.winID[row.Window]
		ctx, cancel := context.WithTimeout(r.ctx, scriptTimeout)
		out, err := r.e.scripts.Run(ctx, r.e.scriptEnv(), freeze, "reclaim", key, pid, wid)
		cancel()
		if err != nil {
			r.e.logf("FROZEN-RECLAIM %s: %v", row.Key, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "FROZEN-ORPHAN"):
				r.orphans = append(r.orphans, row.Key+" "+line)
				r.e.logf("%s", line)
			case strings.HasPrefix(line, "FROZEN-CLAIM-FAILED"), strings.HasPrefix(line, "FROZEN-FOREIGN"),
				strings.HasPrefix(line, "FROZEN-UNREADABLE"):
				r.warn("%s", line)
			case line != "":
				r.e.logf("%s", line)
			}
		}
	}
}

// staleIntents is the B43 scan: freeze-store entries whose tombstone is not
// in the snapshot are logged; the store is read-only and nothing is refrozen.
func (r *restorer) staleIntents() {
	d, err := statedir.OpenExisting(r.e.paths.FreezeDir, r.e.claudeRoot)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	names, err := d.ReadDirNames()
	if err != nil {
		return
	}
	planned := map[string]bool{}
	for _, ri := range r.plan.Tombstones {
		planned[r.plan.Rows[ri].Pane.FrozenKey] = true
	}
	for _, n := range names {
		if k, ok := strings.CutSuffix(n, ".state"); ok && !planned[k] {
			r.e.logf("FROZEN-STALE-INTENT key=%s: in the freeze store but not in the snapshot — left untouched", k)
		}
	}
}

func (r *restorer) ledgerAndTabs() {
	if _, err := os.Stat(r.e.paths.FreezeDir); err == nil {
		if s := r.script("cc_freeze.sh"); s != "" {
			ctx, cancel := context.WithTimeout(r.ctx, scriptTimeout)
			if _, err := r.e.scripts.Run(ctx, r.e.scriptEnv(), s, "ledger-seed"); err != nil {
				r.e.logf("LEDGER-SEED %v", err)
			}
			cancel()
		}
	}
	if s := r.script("cc_tab_reconcile.sh"); s != "" {
		ctx, cancel := context.WithTimeout(r.ctx, scriptTimeout)
		if _, err := r.e.scripts.Run(ctx, r.e.scriptEnv(), s, "--all"); err != nil {
			r.e.logf("TAB-RECONCILE %v", err)
		} else {
			r.e.logf("TAB-RECONCILE --all")
		}
		cancel()
	}
}

// launchRows are the rows the LAUNCH verdict judges: created with
// MOOX_RESUME. layout, skip and frozen-banner rows never are (§6.5).
func (r *restorer) launchRows() []launchRow {
	var rows []launchRow
	for i, row := range r.plan.Rows {
		if !row.Resumes() || r.rowPane[i] == "" {
			continue
		}
		rows = append(rows, launchRow{LaunchRow: verdict.LaunchRow{Key: row.Key, Pane: r.rowPane[i], Claude: row.Claude,
			Token: launchToken(row.Original)}, ack: rowFile(i)})
	}
	return rows
}

// ---------------------------------------------------------------------------
// chains with resume-after-failure (PR-H3, E5)
// ---------------------------------------------------------------------------

// step is one creating command that prints exactly one line (-P -F). after
// runs right behind it in the same chain and prints nothing; fix runs once
// before the step's single retry.
type step struct {
	cmd   []string
	after [][]string
	fix   []string
	done  func(line string) bool
	what  string
}

func argvLen(cmds [][]string) int {
	n := 0
	for _, c := range cmds {
		for _, a := range c {
			n += len(a) + 1
		}
	}
	return n
}

// runSteps runs steps as chains under chainLimit. tmux stops a chain at the
// first failing command and the printed lines say how far it got: the run
// resumes with the first step that printed nothing, never re-running one that
// succeeded (so the row → pane map stays aligned, A8). A step that fails twice
// is skipped (its rows are misses) and the run continues.
func (r *restorer) runSteps(steps []step) {
	i := 0
	for i < len(steps) && r.ctx.Err() == nil {
		var cmds [][]string
		j := i
		for j < len(steps) {
			c := append([][]string{steps[j].cmd}, steps[j].after...)
			if j > i && argvLen(cmds)+argvLen(c) > chainLimit {
				break
			}
			cmds = append(cmds, c...)
			j++
		}
		out, err := r.e.tmux.Run(r.ctx, tmuxtarget.Chain(cmds...)...)
		lines := nonEmptyLines(string(out))
		n := 0
		for n < len(lines) && i+n < j && steps[i+n].done(lines[n]) {
			n++
		}
		if i+n == j {
			if err != nil {
				r.e.logf("CHAIN after %s: %v", steps[j-1].what, err)
			}
			i = j
			continue
		}
		f := i + n
		if steps[f].fix != nil {
			_, _ = r.e.tmux.Run(r.ctx, steps[f].fix...)
		}
		out, err2 := r.e.tmux.Run(r.ctx, tmuxtarget.Chain(append([][]string{steps[f].cmd}, steps[f].after...)...)...)
		if l := nonEmptyLines(string(out)); len(l) == 0 || !steps[f].done(l[0]) {
			r.e.logf("CREATE-FAILED %s twice: %v / %v — a miss", steps[f].what, err, err2)
		}
		i = f + 1
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// runBestEffort runs commands that print nothing and are idempotent, as
// chains; a chain that fails is re-run one command at a time so one bad
// command cannot skip the others.
func (r *restorer) runBestEffort(what string, cmds [][]string) {
	for len(cmds) > 0 {
		j := 1
		for j < len(cmds) && argvLen(cmds[:j+1]) <= chainLimit {
			j++
		}
		chunk := cmds[:j]
		cmds = cmds[j:]
		if _, err := r.e.tmux.Run(r.ctx, tmuxtarget.Chain(chunk...)...); err == nil {
			continue
		}
		for _, c := range chunk {
			if _, err := r.e.tmux.Run(r.ctx, tmuxtarget.Chain(c)...); err != nil {
				r.e.logf("%s %s: %v", what, strings.Join(c, " "), err)
			}
		}
	}
}
