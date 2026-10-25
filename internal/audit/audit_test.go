package audit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/audit"
)

// These are the four audit tests §10's P1 row names. Each one asserts THREE
// things, and the third is the one that stops the other two being theatre:
//
//  1. the real module produces zero findings;
//  2. the scanner visited a plausible number of files (a walker that silently
//     found nothing would otherwise pass clause 1 forever);
//  3. the scanner reports a deliberately violating snippet.
//
// Clause 3 exists because of a real defect in this design's own history:
// revision 0 claimed the tmux isolation guard was "asserted by the same AST
// audit as §9.4a", but that audit walks non-test files while every tmux exec
// site it had to police is in a _test.go. As written it audited nothing, and
// nothing about its passing said so.

// loadModule parses the whole module once per test. Cheap enough (well under a
// second for ~40 files) that sharing it between tests is not worth the
// coupling.
func loadModule(t *testing.T) (root string, files []audit.File) {
	t.Helper()
	root, err := audit.ModuleRoot(".")
	if err != nil {
		t.Fatalf("locating module root: %v", err)
	}
	files, err = audit.LoadModule(root)
	if err != nil {
		t.Fatalf("parsing module: %v", err)
	}
	// The module root is the directory whose go.mod declares this module
	// (moox architecture §2.2 step 4): a path suffix would pass for any
	// checkout directory name and fail for a correctly named one elsewhere.
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading the module root's go.mod: %v", err)
	}
	if first, _, _ := strings.Cut(string(mod), "\n"); strings.TrimSpace(first) != "module "+audit.ModulePath {
		t.Fatalf("module root is %q and its go.mod declares %q, expected \"module %s\"", root, first, audit.ModulePath)
	}
	return root, files
}

func loadSnippet(t *testing.T, rel, src string) audit.File {
	t.Helper()
	f, err := audit.LoadSource(rel, src)
	if err != nil {
		t.Fatalf("parsing snippet %s: %v", rel, err)
	}
	return f
}

// ---------------------------------------------------------------------------
// Audit 1 — §9.4a's package-keyed write audit
// ---------------------------------------------------------------------------

func TestNoCodeWeShipWritesToTheFilesystem(t *testing.T) {
	_, files := loadModule(t)
	nonTest := audit.NonTest(files)

	if len(nonTest) < 15 {
		t.Fatalf("only %d non-test files scanned; the walker is not seeing the module", len(nonTest))
	}

	if f := audit.Writes(nonTest, audit.WritingPackages); len(f) != 0 {
		t.Fatalf("code we ship writes to the filesystem outside the %d allowlisted packages:\n%s",
			len(audit.WritingPackages), audit.Format(f))
	}

	// The exception list is enumerated, not inferred. A second writing package
	// has to be added here deliberately, and this assertion is what makes
	// "deliberately" mean "with a test change in the same commit". It was two
	// (sockserve, agentscli) until code review round 2's A2: both were exempt
	// wholesale and both wrote by path after a lexical check. Now both write
	// through the one package below, which is itself held to ContainedWrites.
	if len(audit.WritingPackages) != 1 || audit.WritingPackages[0] != "internal/statedir" {
		t.Fatalf("the write allowlist is %v, expected exactly statedir", audit.WritingPackages)
	}

	// And the allowlisted package is NOT exempt from scrutiny: every write it
	// makes must be relative to its verified directory descriptor and refuse
	// to follow a symlink.
	if f := audit.ContainedWrites(nonTest, audit.WritingPackages); len(f) != 0 {
		t.Fatalf("the writing package makes a path-based or symlink-following write:\n%s", audit.Format(f))
	}
	scanned := 0
	for _, f := range nonTest {
		if f.Dir == audit.WritingPackages[0] {
			scanned++
		}
	}
	if scanned == 0 {
		t.Fatal("ContainedWrites scanned no file of the writing package; it is auditing nothing")
	}
}

