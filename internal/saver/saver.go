// Package saver is moox's event-driven save loop (architecture §5), hosted by
// `tt serve --foreground`: four `wait-for` waiters (moox-dirty, moox-touch,
// moox-save, moox-restore-req) feed ONE loop goroutine, which is the only
// caller of Engine.Save and Engine.Restore in the process (B22, A3). The
// loop debounces dirty events into one save (clamped by a hard ceiling),
// keeps a backup chain alive only while a client is attached, answers every
// save and restore request on its own channel, suppresses saving from the
// moment a restore is decided until its LAUNCH verdict, and backs off when
// another process holds save.lock.
//
// Timers (tt §9.3a, audit.go:663-665): "one-shot, armed only by an event or
// once at start, self-disarming, and never re-armed while idle". Every arm
// below is a Clock.NewTimer created in response to an event, and each comment
// names the clause it satisfies. An idle server — no client, no hook event —
// holds no armed timer (TestSaver_IdleHasNoArmedTimer).
package saver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// The daemon's channels (§12.4).
const (
	ChanDirty      = "moox-dirty"
	ChanTouch      = "moox-touch"
	ChanSave       = "moox-save"
	ChanRestoreReq = "moox-restore-req"
)

// Defaults of the three timing options (§12.4).
const (
	DefaultDebounce = 3 * time.Second
	DefaultMaxWait  = 30 * time.Second
	DefaultBackup   = 15 * time.Minute
)

// Bounds.
const (
	saveTimeout      = 120 * time.Second // every save runs under it (§5.6 (4))
	heartbeatTimeout = 60 * time.Second  // §5.7
	busyMaxTries     = 5                 // 3, 6, 12, 24, 48 s, then BUSY-GAVE-UP
	bootstrapPoll    = 100 * time.Millisecond
	bootstrapWithin  = 30 * time.Second
)

// Engine is the part of *engine.Engine the loop uses (a consumer-side port;
// the unit tests fake it).
type Engine interface {
	Save(ctx context.Context, r engine.SaveRequest) (engine.SaveResult, error)
	Restore(ctx context.Context, r engine.RestoreRequest) (engine.RestoreResult, error)
	ManualPreconditions(ctx context.Context) error
	WaitBootstrap(ctx context.Context, interval, within time.Duration) bool
	SkipBoot(ctx context.Context, reason, restoring string)
	Heartbeat(ctx context.Context) error
}

// Runner runs one tmux exec (one command or one Chain).
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Boot asks the loop to boot-restore first (§6.4: the daemon the cold gate
// forked with --boot). The loop starts with restoring=true.
type Boot struct {
	// Stub: MOOX_STUB_COMMANDS=1 in the env that started the server.
	Stub bool
}

// Config configures Run.
type Config struct {
	Target tmuxtarget.Target
	Engine Engine
	// Tmux runs the loop's own tmux execs (request reads, acks, status);
	// nil means Target.
	Tmux Runner
	// Clock: nil means SystemClock.
	Clock Clock
	// Debounce, MaxWait, Backup: @moox-save-debounce (3s), -max-wait (30s),
	// -backup (15m; 0 disables). See ReadTiming.
	Debounce, MaxWait, Backup time.Duration
	Log                       io.Writer
	// Boot: non-nil → boot-restore first.
	Boot *Boot
	// OnState is told every state change ("idle", "dirty", "saving",
	// "restoring"); `tt serve` writes it to saver.state. nil ignores.
	OnState func(state string)
	// Alive answers a requester pid's liveness; nil means kill(pid, 0).
	Alive func(pid int) bool
	// NewWatcher builds the waiter of one channel; nil means a real
	// Watcher on Target. Tests feed events through it.
	NewWatcher func(channel string, signal func()) func(ctx context.Context) error
}

// Run is the daemon's saver. It returns when ctx ends (nil), or when a
// waiter reports the server gone (ErrServerGone) or gave up (ErrGaveUp).
// A boot restore or a requested one that is still running when Run returns
// keeps going until ctx ends; its LAUNCH verdict is not waited for.
func Run(ctx context.Context, c Config) error {
	m := newMachine(ctx, c)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := map[string]chan struct{}{}
	for _, ch := range []string{ChanDirty, ChanTouch, ChanSave, ChanRestoreReq} {
		events[ch] = make(chan struct{}, 1)
	}
	done := make(chan error, 4)
	var wg sync.WaitGroup
	for name, ev := range events {
		ev := ev
		signal := func() {
			select { // coalescing: the loop reads every pending request at once
			case ev <- struct{}{}:
			default:
			}
		}
		run := m.watcher(name, signal)
		wg.Add(1)
		go func() { defer wg.Done(); done <- run(ctx) }()
	}
	defer wg.Wait()

	if c.Boot != nil {
		m.startBoot(*c.Boot)
	}
	m.setState()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			cancel()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-events[ChanDirty]:
			m.onDirty()
		case <-events[ChanTouch]:
			m.onTouch()
		case <-events[ChanSave]:
			m.onSaveRequest()
		case <-events[ChanRestoreReq]:
			m.onRestoreRequest()
		case <-m.timerC(m.debounce):
			m.onDebounce()
		case <-m.timerC(m.backup):
			m.onBackup()
		case end := <-m.restoreDone:
			m.onRestoreEnd(end)
		}
		m.setState()
	}
}

// machine is the loop's state (§5.3). Every field is owned by the loop
// goroutine; the restore goroutine talks to it only through restoreDone.
type machine struct {
	ctx   context.Context
	c     Config
	clk   Clock
	tmux  Runner
	alive func(int) bool
	log   io.Writer

	dirty      bool
	firstDirty time.Time
	debounce   Timer
	backup     Timer
	busyTries  int
	atCeiling  bool // the armed debounce was clamped to the ceiling
	restoring  bool
	snapshotFP string

	restoreDone chan restoreEnd
	heartbeat   atomic.Bool // a heartbeat is running (§5.7)
	hbWG        sync.WaitGroup
	lastState   string
	armed       int // timers armed and not yet fired or stopped (the idle oracle)
}

type restoreEnd struct {
	kind   string
	result string // LAUNCH verdict result, or why it did not run
}

func newMachine(ctx context.Context, c Config) *machine {
	m := &machine{ctx: ctx, c: c, clk: c.Clock, tmux: c.Tmux, alive: c.Alive, log: c.Log,
		restoreDone: make(chan restoreEnd, 1)}
	if m.clk == nil {
		m.clk = SystemClock{}
	}
	if m.tmux == nil {
		m.tmux = c.Target
	}
	if m.alive == nil {
		m.alive = pidAlive
	}
	if m.log == nil {
		m.log = io.Discard
	}
	return m
}

func (m *machine) logf(format string, a ...any) {
	fmt.Fprintf(m.log, "%s pid=%d %s\n", m.clk.Now().UTC().Format(time.RFC3339), os.Getpid(), fmt.Sprintf(format, a...))
}

func (m *machine) watcher(name string, signal func()) func(context.Context) error {
	if m.c.NewWatcher != nil {
		return m.c.NewWatcher(name, signal)
	}
	return Watcher{Target: m.c.Target, Channel: name, Logf: m.logf, Signal: signal}.Run
}

// ---------------------------------------------------------------------------
// timers
// ---------------------------------------------------------------------------

func (m *machine) timerC(t Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C()
}

func (m *machine) arm(d time.Duration) Timer {
	m.armed++
	return m.clk.NewTimer(d)
}

func (m *machine) stop(t *Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
		m.armed--
	}
}

// fired clears a timer that just fired (self-disarming).
func (m *machine) fired(t *Timer) {
	*t = nil
	m.armed--
}

// armedTimers is the idle oracle of TestSaver_IdleHasNoArmedTimer.
func (m *machine) armedTimers() int { return m.armed }