func TestWriteAuditActuallyDetectsWrites(t *testing.T) {
	// One violation per banned shape, including the two an audit keyed only on
	// CallExpr would miss: a write-mode os.OpenFile, and a bare REFERENCE to a
	// write call stored in a variable.
	const violating = `package thing

import "os"

var alias = os.WriteFile

func write(p string) {
	os.MkdirAll(p, 0o755)
	os.Remove(p)
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o644)
	_ = f
}
`
	f := loadSnippet(t, "internal/observer/adapters/registryfs/bad.go", violating)
	findings := audit.Writes([]audit.File{f}, audit.WritingPackages)

	// alias, MkdirAll, Remove, OpenFile — plus os.Create appearing inside the
	// OpenFile flag expression is NOT a call and must not be counted twice.
	if len(findings) < 4 {
		t.Fatalf("expected at least 4 findings from the violating snippet, got %d:\n%s",
			len(findings), audit.Format(findings))
	}

	want := []string{"WriteFile", "MkdirAll", "Remove", "OpenFile"}
	got := audit.Format(findings)
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("findings do not mention %s:\n%s", w, got)
		}
	}

	// sockserve is no longer allowlisted (code review round 2, A2): the same
	// file there is now four findings like anywhere else.
	formerlyExempt := loadSnippet(t, "internal/observer/adapters/sockserve/pidfile.go", violating)
	if f := audit.Writes([]audit.File{formerlyExempt}, audit.WritingPackages); len(f) < 4 {
		t.Fatalf("sockserve's path-based writes produced %d findings, want at least 4:\n%s",
			len(f), audit.Format(f))
	}

	// Inside the ONE allowlisted package the package-keyed audit is silent —
	// which is what "keyed on package" means — but the exemption is no longer
	// blind: every one of these writes takes a PATH, and ContainedWrites
	// reports each of them.
	allowed := loadSnippet(t, "internal/statedir/pidfile.go", violating)
	if f := audit.Writes([]audit.File{allowed}, audit.WritingPackages); len(f) != 0 {
		t.Fatalf("allowlisted package reported findings:\n%s", audit.Format(f))
	}
	contained := audit.ContainedWrites([]audit.File{allowed}, audit.WritingPackages)
	for _, w := range want {
		if !strings.Contains(audit.Format(contained), w) {
			t.Errorf("ContainedWrites let the writing package's %s through:\n%s", w, audit.Format(contained))
		}
	}
}

// The writing package's own rule, from both sides: the fd-relative, no-follow
// shapes statedir actually uses are clean, and each way of writing by path or
// through a symlink is a finding.
func TestTheWritingPackageIsHeldToDirectoryRelativeNoFollowWrites(t *testing.T) {
	const good = `package statedir

import "golang.org/x/sys/unix"

func good(dirfd int, data []byte) {
	fd, _ := unix.Openat(dirfd, "tmp", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	_, _ = unix.Write(fd, data)
	_ = unix.Fchmod(fd, 0o600)
	_ = unix.Renameat(dirfd, "tmp", dirfd, "f")
	_ = unix.Unlinkat(dirfd, "f", 0)
	_ = unix.Mkdirat(dirfd, "d", 0o700)
	r, _ := unix.Openat(dirfd, "d", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = r
	root, _ := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	_ = root
}
`
	g := loadSnippet(t, "internal/statedir/good.go", good)
	if f := audit.ContainedWrites([]audit.File{g}, audit.WritingPackages); len(f) != 0 {
		t.Fatalf("the directory-relative shapes were rejected:\n%s", audit.Format(f))
	}

	const bad = `package statedir

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func bad(dirfd int, p string) {
	_ = os.WriteFile(p, nil, 0o600)
	_ = unix.Unlink(p)
	_ = unix.Rename(p, p)
	fd, _ := unix.Openat(dirfd, "f", unix.O_WRONLY|unix.O_CREAT, 0o600)
	_ = fd
	_ = unix.Unlinkat(unix.AT_FDCWD, p, 0)
	w, _ := unix.Open(p, unix.O_WRONLY|unix.O_CREAT, 0o600)
	_ = w
}
`
	b := loadSnippet(t, "internal/statedir/bad.go", bad)
	got := audit.Format(audit.ContainedWrites([]audit.File{b}, audit.WritingPackages))
	for _, w := range []string{"os.WriteFile", "unix.Unlink ", "unix.Rename ", "O_NOFOLLOW", "AT_FDCWD", "unix.Open with write flags"} {
		if !strings.Contains(got, w) {
			t.Errorf("ContainedWrites did not report %q:\n%s", w, got)
		}
	}
}

// Binding a unix socket creates an inode at a path — a write — and the socket
// server's bind used to sit outside every check.
func TestBindingASocketOutsideTheWritingPackageIsAFinding(t *testing.T) {
	const binding = `package sockserve

import "net"

func bind(p string) {
	ln, _ := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	_ = ln
	l2, _ := net.Listen("unix", p)
	_ = l2
}
`
	f := loadSnippet(t, "internal/observer/adapters/sockserve/bind.go", binding)
	if got := audit.Writes([]audit.File{f}, audit.WritingPackages); len(got) != 2 {
		t.Fatalf("binding outside statedir produced %d findings, want 2:\n%s", len(got), audit.Format(got))
	}
	inside := loadSnippet(t, "internal/statedir/bind.go", binding)
	if got := audit.Writes([]audit.File{inside}, audit.WritingPackages); len(got) != 0 {
		t.Fatalf("binding inside statedir was reported:\n%s", audit.Format(got))
	}
}

// The POSIX half, which the audit was blind to. Every call below is a real
// filesystem mutation spelled the way golang.org/x/sys/unix spells it, and
// before SyscallWriteCalls existed this exact snippet produced ZERO findings
// while the os-package snippet above produced three of three. watchkq is the
// package that makes it matter: not allowlisted, imports unix, and holds ~57
// descriptors on paths inside ~/.claude/.
func TestWriteAuditDetectsThePosixSpellingsToo(t *testing.T) {
	const violating = `package thing

import "golang.org/x/sys/unix"

var alias = unix.Unlink

func mutate(p string, fd int) {
	unix.Unlink(p)
	unix.Rmdir(p)
	unix.Write(fd, nil)
	unix.Pwrite(fd, nil, 0)
	unix.Ftruncate(fd, 0)
	unix.Fchmod(fd, 0o600)
	unix.Mkdirat(fd, p, 0o700)
	unix.Renameat(fd, p, fd, p)
	unix.Setxattr(p, "k", nil, 0)
	unix.Utimes(p, nil)
}
`
	f := loadSnippet(t, "internal/observer/adapters/watchkq/bad.go", violating)
	findings := audit.Writes([]audit.File{f}, audit.WritingPackages)

	// Ten calls plus the bare reference stored in a variable.
	for _, want := range []string{
		"Unlink", "Rmdir", "Write", "Pwrite", "Ftruncate",
		"Fchmod", "Mkdirat", "Renameat", "Setxattr", "Utimes",
	} {
		if !strings.Contains(audit.Format(findings), "unix."+want) {
			t.Errorf("findings do not mention unix.%s:\n%s", want, audit.Format(findings))
		}
	}
	if len(findings) < 11 {
		t.Fatalf("expected at least 11 findings (ten calls and one alias), got %d:\n%s",
			len(findings), audit.Format(findings))
	}

	// The syscall qualifier is the same surface under another name.
	const viaSyscall = `package thing

import "syscall"

func mutate(p string) { syscall.Unlink(p) }
`
	g := loadSnippet(t, "internal/observer/adapters/proclive/bad.go", viaSyscall)
	if got := audit.Writes([]audit.File{g}, audit.WritingPackages); len(got) != 1 {
		t.Fatalf("syscall.Unlink produced %d findings, want 1:\n%s", len(got), audit.Format(got))
	}

	// And the read-only syscalls this tree actually uses stay clean, or the
	// audit would be unusable where it is needed most.
	const readOnly = `package watchkq

import "golang.org/x/sys/unix"

func read(p string, fd int) {
	var st unix.Stat_t
	_ = unix.Stat(p, &st)
	_ = unix.Fstat(fd, &st)
	_ = unix.Close(fd)
	_, _ = unix.Kevent(fd, nil, nil, nil)
	_ = unix.CloseOnExec(fd)
	_, _ = unix.Read(fd, nil)
}
`
	h := loadSnippet(t, "internal/observer/adapters/watchkq/watch_darwin.go", readOnly)
	if got := audit.Writes([]audit.File{h}, audit.WritingPackages); len(got) != 0 {
		t.Fatalf("a read-only syscall was rejected:\n%s", audit.Format(got))
	}
}

func TestWriteAuditPermitsProvablyReadOnlyOpens(t *testing.T) {
	// The read-only open the kqueue watcher will need in P5. If this were
	// rejected, §6.3's per-file arming would have to be written inside one of
	// the two writing packages, which would defeat the point of the list.
	const readOnly = `package watchkq

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func arm(p string) {
	fd, _ := unix.Open(p, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	_ = fd
	f, _ := os.OpenFile(p, os.O_RDONLY, 0)
	_ = f
	g, _ := os.Open(p)
	_ = g
}
`
	f := loadSnippet(t, "internal/observer/adapters/watchkq/watch_darwin.go", readOnly)
	if got := audit.Writes([]audit.File{f}, audit.WritingPackages); len(got) != 0 {
		t.Fatalf("a provably read-only open was rejected:\n%s", audit.Format(got))
	}
}