// ---------------------------------------------------------------------------
// events (§5.3)
// ---------------------------------------------------------------------------

func (m *machine) onDirty() {
	if m.restoring {
		m.dirty = true // suppressed (B29, D10): no timer
		return
	}
	now := m.clk.Now()
	if !m.dirty {
		m.dirty, m.firstDirty = true, now
	}
	left := m.c.MaxWait - now.Sub(m.firstDirty)
	if left <= 0 {
		m.stop(&m.debounce)
		m.save("max-wait", false, false)
		return
	}
	d := m.c.Debounce
	m.atCeiling = false
	if left < d {
		d, m.atCeiling = left, true // clamped: the ceiling is hard (R2-M8)
	}
	m.stop(&m.debounce)
	// One-shot, armed by this dirty event, self-disarming (§9.3a).
	m.debounce = m.arm(d)
}

func (m *machine) onTouch() {
	if m.restoring || m.backup != nil || m.c.Backup <= 0 {
		return // never extended, never re-armed while armed
	}
	// One-shot, armed by this touch event (§9.3a).
	m.backup = m.arm(m.c.Backup)
}

func (m *machine) onDebounce() {
	m.fired(&m.debounce)
	reason := "debounce"
	if m.atCeiling {
		reason = "max-wait" // the clamped timer fired AT the ceiling
	}
	m.atCeiling = false
	m.save(reason, false, false)
}

func (m *machine) onBackup() {
	m.fired(&m.backup)
	res, _ := m.save("backup", true, false)
	if res.AttachedClients > 0 && m.c.Backup > 0 && !m.restoring {
		// Re-armed by its OWN firing, and only while a client is attached:
		// a detached server is idle and gets no timer (R2-C4, §9.3a).
		m.backup = m.arm(m.c.Backup)
	}
}

// save runs one Engine.Save and applies its result to the state (§5.3
// save(r)); the result string is what a request is answered with (§5.5).
func (m *machine) save(reason string, contents, force bool) (engine.SaveResult, string) {
	m.setStateTo("saving")
	ctx, cancel := context.WithTimeout(m.ctx, saveTimeout)
	res, err := m.c.Engine.Save(ctx, engine.SaveRequest{Reason: reason, Contents: contents, Force: force})
	cancel()
	result := ResultOf(res, err)
	m.logf("SAVE reason=%s contents=%v force=%v -> %s (%d execs, %s)", reason, contents, force, result, res.Execs,
		res.Took.Round(time.Millisecond))
	var busy *engine.BusyError
	switch {
	case err == nil:
		m.dirty, m.busyTries = false, 0
		if res.SnapshotFP != "" {
			m.snapshotFP = res.SnapshotFP
		}
		if res.Published {
			m.kickHeartbeat()
		}
	case errors.Is(err, engine.ErrRestoring), errors.Is(err, engine.ErrShuttingDown):
		// Another process owns the server; it ends by signalling moox-dirty.
		m.dirty = true
	case errors.As(err, &busy), errors.Is(err, engine.ErrBusy):
		m.dirty = true
		m.busyTries++
		if m.busyTries <= busyMaxTries {
			m.stop(&m.debounce)
			m.atCeiling = false
			// One-shot, armed by the busy RESULT event (§9.3a): 3, 6, 12,
			// 24, 48 s.
			m.debounce = m.arm(m.c.Debounce << (m.busyTries - 1))
		} else {
			pid := 0
			if busy != nil {
				pid = busy.Pid
			}
			m.logf("BUSY-GAVE-UP after %d busy saves (lock pid %d); the next dirty event restarts", m.busyTries-1, pid)
			m.status(fmt.Sprintf("save busy (lock pid %d)", pid))
		}
	case errors.Is(err, engine.ErrBlocked):
		m.dirty = false // the engine already set @moox_status "save blocked"
	case errors.Is(err, engine.ErrNotActive):
		m.dirty = false // being deactivated (§3.3)
	default:
		// invalid, error: nothing is retried by a timer; the next event does.
		m.dirty = false
	}
	return res, result
}