// ---------------------------------------------------------------------------
// Audit 2 — §9.4a's test-file pass
// ---------------------------------------------------------------------------

func TestEveryTmuxExecInATestIsIsolated(t *testing.T) {
	_, files := loadModule(t)
	tests := audit.Tests(files)

	if len(tests) < 8 {
		t.Fatalf("only %d test files scanned; the walker is not seeing the test suite", len(tests))
	}

	if f := audit.TmuxExecInTests(tests); len(f) != 0 {
		t.Fatalf("a test shells out to tmux without both -f /dev/null and -L:\n%s", audit.Format(f))
	}

	// P10 CHANGED WHAT THIS ASSERTS. Every tmux-touching adapter is still
	// driven through a package-level function variable (the seam pattern
	// tmux.go uses), so the hermetic suite contains no tmux exec at all — but
	// tmuxlive_gate_test.go now contains one, behind a build tag, and the
	// clause above is what keeps its two guards on it. Asserted here rather
	// than logged, because "the audit polices a real site" is the difference
	// between this test and the detection test below.
	root, err := audit.ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := os.ReadFile(root + "/cmd/tt/tmuxlive_gate_test.go")
	if err != nil {
		t.Fatalf("the tmuxlive gate is missing, so this audit polices nothing: %v", err)
	}
	// Exactly one site, and it carries both guards as LITERALS of the call —
	// which is the form the scanner above can see. (Two comments in that file
	// quote other shapes on purpose, including the architecture's own, so this
	// counts the guarded constructor rather than the words `exec.Command`.)
	if n := strings.Count(string(gate), `exec.Command("tmux", "-f", "/dev/null", "-L"`); n != 1 {
		t.Errorf("the gate has %d guarded tmux constructors, want exactly 1 (liveTmux)", n)
	}
}

func TestTmuxIsolationAuditActuallyDetectsBareExec(t *testing.T) {
	const cases = `package tmuxlive

import (
	"context"
	"os/exec"
	"testing"
)

func TestBare(t *testing.T) {
	exec.Command("tmux", "list-panes", "-a")
}

func TestOnlyConf(t *testing.T) {
	exec.Command("tmux", "-f", "/dev/null", "list-panes")
}

func TestOnlySocket(t *testing.T) {
	exec.Command("tmux", "-L", "tt-test", "list-panes")
}

func TestGood(t *testing.T) {
	exec.Command("tmux", "-f", "/dev/null", "-L", "tt-test", "list-panes", "-a")
}

func TestGoodContext(t *testing.T) {
	exec.CommandContext(context.Background(), "tmux", "-f", "/dev/null", "-L", "tt-test", "kill-server")
}
`
	f := loadSnippet(t, "internal/observer/adapters/tmuxlive/live_test.go", cases)
	findings := audit.TmuxExecInTests([]audit.File{f})

	if len(findings) != 3 {
		t.Fatalf("expected exactly 3 findings (bare, conf-only, socket-only), got %d:\n%s",
			len(findings), audit.Format(findings))
	}
	got := audit.Format(findings)
	for _, fn := range []string{"TestBare", "TestOnlyConf", "TestOnlySocket"} {
		if !strings.Contains(got, fn) {
			t.Errorf("findings do not name %s:\n%s", fn, got)
		}
	}
	for _, fn := range []string{"TestGood", "TestGoodContext"} {
		if strings.Contains(got, fn) {
			t.Errorf("%s is correctly isolated but was reported:\n%s", fn, got)
		}
	}

	// A non-test file with the same content is NOT this audit's business —
	// pass 1 polices production code, pass 2 polices tests, and each has to
	// actually restrict itself to its own half.
	prod := loadSnippet(t, "internal/observer/adapters/tmuxlive/live.go",
		strings.ReplaceAll(cases, "func Test", "func Run"))
	if got := audit.TmuxExecInTests([]audit.File{prod}); len(got) != 0 {
		t.Fatalf("the test-file pass reported a non-test file:\n%s", audit.Format(got))
	}
}

// ---------------------------------------------------------------------------
// Audit 3 — §9.3a's timer rule and the T5 spawn precondition
// ---------------------------------------------------------------------------

func TestNoTimerIsArmedOutsideTheAllowlist(t *testing.T) {
	_, files := loadModule(t)
	nonTest := audit.NonTest(files)

	if f := audit.Timers(nonTest, audit.TimerAllowlist); len(f) != 0 {
		t.Fatalf("a banned timer appears outside the §9.3a allowlist:\n%s", audit.Format(f))
	}

	// Every allowlist entry must carry its citation. §9.3a fixes the format as
	// (site, rule-citation) precisely so an exemption cannot be added without
	// naming the clause that permits it.
	if len(audit.TimerAllowlist) == 0 {
		t.Fatal("the timer allowlist is empty, so this audit has never been exercised against a real site")
	}
	for _, entry := range audit.TimerAllowlist {
		if strings.TrimSpace(entry.Citation) == "" {
			t.Errorf("allowlist entry %q has no rule citation", entry.Site)
		}
		if !strings.Contains(entry.Site, ":") {
			t.Errorf("allowlist entry %q is not in file:func form", entry.Site)
		}
	}

	// The allowlist is LOAD-BEARING, not decorative: removing it must make the
	// real tree fail. This is the assertion that proves the audit is looking at
	// the module rather than at nothing — §6.5's torn-read retry is a genuine
	// time.Sleep in shipped code, and with an empty allowlist it must surface.
	withoutAllowlist := audit.Timers(nonTest, nil)
	if len(withoutAllowlist) == 0 {
		t.Fatal("with an empty allowlist the audit still found nothing: it is not scanning the module")
	}
	if !strings.Contains(audit.Format(withoutAllowlist), "registryfs") {
		t.Errorf("expected §6.5's torn-read retry to be the allowlisted site, got:\n%s",
			audit.Format(withoutAllowlist))
	}
}

func TestTimerAuditActuallyDetectsBannedTimers(t *testing.T) {
	const violating = `package refresh

import "time"

var t0 = time.Tick(time.Second)

func loop() {
	tk := time.NewTicker(time.Second)
	_ = tk
	<-time.After(time.Second)
	time.Sleep(time.Second)
}

func permitted() {
	// A one-shot timer armed by an event is exactly what the rule allows.
	tm := time.NewTimer(time.Second)
	defer tm.Stop()
}
`
	f := loadSnippet(t, "internal/observer/refresh.go", violating)
	findings := audit.Timers([]audit.File{f}, audit.TimerAllowlist)

	if len(findings) != 4 {
		t.Fatalf("expected 4 findings (Tick, NewTicker, After, Sleep), got %d:\n%s",
			len(findings), audit.Format(findings))
	}
	got := audit.Format(findings)
	for _, w := range []string{"time.Tick", "time.NewTicker", "time.After", "time.Sleep"} {
		if !strings.Contains(got, w) {
			t.Errorf("findings do not mention %s:\n%s", w, got)
		}
	}
	if strings.Contains(got, "NewTimer") {
		t.Errorf("time.NewTimer is a permitted one-shot and must not be flagged:\n%s", got)
	}

	// An entry with no citation grants no exemption.
	uncited := []audit.TimerAllow{{Site: "internal/observer/refresh.go:loop"}}
	if len(audit.Timers([]audit.File{f}, uncited)) != 4 {
		t.Error("an allowlist entry with no rule citation granted an exemption")
	}
	cited := []audit.TimerAllow{{Site: "internal/observer/refresh.go:loop", Citation: "§9.3a, argued"}}
	if n := len(audit.Timers([]audit.File{f}, cited)); n != 1 {
		t.Errorf("a cited entry should exempt loop()'s three sites, leaving the package-scope one; got %d", n)
	}
}

func TestT5SpawnPreconditionGuardsEveryCorroborationSpawn(t *testing.T) {
	_, files := loadModule(t)
	nonTest := audit.NonTest(files)

	if f := audit.AgentsSpawnGuard(nonTest); len(f) != 0 {
		t.Fatalf("a corroboration listing is taken without T5's staleAfterMs precondition:\n%s",
			audit.Format(f))
	}

	// Honest about the state of the tree: P1 contains no call into the agents
	// source at all, so this assertion is currently vacuous on the real module
	// and the detection test below is what makes it a rule rather than a
	// comment. P4 is the phase that makes it bite.
	t.Log("agents-cli call sites in shipped code: 0 (P4 introduces the first); " +
		"proven by TestT5AuditActuallyDetectsAnUnguardedSpawn")
}