func (m *machine) status(s string) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	if _, err := m.tmux.Run(ctx, "set-option", "-g", "@moox_status", s); err != nil {
		m.logf("STATUS %q: %v", s, err)
	}
}

// kickHeartbeat starts cc_freeze.sh heartbeat after a published save (§5.7),
// in a goroutine with a 60 s deadline. It never delays the next save; one
// still running makes this save skip it.
func (m *machine) kickHeartbeat() {
	if !m.heartbeat.CompareAndSwap(false, true) {
		m.logf("HEARTBEAT-SKIPPED the previous one is still running")
		return
	}
	m.hbWG.Add(1)
	go func() {
		defer m.hbWG.Done()
		defer m.heartbeat.Store(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), heartbeatTimeout)
		defer cancel()
		if err := m.c.Engine.Heartbeat(ctx); err != nil {
			m.logf("HEARTBEAT %v", err)
		}
	}()
}

// ResultOf is the §5.5 result word of one save.
func ResultOf(res engine.SaveResult, err error) string {
	var busy *engine.BusyError
	switch {
	case err == nil && res.Published:
		return "published " + res.File
	case err == nil && res.Pristine:
		return "pristine"
	case err == nil:
		return "unchanged"
	case errors.Is(err, engine.ErrShuttingDown):
		return "shutting-down"
	case errors.Is(err, engine.ErrRestoring):
		return "restoring"
	case errors.As(err, &busy):
		return "busy " + strconv.Itoa(busy.Pid)
	case errors.Is(err, engine.ErrBusy):
		return "busy 0"
	case errors.Is(err, engine.ErrBlocked):
		return "blocked"
	case errors.Is(err, engine.ErrInvalid):
		return "invalid"
	case errors.Is(err, engine.ErrNotActive):
		return "not-active"
	}
	return "error " + oneLine(err.Error())
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ExitCode is `moox save`'s exit for a result word (§5.5 table).
func ExitCode(result string) int {
	word, _, _ := strings.Cut(result, " ")
	switch word {
	case "published", "unchanged", "pristine":
		return 0
	case "restoring", "shutting-down", "busy":
		return 75
	case "blocked":
		return 5
	case "invalid":
		return 4
	case "not-active":
		return 3
	}
	return 1
}

// ---------------------------------------------------------------------------
// restores (§5.4, §6.4, §6.4a): every restore runs here
// ---------------------------------------------------------------------------

// beginRestore is "a restore is decided": restoring=true, no save timer
// survives, and the next save happens only after RESTORE-END.
func (m *machine) beginRestore() {
	m.restoring = true
	m.stop(&m.debounce)
	m.stop(&m.backup)
}

func (m *machine) onRestoreEnd(end restoreEnd) {
	m.restoring = false
	m.logf("RESTORE-END kind=%s %s; saving resumes (dirty=%v)", end.kind, end.result, m.dirty)
	if m.dirty {
		m.firstDirty, m.atCeiling = m.clk.Now(), false
		// One-shot, armed by the RESTORE-END event (§9.3a): the dirty
		// events collapsed during the restore become ONE save.
		m.debounce = m.arm(m.c.Debounce)
	}
}

// startBoot runs the boot restore (§6.4 daemon steps 1–3) in the restore
// goroutine: the bounded bootstrap poll, Restore, the LAUNCH verdict.
func (m *machine) startBoot(b Boot) {
	m.beginRestore()
	go func() {
		end := restoreEnd{kind: engine.KindBoot}
		defer func() { m.restoreDone <- end }()
		e := m.c.Engine
		if !e.WaitBootstrap(m.ctx, bootstrapPoll, bootstrapWithin) {
			if m.ctx.Err() == nil {
				e.SkipBoot(context.WithoutCancel(m.ctx), "no-bootstrap", m.ownBootMarker())
			}
			end.result = "SKIPPED:no-bootstrap"
			return
		}
		res, err := e.Restore(m.ctx, engine.RestoreRequest{Kind: engine.KindBoot, Stub: b.Stub})
		if err != nil {
			reason := "error"
			if errors.Is(err, engine.ErrLockTimeout) {
				reason = "lock-timeout"
			}
			m.logf("BOOT-RESTORE failed: %v", err)
			e.SkipBoot(context.WithoutCancel(m.ctx), reason, m.ownBootMarker())
			end.result = "SKIPPED:" + reason
			return
		}
		m.logf("BOOT-RESTORE created %d of %d rows from %s; BOOT %s; waiting for the LAUNCH verdict",
			len(res.Created), len(res.Plan.Rows), res.Source, res.Boot.Result)
		end.result = m.awaitLaunch(res)
	}()
}

func (m *machine) awaitLaunch(res engine.RestoreResult) string {
	select {
	case lv, ok := <-res.Launch:
		if !ok {
			return "LAUNCH (no verdict)"
		}
		return "BOOT " + res.Boot.Result + ", LAUNCH " + lv.Result
	case <-m.ctx.Done():
		return "LAUNCH (daemon stopping)"
	}
}

func (m *machine) readOption(name string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), 2*time.Second)
	defer cancel()
	out, err := m.tmux.Run(ctx, "show-options", "-gqv", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ownBootMarker is @moox-restoring when it is THIS daemon's boot marker
// (`boot:<our pid>:…`, set by the gate right after the fork), else "". A
// skipped boot clears only its own marker: another value read here — a
// `moox restart` shutdown marker, a requested restore — is never cleared by
// it (G1-M1).
func (m *machine) ownBootMarker() string {
	v := m.readOption("@moox-restoring")
	if kind, pid, ok := engine.RestoringOwner(v); ok && kind == engine.KindBoot && pid == os.Getpid() {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// saver.state
// ---------------------------------------------------------------------------

func (m *machine) state() string {
	switch {
	case m.restoring:
		return "restoring"
	case m.dirty:
		return "dirty"
	}
	return "idle"
}

func (m *machine) setState() { m.setStateTo(m.state()) }

func (m *machine) setStateTo(s string) {
	if s == m.lastState || m.c.OnState == nil {
		m.lastState = s
		return
	}
	m.lastState = s
	m.c.OnState(s)
}

// ---------------------------------------------------------------------------
// options
// ---------------------------------------------------------------------------

// Timing is the three timing options.
type Timing struct{ Debounce, MaxWait, Backup time.Duration }

// ReadTiming reads @moox-save-debounce, @moox-save-max-wait and
// @moox-save-backup in one exec. Each takes a Go duration or an integer
// number of seconds (PR-L1); an unset or unparsable value is the default;
// a backup of 0 disables the backup chain.
func ReadTiming(ctx context.Context, r Runner) Timing {
	t := Timing{DefaultDebounce, DefaultMaxWait, DefaultBackup}
	out, err := r.Run(ctx, "display-message", "-p",
		"#{@moox-save-debounce}\x1f#{@moox-save-max-wait}\x1f#{@moox-save-backup}")
	if err != nil {
		return t
	}
	f := strings.Split(strings.TrimRight(string(out), "\n"), "\x1f")
	if len(f) != 3 {
		return t
	}
	if d, ok := ParseDuration(f[0]); ok && d > 0 {
		t.Debounce = d
	}
	if d, ok := ParseDuration(f[1]); ok && d > 0 {
		t.MaxWait = d
	}
	if d, ok := ParseDuration(f[2]); ok && d >= 0 {
		t.Backup = d
	}
	return t
}

// ParseDuration takes "3s", "500ms", "15m" or a bare integer of seconds.
func ParseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}

// pidAlive is kill(pid, 0): EPERM still means the process exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