// TestTheT5AllowlistIsOneCitedEntry. An allowlist is only as honest as the
// pressure to keep it small, so its size and its citations are asserted rather
// than reviewed — the same shape §5.1's omitempty exception is held to.
func TestTheT5AllowlistIsOneCitedEntry(t *testing.T) {
	if len(audit.AgentsSpawnAllowlist) != 1 {
		t.Fatalf("the T5 allowlist has %d entries; P10 added exactly one (the doctor)",
			len(audit.AgentsSpawnAllowlist))
	}
	entry := audit.AgentsSpawnAllowlist[0]
	if entry.Dir != "internal/observer/adapters/doctorfs" {
		t.Errorf("the allowlisted package is %q, want the doctor's", entry.Dir)
	}
	if !strings.Contains(entry.Citation, "§9.5") {
		t.Errorf("the entry carries no §9.5 citation: %q", entry.Citation)
	}
}

// TestTheT5AuditStillPolicesEveryOtherPackage. The allowlist is keyed on the
// package directory, so widening it must not weaken the rule anywhere else.
func TestTheT5AuditStillPolicesEveryOtherPackage(t *testing.T) {
	const violating = `package watchloop

import "context"

type o struct{ deps struct{ Agents interface{ List(context.Context) (any, error) } } }

func (x *o) onChange(ctx context.Context) {
	_, _ = x.deps.Agents.List(ctx)
}
`
	f := loadSnippet(t, "internal/observer/watchloop.go", violating)
	if got := audit.AgentsSpawnGuard([]audit.File{f}); len(got) != 1 {
		t.Fatalf("expected 1 finding outside the allowlist, got %d:\n%s", len(got), audit.Format(got))
	}

	inDoctor := loadSnippet(t, "internal/observer/adapters/doctorfs/doctorfs.go", violating)
	if got := audit.AgentsSpawnGuard([]audit.File{inDoctor}); len(got) != 0 {
		t.Fatalf("the allowlisted package was still reported:\n%s", audit.Format(got))
	}
}

func TestT5AuditActuallyDetectsAnUnguardedSpawn(t *testing.T) {
	const unguarded = `package observer

import "context"

type deps struct{ Agents interface{ List(context.Context) (int, error) } }

type o struct {
	deps   deps
	policy struct{ StaleAfterMs int64 }
}

func (x *o) rebuildAndPublish(ctx context.Context, ageMs int64) {
	// This is the shape §4.2 forbids: a spawn on the Change branch with
	// nothing comparing the existing listing's age to the threshold.
	x.deps.Agents.List(ctx)
}

func (x *o) guarded(ctx context.Context, ageMs int64) {
	if ageMs > x.policy.StaleAfterMs {
		x.deps.Agents.List(ctx)
	}
}
`
	f := loadSnippet(t, "internal/observer/refresh.go", unguarded)
	findings := audit.AgentsSpawnGuard([]audit.File{f})

	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding (the unguarded spawn), got %d:\n%s",
			len(findings), audit.Format(findings))
	}
	if !strings.Contains(findings[0].Func, "rebuildAndPublish") {
		t.Errorf("the finding names %q, expected rebuildAndPublish", findings[0].Func)
	}

	// The adapter that IS the agents source obviously calls it, and is exempt.
	adapter := loadSnippet(t, "internal/observer/adapters/agentscli/agentscli.go", unguarded)
	if got := audit.AgentsSpawnGuard([]audit.File{adapter}); len(got) != 0 {
		t.Fatalf("the agentscli adapter itself was policed:\n%s", audit.Format(got))
	}
}

// ---------------------------------------------------------------------------
// Audit 4 — §5.1's omitempty and json-tag bans
// ---------------------------------------------------------------------------

func TestOmitemptyIsBannedEverywhere(t *testing.T) {
	_, files := loadModule(t)

	// Test files included, deliberately: a test struct with omitempty would
	// encode a shape no production surface can produce, and an assertion
	// against it would be an assertion about nothing.
	if f := audit.Omitempty(files, audit.OmitemptyAllowlist); len(f) != 0 {
		t.Fatalf("omitempty appears in a struct tag:\n%s", audit.Format(f))
	}

	// With NO allowlist the real tree is still clean. That is the substantive
	// finding: the single named exception is not actually needed by any field,
	// because waitReason expresses its two absences as "" and "unknown"
	// instead of as a missing key.
	if f := audit.Omitempty(files, nil); len(f) != 0 {
		t.Fatalf("the tree depends on the omitempty allowlist, which §5.1 does not permit:\n%s",
			audit.Format(f))
	}

	if len(audit.OmitemptyAllowlist) != 1 || audit.OmitemptyAllowlist[0].JSONName != "waitReason" {
		t.Fatalf("§5.1 allows exactly one named exception, waitReason; got %+v",
			audit.OmitemptyAllowlist)
	}
	if strings.TrimSpace(audit.OmitemptyAllowlist[0].Citation) == "" {
		t.Error("the waitReason exception carries no citation, which §5.1 requires")
	}
}

func TestOmitemptyAuditActuallyDetectsOmitempty(t *testing.T) {
	const violating = `package wire

type Session struct {
	BlockedForMs *int64  ` + "`json:\"blockedForMs,omitempty\"`" + `
	Subagents    *int    ` + "`json:\"subagents,omitempty\"`" + `
	WaitReason   string  ` + "`json:\"waitReason,omitempty\"`" + `
	Status       string  ` + "`json:\"status\"`" + `
}
`
	f := loadSnippet(t, "internal/observer/wire/wire.go", violating)

	// waitReason is the allowlisted name, so with the allowlist in force only
	// the two tri-state fields are reported.
	withAllow := audit.Omitempty([]audit.File{f}, audit.OmitemptyAllowlist)
	if len(withAllow) != 2 {
		t.Fatalf("expected 2 findings with the allowlist in force, got %d:\n%s",
			len(withAllow), audit.Format(withAllow))
	}
	got := audit.Format(withAllow)
	if !strings.Contains(got, "blockedForMs") || !strings.Contains(got, "subagents") {
		t.Errorf("findings do not cover both tri-state fields:\n%s", got)
	}

	if n := len(audit.Omitempty([]audit.File{f}, nil)); n != 3 {
		t.Errorf("expected 3 findings with no allowlist, got %d", n)
	}
}

func TestJSONTagsExistOnlyInWire(t *testing.T) {
	_, files := loadModule(t)

	// One tag set per on-disk/wire format (moox §2.4): tt's wire, the
	// snapshot format, the setup manifest.
	if f := audit.JSONTags(files, audit.WireDirs...); len(f) != 0 {
		t.Fatalf("a json struct tag exists outside %v, so four surfaces can now disagree:\n%s",
			audit.WireDirs, audit.Format(f))
	}

	// And the walker really does see tags: wire itself carries plenty. Without
	// this the audit above would pass on a tree where the tag walker was
	// broken.
	wire := make([]audit.File, 0, 4)
	for _, f := range files {
		if f.Dir == audit.WirePackageDir && !f.IsTest {
			wire = append(wire, f)
		}
	}
	if len(wire) == 0 {
		t.Fatalf("no non-test files found in %s", audit.WirePackageDir)
	}
	if n := audit.CountJSONTags(wire); n < 40 {
		t.Fatalf("found only %d json tags in %s; the tag walker is not working",
			n, audit.WirePackageDir)
	}
}

func TestJSONTagAuditActuallyDetectsTagsOutsideWire(t *testing.T) {
	const violating = `package jsonout

type localDTO struct {
	SessionID string ` + "`json:\"sessionId\"`" + `
	Live      *bool  ` + "`json:\"live\"`" + `
}

func build() any {
	type inlineDTO struct {
		Extra string ` + "`json:\"extra\"`" + `
	}
	return inlineDTO{}
}
`
	f := loadSnippet(t, "internal/observer/adapters/jsonout/jsonout.go", violating)
	findings := audit.JSONTags([]audit.File{f}, audit.WirePackageDir)

	// Three, including the one declared inside a function body — which is
	// where a "just this once" DTO actually gets written.
	if len(findings) != 3 {
		t.Fatalf("expected 3 findings, got %d:\n%s", len(findings), audit.Format(findings))
	}
	if !strings.Contains(audit.Format(findings), "Extra") {
		t.Errorf("the function-local DTO was not reported:\n%s", audit.Format(findings))
	}

	inWire := loadSnippet(t, audit.WirePackageDir+"/wire.go", violating)
	if got := audit.JSONTags([]audit.File{inWire}, audit.WirePackageDir); len(got) != 0 {
		t.Fatalf("tags inside wire were reported:\n%s", audit.Format(got))
	}
}
