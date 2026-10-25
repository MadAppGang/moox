// These tests use only the documented public subprocess and wire surfaces.
// TestMain builds this checkout once per run; no prebuilt binary is accepted.
package blackbox_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type object = map[string]any

var checkoutBinary string

func TestMain(m *testing.M) { os.Exit(runBlackbox(m)) }

func runBlackbox(m *testing.M) int {
	pkgDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	moduleDir := filepath.Dir(pkgDir)
	buildDir, err := os.MkdirTemp("/tmp", "ttbb-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(buildDir)
	checkoutBinary = filepath.Join(buildDir, "tt")
	fmt.Printf("BLACKBOX checkout module: %s\n", moduleDir)
	if _, set := os.LookupEnv("TT_BB_BINARY"); set {
		fmt.Println("BLACKBOX TT_BB_BINARY IGNORED: only the freshly built checkout is tested")
	}
	// Git metadata only: never read or print implementation source or diffs.
	for _, query := range []struct {
		label string
		args  []string
	}{
		{"HEAD", []string{"rev-parse", "HEAD"}},
		{"HEAD tree", []string{"rev-parse", "HEAD^{tree}"}},
		{"working tree status (porcelain v1; includes untracked files)", []string{"status", "--porcelain=v1", "--untracked-files=all"}},
	} {
		cmd := exec.Command("git", query.args...)
		cmd.Dir = moduleDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot record %s: %v\n%s", query.label, err, out)
			return 1
		}
		fmt.Printf("BLACKBOX %s:\n%s", query.label, out)
		if len(out) == 0 {
			fmt.Println("(clean)")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", checkoutBinary, ".")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.WaitDelay = time.Second
	fmt.Println("BLACKBOX build: go build -mod=readonly -o <fresh temporary directory>/tt .")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "checkout build failed: %v\n%s", err, out)
		return 1
	}
	binary, err := os.Open(checkoutBinary)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, binary)
	closeErr := binary.Close()
	if copyErr != nil || closeErr != nil {
		fmt.Fprintf(os.Stderr, "binary hashing failed: copy=%v close=%v\n", copyErr, closeErr)
		return 1
	}
	fmt.Printf("BLACKBOX tested binary: %s\nBLACKBOX binary SHA-256: %x\n", checkoutBinary, hash.Sum(nil))
	return m.Run()
}

// Source IDs are semantic; the brief does not require diagnostic layout tests.
func sourceByID(t *testing.T, sources object, id string) object {
	t.Helper()
	for _, value := range sources {
		if s, ok := value.(map[string]any); ok && s["id"] == id {
			return s
		}
	}
	t.Fatalf("source id %q absent: %v", id, sources)
	return nil
}

func sourceCounter(t *testing.T, source object, key string) any {
	t.Helper()
	if value, ok := source[key]; ok {
		return value
	}
	return field(t, obj(t, field(t, source, "counters")), key)
}

type fixture struct {
	t                          *testing.T
	root, home, socket, binary string
	env                        []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// Short paths avoid Darwin's sockaddr_un limit even for long test names.
	root, err := os.MkdirTemp("/tmp", "ttbb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	f := &fixture{t: t, root: root, home: filepath.Join(root, "home"), socket: filepath.Join(root, "socket")}
	f.binary = checkoutBinary
	if _, err := os.Stat(f.binary); err != nil {
		t.Fatalf("TestMain checkout binary unavailable: %v", err)
	}
	for _, dir := range []string{f.home, f.socket, filepath.Join(root, "tmux"), filepath.Join(root, "bin"), filepath.Join(f.home, ".claude", "sessions"), filepath.Join(f.home, ".claude", "jobs"), filepath.Join(f.home, ".claude", "projects")} {
		f.mkdir(dir)
	}
	// A minimal environment does not inherit real Claude/XDG/tmux configuration.
	f.env = []string{"HOME=" + f.home, "PATH=" + filepath.Join(root, "bin"), "TMUX_TMPDIR=" + filepath.Join(root, "tmux"), "TMPDIR=" + root, "XDG_STATE_HOME=" + filepath.Join(f.home, ".local", "state"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"), "TERM=dumb", "LANG=C", "LC_ALL=C"}
	f.write(filepath.Join(root, "bin", "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$$\" >> \"$HOME/claude-pids\"\nif [ \"$1\" != agents ] || [ \"$2\" != --json ]; then exit 64; fi\nif [ -f \"$HOME/hang-claude\" ]; then exec /bin/sleep 86400; fi\nexec /bin/cat \"$HOME/agents.json\"\n"))
	if err := os.Chmod(filepath.Join(root, "bin", "claude"), 0700); err != nil {
		t.Fatal(err)
	}
	// Fact 7 documents the three identity format variables. The allowed docs
	// do not fix tt's list-panes argv/column layout: render these variables in
	// the requested -F format, instead of guessing an implementation delimiter.
	// README also defines pane_dead. moox §4.5 added two things every tt tmux
	// exec now carries: the global "-u -S <socket>" prefix, and #{pane_pid} in
	// the listing (the foreign-server rule). The fake accepts the prefix and
	// renders pane_pid from an optional fifth column — empty by default, which
	// tt reads as "no pids", i.e. today's pane-set-only gate. All other
	// commands/formats fail closed.
	f.write(filepath.Join(root, "bin", "tmux"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$HOME/tmux-calls"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -u) shift ;;
    -S) [ "$#" -ge 2 ] || exit 64; shift 2 ;;
    *) break ;;
  esac
done
if [ "$1" != list-panes ]; then exit 64; fi
shift
format='#{session_name}:#{window_id}.#{pane_id}'
while [ "$#" -gt 0 ]; do
  case "$1" in
    -a) shift ;;
    -F) [ "$#" -ge 2 ] || exit 64; format=$2; shift 2 ;;
    *) printf 'unsupported fake tmux argument\n' >&2; exit 64 ;;
  esac
done
if [ -f "$HOME/tmux-fail" ]; then
  printf 'pane gate unavailable (isolated blackbox fixture)\n' >&2
  exit 1
fi
export TT_BB_PANE_FORMAT="$format"
exec /usr/bin/awk -F '\t' '
BEGIN {
  format = ENVIRON["TT_BB_PANE_FORMAT"]
  check = format
  gsub(/#\{session_name\}|#\{window_id\}|#\{pane_id\}|#\{pane_dead\}|#\{pane_pid\}/, "", check)
  if (index(check, "#{")) exit 64
}
{
  line = format
  gsub(/#\{session_name\}/, $1, line)
  gsub(/#\{window_id\}/, $2, line)
  gsub(/#\{pane_id\}/, $3, line)
  gsub(/#\{pane_dead\}/, $4, line)
  gsub(/#\{pane_pid\}/, $5, line)
  print line
}' "$HOME/tmux-panes"
`))
	if err := os.Chmod(filepath.Join(root, "bin", "tmux"), 0700); err != nil {
		t.Fatal(err)
	}
	f.panes()
	f.json(filepath.Join(f.home, "agents.json"), []any{})
	return f
}

// Each row is session_name, window_id, pane_id, pane_dead separated by tabs.
func (f *fixture) panes(rows ...string) {
	f.t.Helper()
	data := strings.Join(rows, "\n")
	if len(rows) > 0 {
		data += "\n"
	}
	f.write(filepath.Join(f.home, "tmux-panes"), []byte(data))
}

func (f *fixture) mkdir(path string) {
	f.t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) write(path string, b []byte) {
	f.t.Helper()
	f.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, b, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) json(path string, v any) {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(path, append(b, '\n'))
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func instant() time.Time       { return time.Now().UTC().Truncate(time.Millisecond) }

func (f *fixture) ownedPID() int {
	f.t.Helper()
	c := exec.Command("/bin/sleep", "600")
	c.Env = f.env
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); _ = c.Wait() })
	return c.Process.Pid
}

func (f *fixture) deadPID() int {
	f.t.Helper()
	c := exec.Command("/usr/bin/true")
	c.Env = f.env
	if err := c.Start(); err != nil {
		f.t.Fatal(err)
	}
	pid := c.Process.Pid
	if err := c.Wait(); err != nil {
		f.t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		f.t.Fatalf("dead-pid fixture is not dead: %v", err)
	}
	return pid
}

func (f *fixture) registry(id, status, waiting string, since time.Time, pid int) (string, object) {
	f.t.Helper()
	r := object{"sessionId": id, "pid": pid, "status": status, "name": id, "nameSource": "user", "cwd": filepath.Join(f.home, "work", "repo_with_underscore"), "startedAt": since.Add(-24 * time.Hour).UnixMilli(), "statusUpdatedAt": since.UnixMilli(), "updatedAt": since.UnixMilli(), "version": "2.1.275", "formerNames": []any{}}
	if waiting != "" {
		r["waitingFor"] = waiting
	}
	path := filepath.Join(f.home, ".claude", "sessions", strconv.Itoa(pid)+".json")
	f.json(path, r)
	return path, r
}

func (f *fixture) transcript(slug, id string, records ...object) string {
	f.t.Helper()
	var b bytes.Buffer
	for _, r := range records {
		if err := json.NewEncoder(&b).Encode(r); err != nil {
			f.t.Fatal(err)
		}
	}
	p := filepath.Join(f.home, ".claude", "projects", slug, id+".jsonl")
	f.write(p, b.Bytes())
	return p
}

func (f *fixture) job(dir, id, state string, updated time.Time, transcript string) (string, object) {
	f.t.Helper()
	r := object{"sessionId": id, "daemonShort": dir, "state": state, "tempo": state, "name": id, "nameSource": "user", "cwd": filepath.Join(f.home, "work", "jobs"), "createdAt": stamp(updated.Add(-10 * 24 * time.Hour)), "updatedAt": stamp(updated), "linkScanPath": transcript, "linkScanOffset": 0, "children": []any{}, "needs": "inspect fixture output", "detail": "fixture detail", "tokens": 17, "cliVersion": "2.1.275", "resumeSessionId": id, "backend": "local", "firstTerminalAt": nil, "intent": "PRIVATE_INTENT_SENTINEL", "output": "PRIVATE_OUTPUT_SENTINEL", "providerEnv": object{}, "respawnFlags": []any{}, "template": nil}
	// The record's folder exists: rule 2c (job-folder-gone) answers null for a
	// confirmed-absent cwd, and these fixtures describe jobs whose folder is there.
	if err := os.MkdirAll(r["cwd"].(string), 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(f.home, ".claude", "jobs", dir, "state.json")
	f.json(p, r)
	return p, r
}

func (f *fixture) agents(rows ...object) { f.json(filepath.Join(f.home, "agents.json"), rows) }
func agent(id string, pid int, status string) object {
	r := object{"sessionId": id, "kind": "interactive", "pid": pid, "name": id, "cwd": "/fixture", "startedAt": instant().Add(-time.Hour).UnixMilli(), "status": status}
	return r
}
func backgroundAgent(dir, id, state string) object {
	return object{"sessionId": id, "id": dir, "kind": "background", "state": state, "name": id, "cwd": "/fixture", "startedAt": instant().Add(-time.Hour).UnixMilli()}
}

func (f *fixture) run(args ...string) ([]byte, []byte, int) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, f.binary, args...)
	c.Env = f.env
	c.Dir = f.home
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	var out, errout bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errout
	err := c.Run()
	if c.Process != nil {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		f.t.Fatalf("tt %v exceeded 12s (owned process group killed): %s", args, errout.String())
	}
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			f.t.Fatal(err)
		}
	}
	return out.Bytes(), errout.Bytes(), code
}

func decode(t *testing.T, b []byte) object {
	t.Helper()
	var o object
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return o
}
func (f *fixture) sessions(args ...string) object {
	f.t.Helper()
	argv := append([]string{"sessions", "--json"}, args...)
	out, stderr, code := f.run(argv...)
	if code != 0 {
		f.t.Fatalf("tt %v exited %d: %s\n%s", argv, code, stderr, out)
	}
	return decode(f.t, out)
}
func obj(t *testing.T, v any) object {
	t.Helper()
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("wanted object, got %#v", v)
	}
	return o
}
func list(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("wanted array, got %#v", v)
	}
	return a
}
func field(t *testing.T, o object, k string) any {
	t.Helper()
	v, ok := o[k]
	if !ok {
		t.Fatalf("required field %q is absent in %v", k, o)
	}
	return v
}
func eq(t *testing.T, got, want any, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %#v; want %#v", label, got, want)
	}
}
func session(t *testing.T, snapshot object, id string) object {
	t.Helper()
	for _, v := range list(t, field(t, snapshot, "sessions")) {
		s := obj(t, v)
		if s["sessionId"] == id {
			return s
		}
	}
	t.Fatalf("session %q missing: %v", id, snapshot["sessions"])
	return nil
}
func ids(t *testing.T, snapshot object) []string {
	t.Helper()
	result := []string{}
	for _, v := range list(t, field(t, snapshot, "sessions")) {
		s := obj(t, v)
		result = append(result, fmt.Sprint(s["sessionId"]))
	}
	sort.Strings(result)
	return result
}
func hasAdvisory(t *testing.T, s object, code string) bool {
	t.Helper()
	for _, v := range list(t, field(t, s, "advisories")) {
		if obj(t, v)["code"] == code {
			return true
		}
	}
	return false
}
func requireAdvisory(t *testing.T, s object, code string) {
	t.Helper()
	if !hasAdvisory(t, s, code) {
		t.Errorf("missing advisory %q: %v", code, s["advisories"])
	}
}
func parsedTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("wanted timestamp, got %#v", v)
	}
	out, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkTime(t *testing.T, s object, since time.Time, attention bool) {
	t.Helper()
	eq(t, parsedTime(t, field(t, s, "statusSince")), since, "statusSince preserves authoritative timestamp")
	observed := parsedTime(t, field(t, s, "observedAt"))
	age := float64(observed.Sub(since).Milliseconds())
	eq(t, field(t, s, "statusAgeMs"), age, "statusAgeMs = observedAt - statusSince")
	eq(t, field(t, s, "attention"), attention, "attention")
	if attention {
		eq(t, field(t, s, "blockedForMs"), field(t, s, "statusAgeMs"), "blockedForMs = statusAgeMs")
	} else {
		eq(t, field(t, s, "blockedForMs"), nil, "non-attention blockedForMs")
	}
}

func TestInteractiveLivenessAndIncludeDead(t *testing.T) {
	f := newFixture(t)
	now := instant()
	alive := f.ownedPID()
	dead := f.deadPID()
	f.registry("live-session", "idle", "", now, alive)
	f.registry("dead-session", "waiting", "input needed", now, dead)
	f.agents(agent("live-session", alive, "idle"), agent("dead-session", dead, "waiting"))
	s := f.sessions()
	eq(t, ids(t, s), []string{"live-session"}, "AC1 default live set")
	a := session(t, s, "live-session")
	eq(t, field(t, a, "live"), true, "owned pid alive")
	eq(t, field(t, a, "procAlive"), true, "procAlive")
	eq(t, field(t, a, "liveReason"), "proc-only", "no tmux reference")
	all := f.sessions("--include-dead")
	eq(t, ids(t, all), []string{"dead-session", "live-session"}, "includeDead")
	d := session(t, all, "dead-session")
	eq(t, field(t, d, "live"), false, "dead pid")
	eq(t, field(t, d, "liveReason"), "pid-dead", "reason")
	eq(t, obj(t, all["counts"])["attention"], float64(0), "dead waiting record not attention count")
}

func TestPublicCommandHelp(t *testing.T) {
	for _, command := range []string{"sessions", "observe", "mcp"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t)
			out, stderr, code := f.run(command, "--help")
			eq(t, code, 0, "public help exits successfully")
			t.Logf("fresh checkout tt %s --help:\n%s%s", command, out, stderr)
		})
	}
}

func TestInteractivePaneLiveness(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows       []string
		gateError  bool
		live, pane any
		reason     string
	}{
		{"present", []string{"unrelated\t@8\t%9\t0", "bb-session\t@51\t%107\t0"}, false, true, true, "proc+pane"},
		{"gone-other-pane", []string{"bb-session\t@51\t%108\t0"}, false, false, false, "pane-gone"},
		{"gate-error", []string{"bb-session\t@51\t%107\t0"}, true, nil, nil, "gate-error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			pid := f.ownedPID()
			path, row := f.registry("pane-session", "waiting", "input needed", instant(), pid)
			row["tmux"] = "bb-session:@51.%107"
			f.json(path, row)
			f.agents(agent("pane-session", pid, "waiting"))
			f.panes(tc.rows...)
			if tc.gateError {
				f.write(filepath.Join(f.home, "tmux-fail"), []byte("fail pane listing\n"))
			}
			// Prove that the fake supplies the intended pane evidence or the
			// explicit error before interpreting tt's answer. The docs do not
			// define a successful empty listing as valid pane-gone evidence.
			probe := exec.Command(filepath.Join(f.root, "bin", "tmux"), "list-panes", "-a", "-F", "#{session_name}:#{window_id}.#{pane_id}")
			probe.Env = f.env
			probeOut, probeErr := probe.CombinedOutput()
			if tc.gateError {
				if e, ok := probeErr.(*exec.ExitError); !ok || e.ExitCode() != 1 {
					t.Fatalf("fake error control: got %v; want exit 1", probeErr)
				}
			} else {
				if probeErr != nil {
					t.Fatalf("fake successful listing failed: %v; %s", probeErr, probeOut)
				}
				var want strings.Builder
				for _, row := range tc.rows {
					parts := strings.Split(row, "\t")
					fmt.Fprintf(&want, "%s:%s.%s\n", parts[0], parts[1], parts[2])
				}
				if string(probeOut) != want.String() {
					t.Fatalf("fake listing: got %q; want %q", probeOut, want.String())
				}
			}
			t.Logf("independent fake control: error=%v output=%q", probeErr, probeOut)
			all := f.sessions("--include-dead")
			s := session(t, all, "pane-session")
			t.Logf("tt pane evidence: procAlive=%v paneAlive=%v live=%v liveReason=%v advisories=%v", s["procAlive"], s["paneAlive"], s["live"], s["liveReason"], s["advisories"])
			eq(t, field(t, s, "procAlive"), true, "owned pid remains alive in every pane case")
			eq(t, field(t, s, "paneAlive"), tc.pane, "pane gate evidence")
			eq(t, field(t, s, "live"), tc.live, "two-gate liveness")
			eq(t, field(t, s, "liveReason"), tc.reason, "deciding liveness rule")
			eq(t, field(t, s, "tmux"), object{"raw": "bb-session:@51.%107", "session": "bb-session", "window": "@51", "pane": "%107"}, "documented tmux identity preserves sigils")
			if tc.gateError {
				requireAdvisory(t, s, "liveness-unknown")
			}
			wantIDs := []string{"pane-session"}
			wantAttention := float64(1)
			if tc.live == false {
				wantIDs = []string{}
				wantAttention = 0
			}
			eq(t, ids(t, f.sessions()), wantIDs, "default excludes false but retains unknown liveness")
			counts := obj(t, field(t, all, "counts"))
			eq(t, field(t, counts, "attention"), wantAttention, "dead pane cannot contribute attention")
			for _, bucket := range []string{"live", "dead", "unknown"} {
				want := float64(0)
				if (bucket == "live" && tc.live == true) || (bucket == "dead" && tc.live == false) || (bucket == "unknown" && tc.live == nil) {
					want = 1
				}
				eq(t, field(t, counts, bucket), want, "liveness count "+bucket)
			}
			calls, err := os.ReadFile(filepath.Join(f.home, "tmux-calls"))
			if err != nil || !strings.Contains(string(calls), "list-panes") {
				t.Fatalf("fake pane gate was never queried: %v; %s", err, calls)
			}
			t.Logf("fixture tmux calls (observed, not a pinned argv contract):\n%s", calls)
		})
	}
}

func TestWaitingReasonsAndNonWaitingEmpty(t *testing.T) {
	for _, tc := range []struct{ status, raw, want string }{{"waiting", "input needed", "input-needed"}, {"waiting", "permission prompt", "permission-prompt"}, {"waiting", "", "unknown"}, {"waiting", "future reason", "unknown"}, {"idle", "input needed", ""}, {"busy", "permission prompt", ""}, {"shell", "input needed", ""}} {
		t.Run(tc.status+"/"+tc.raw, func(t *testing.T) {
			f := newFixture(t)
			pid := f.ownedPID()
			f.registry("reason", tc.status, tc.raw, instant(), pid)
			s := session(t, f.sessions(), "reason")
			eq(t, field(t, s, "waitReason"), tc.want, "AC2 waitReason")
			eq(t, field(t, s, "rawStatus"), tc.status, "rawStatus")
			eq(t, field(t, s, "attention"), tc.status == "waiting", "attention")
		})
	}
}

func TestRegistryAgesIgnoreTranscriptMtime(t *testing.T) {
	for _, status := range []string{"idle", "waiting"} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			since := instant().Add(-72 * time.Hour)
			pid := f.ownedPID()
			f.registry("aged", status, "input needed", since, pid)
			latest := instant().Add(-time.Minute)
			p := f.transcript("unrelated-actual-directory", "aged", object{"type": "assistant", "timestamp": stamp(latest)})
			if err := os.Chtimes(p, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			s := session(t, f.sessions(), "aged")
			checkTime(t, s, since, status == "waiting")
			if activity := field(t, s, "lastActivityAt"); activity == nil {
				t.Errorf("architecture 3.4 requires newest transcript timestamp %s for enriched interactive session; lastActivityAt is null (authoritative ages passed)", stamp(latest))
			} else {
				eq(t, parsedTime(t, activity), latest, "independent transcript activity")
			}
		})
	}
}

func TestMissingStatusTimestampIsNullWithAdvisory(t *testing.T) {
	f := newFixture(t)
	p, r := f.registry("missing-time", "waiting", "input needed", instant().Add(-72*time.Hour), f.ownedPID())
	delete(r, "statusUpdatedAt")
	f.json(p, r)
	s := session(t, f.sessions(), "missing-time")
	for _, k := range []string{"statusSince", "statusAgeMs", "blockedForMs"} {
		eq(t, field(t, s, k), nil, k)
	}
	if field(t, s, "blockedForUnavailable") == nil || s["blockedForUnavailable"] == "" {
		t.Error("AC3 requires a reason for unavailable blockedForMs")
	}
	requireAdvisory(t, s, "status-since-unavailable")
}

func TestSubagentsCountMetaFilesIncludingNestedSpawn(t *testing.T) {
	f := newFixture(t)
	f.registry("nested", "idle", "", instant(), f.ownedPID())
	p := f.transcript("actual", "nested", object{"type": "assistant", "timestamp": stamp(instant()), "message": object{"content": []any{object{"type": "tool_use", "name": "Task", "id": "one-parent-visible-spawn"}}}})
	dir := filepath.Join(filepath.Dir(p), "nested", "subagents")
	for i, depth := range []int{1, 1, 2} {
		f.json(filepath.Join(dir, fmt.Sprintf("agent-%d.meta.json", i)), object{"agentId": fmt.Sprintf("a%d", i), "spawnDepth": depth, "agentType": "Explore"})
	}
	f.write(filepath.Join(dir, "agent-without-meta.jsonl"), []byte("{}\n"))
	f.write(filepath.Join(dir, "not-meta.txt"), []byte("ignored"))
	s := session(t, f.sessions(), "nested")
	tr := obj(t, field(t, s, "transcript"))
	eq(t, field(t, tr, "subagents"), float64(3), "AC4 includes spawnDepth 2 by file")
	eq(t, field(t, tr, "subagentsKnown"), true, "meta directory known")
}

func TestMissingSubagentDirectoryIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.registry("no-meta", "idle", "", instant(), f.ownedPID())
	f.transcript("actual", "no-meta", object{"type": "assistant", "timestamp": stamp(instant())})
	tr := obj(t, session(t, f.sessions(), "no-meta")["transcript"])
	eq(t, field(t, tr, "subagents"), nil, "AC4 absent directory is null")
	eq(t, field(t, tr, "subagentsKnown"), false, "unknown count")
}

func TestAPIErrorSignalNotRecoveredToolError(t *testing.T) {
	for _, apiCount := range []int{0, 2} {
		t.Run(strconv.Itoa(apiCount), func(t *testing.T) {
			f := newFixture(t)
			f.registry("errors", "idle", "", instant(), f.ownedPID())
			records := []object{{"type": "user", "timestamp": stamp(instant()), "message": object{"content": []any{object{"type": "tool_result", "is_error": true, "content": "recovered failure"}}}}, {"type": "assistant", "isApiErrorMessage": false}, {"type": "system", "is_error": true}}
			for i := 0; i < apiCount; i++ {
				records = append(records, object{"type": "assistant", "isApiErrorMessage": true, "timestamp": stamp(instant())})
			}
			f.transcript("actual", "errors", records...)
			tr := obj(t, session(t, f.sessions(), "errors")["transcript"])
			eq(t, field(t, tr, "apiErrors"), float64(apiCount), "AC5 counts only isApiErrorMessage true")
		})
	}
}

func TestColorLastAgentColorIgnoresSubagentColor(t *testing.T) {
	f := newFixture(t)
	f.registry("colors", "idle", "", instant(), f.ownedPID())
	f.transcript("actual", "colors", object{"type": "agent-color", "agentColor": "red"}, object{"type": "agent-color", "agentColor": "green"}, object{"type": "assistant", "timestamp": stamp(instant()), "color": "blue", "message": object{"content": []any{object{"type": "tool_use", "name": "Task", "input": object{"color": "purple"}}}}})
	tr := obj(t, session(t, f.sessions(), "colors")["transcript"])
	eq(t, field(t, tr, "agentColor"), "green", "AC6 last agentColor")
	eq(t, field(t, tr, "colorFound"), true, "colorFound")
}

func TestTranscriptGlobSurvivesCollidingSlugDirectory(t *testing.T) {
	f := newFixture(t)
	_, r := f.registry("glob-target", "idle", "", instant(), f.ownedPID())
	slug := strings.NewReplacer("/", "-", "_", "-").Replace(r["cwd"].(string))
	f.mkdir(filepath.Join(f.home, ".claude", "projects", slug))
	f.transcript(slug, "different-session", object{"type": "agent-color", "agentColor": "red"})
	want := f.transcript("parent-repository-collapse", "glob-target", object{"type": "agent-color", "agentColor": "cyan"}, object{"timestamp": stamp(instant())})
	tr := obj(t, session(t, f.sessions(), "glob-target")["transcript"])
	eq(t, field(t, tr, "path"), want, "AC7 exact session id glob")
	eq(t, field(t, tr, "agentColor"), "cyan", "never use colliding slug's other transcript")
}

func TestColorOutsideTailHasNoOpinion(t *testing.T) {
	f := newFixture(t)
	f.registry("tail", "idle", "", instant(), f.ownedPID())
	records := []object{{"type": "agent-color", "agentColor": "magenta"}}
	// 3 MiB puts color beyond the 200 KiB production tail and a modest bounded fallback.
	for i := 0; i < 3072; i++ {
		records = append(records, object{"type": "assistant", "timestamp": stamp(instant()), "padding": strings.Repeat("x", 1024)})
	}
	f.transcript("actual", "tail", records...)
	s := session(t, f.sessions(), "tail")
	tr := obj(t, field(t, s, "transcript"))
	if color := field(t, tr, "agentColor"); color != nil && color != "" {
		t.Errorf("AC12 color beyond bounded tail has opinion: %#v", color)
	}
	eq(t, field(t, tr, "colorFound"), false, "no color found")
	eq(t, field(t, tr, "apiErrors"), float64(0), "missing color is not API error")
	eq(t, field(t, s, "live"), true, "color miss does not degrade liveness")
}

func TestTimestampBehindUntimestampedTailIsAdvised(t *testing.T) {
	f := newFixture(t)
	f.registry("timing-tail", "idle", "", instant(), f.ownedPID())
	records := []object{{"type": "assistant", "timestamp": stamp(instant())}}
	for i := 0; i < 3072; i++ {
		records = append(records, object{"type": "progress", "padding": strings.Repeat("x", 1024)})
	}
	f.transcript("actual", "timing-tail", records...)
	s := session(t, f.sessions(), "timing-tail")
	eq(t, field(t, s, "lastActivityAt"), nil, "bounded timing tail cannot invent timestamp")
	requireAdvisory(t, s, "activity-since-truncated")
}

func TestBackgroundJobsStateTimingAndTerminalFiltering(t *testing.T) {
	f := newFixture(t)
	updated := instant().Add(-time.Hour)
	for _, state := range []string{"blocked", "working", "done", "failed"} {
		p := f.transcript("jobs-own-thread", state+"-thread", object{"type": "assistant", "timestamp": stamp(updated), "sessionKind": "bg"})
		f.job(state, state, state, updated, p)
	}
	f.agents(backgroundAgent("blocked", "blocked", "blocked"), backgroundAgent("working", "working", "working"))
	snap := f.sessions()
	eq(t, ids(t, snap), []string{"blocked", "working"}, "AC1 terminal jobs excluded")
	s := session(t, snap, "blocked")
	eq(t, field(t, s, "kind"), "background", "fact13 kind")
	eq(t, field(t, s, "status"), "blocked", "blocked stays distinct")
	eq(t, field(t, s, "waitReason"), "", "AC2 blocked not waiting")
	for _, k := range []string{"pid", "tmux", "procAlive", "paneAlive"} {
		eq(t, field(t, s, k), nil, k)
	}
	checkTime(t, s, updated, true)
	eq(t, parsedTime(t, field(t, s, "lastActivityAt")), updated, "job own transcript timing")
	eq(t, field(t, s, "needs"), "inspect fixture output", "job needs")
	eq(t, field(t, s, "statusSource"), "jobs", "authoritative job source")
	eq(t, hasAdvisory(t, s, "status-since-record-later-than-activity"), false, "matching transcript has no late-record advisory")
	all := f.sessions("--include-dead")
	for _, state := range []string{"done", "failed"} {
		j := session(t, all, state)
		eq(t, field(t, j, "live"), false, "terminal job dead")
		eq(t, field(t, j, "liveReason"), "job-terminal", "terminal reason")
		eq(t, field(t, j, "waitReason"), "", "terminal waitReason")
	}
}

func TestLateJobRecordPublishesTimingBound(t *testing.T) {
	f := newFixture(t)
	updated := instant().Add(-time.Hour)
	activity := updated.Add(-5 * 24 * time.Hour)
	p := f.transcript("own", "different-thread-id", object{"type": "assistant", "timestamp": stamp(activity)}, object{"type": "progress"})
	f.job("late", "late-job", "blocked", updated, p)
	s := session(t, f.sessions(), "late-job")
	checkTime(t, s, updated, true)
	eq(t, parsedTime(t, field(t, s, "lastActivityAt")), activity, "timestamp before untimestamped final line")
	eq(t, parsedTime(t, field(t, s, "statusSinceLowerBound")), activity, "AC3 lower bound is transcript activity")
	requireAdvisory(t, s, "status-since-record-later-than-activity")
}

func TestMissingJobTranscriptNeverFallsBackToGlob(t *testing.T) {
	f := newFixture(t)
	id := "missing-job"
	f.transcript("glob-decoy", id, object{"timestamp": stamp(instant()), "type": "agent-color", "agentColor": "red"})
	f.job("missing", id, "blocked", instant().Add(-time.Hour), filepath.Join(f.home, "missing-own-thread.jsonl"))
	f.agents(backgroundAgent("missing", id, "blocked"))
	s := session(t, f.sessions("--include-dead"), id)
	eq(t, field(t, s, "transcript"), nil, "AC7 missing linkScanPath never uses glob")
	eq(t, field(t, s, "lastActivityAt"), nil, "missing own thread activity")
	requireAdvisory(t, s, "transcript-absent")
}

func TestMissingJobTranscriptUsesEvidenceAndRetention(t *testing.T) {
	// The D1 correction explicitly adopts §3.5. Rule 4 says UNKNOWN, not
	// dead, beyond retention; its normative default is 30 days, not 30.03.
	for _, tc := range []struct {
		name, state, reason, advisory string
		age                           time.Duration
		listed, reaped, tempoIdle     bool
		retentionDays                 int
		live                          any
	}{
		{name: "recent-listed", age: time.Hour, listed: true, live: true, reason: "job-non-terminal-corroborated"},
		{name: "recent-unlisted", age: time.Hour, reason: "job-non-terminal-uncorroborated", advisory: "background-gate-disagreement"},
		{name: "before-default-retention", age: 30*24*time.Hour - time.Hour, listed: true, live: true, reason: "job-non-terminal-corroborated"},
		{name: "after-30-days-before-30-point-03", age: 30*24*time.Hour + 10*time.Minute, listed: true, reason: "job-beyond-transcript-retention", advisory: "beyond-transcript-retention"},
		{name: "old-unlisted", age: 35 * 24 * time.Hour, reason: "job-beyond-transcript-retention", advisory: "beyond-transcript-retention"},
		{name: "configured-longer-retention", age: 35 * 24 * time.Hour, retentionDays: 40, listed: true, live: true, reason: "job-non-terminal-corroborated"},
		{name: "configured-shorter-retention", age: 2 * 24 * time.Hour, retentionDays: 1, listed: true, reason: "job-beyond-transcript-retention", advisory: "beyond-transcript-retention"},
		{name: "terminal-wins", state: "done", age: 35 * 24 * time.Hour, listed: true, live: false, reason: "job-terminal"},
		{name: "reaped-wins", age: 35 * 24 * time.Hour, listed: true, reaped: true, live: false, reason: "job-reaped", advisory: "job-reaped"},
		{name: "tempo-disagreement-wins", age: 35 * 24 * time.Hour, listed: true, tempoIdle: true, reason: "job-tempo-disagrees", advisory: "tempo-state-disagreement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.retentionDays != 0 {
				f.json(filepath.Join(f.home, ".claude", "settings.json"), object{"cleanupPeriodDays": tc.retentionDays})
			}
			state := tc.state
			if state == "" {
				state = "blocked"
			}
			updated := instant().Add(-tc.age)
			path, r := f.job("missing", "missing-job", state, updated, filepath.Join(f.home, "missing.jsonl"))
			if tc.reaped {
				r["reapedMidWorkAt"] = stamp(updated)
			}
			if tc.tempoIdle {
				r["tempo"] = "idle"
			}
			f.json(path, r)
			if tc.listed {
				f.agents(backgroundAgent("missing", "missing-job", state))
			}
			s := session(t, f.sessions("--include-dead"), "missing-job")
			eq(t, field(t, s, "live"), tc.live, "§3.5 evidence-based liveness")
			eq(t, field(t, s, "liveReason"), tc.reason, "first matching liveness rule")
			if tc.advisory != "" {
				requireAdvisory(t, s, tc.advisory)
			}
			checkTime(t, s, updated, state == "blocked")
			wantIDs := []string{"missing-job"}
			if tc.live == false {
				wantIDs = []string{}
			}
			eq(t, ids(t, f.sessions()), wantIDs, "only positive evidence of death excludes a job")
		})
	}
}

func TestNoTranscriptWorkingJobUsesRemainingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reason, advisory string
		age                    time.Duration
		listed                 bool
		live                   any
	}{
		{"listed-old-record", "job-non-terminal-corroborated", "", 5 * 24 * time.Hour, true, true},
		{"unlisted-old-record", "job-non-terminal-uncorroborated", "background-gate-disagreement", 5 * 24 * time.Hour, false, nil},
		{"retention-still-applies", "job-beyond-transcript-retention", "beyond-transcript-retention", 35 * 24 * time.Hour, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			updated := instant().Add(-tc.age)
			activity := instant()
			path := f.transcript("own", "working-thread", object{"timestamp": stamp(activity), "type": "agent-color", "agentColor": "green"})
			f.job("working", "working-job", "working", updated, path)
			if tc.listed {
				f.agents(backgroundAgent("working", "working-job", "working"))
			}
			// Positive control: this target is readable and supplies recent timing.
			enriched := session(t, f.sessions("--enrich"), "working-job")
			eq(t, parsedTime(t, field(t, enriched, "lastActivityAt")), activity, "enriched timing control")
			if tc.listed {
				eq(t, field(t, enriched, "live"), true, "old milestone record is not stale transcript activity")
			}
			s := session(t, f.sessions("--no-transcript"), "working-job")
			for _, key := range []string{"transcript", "lastActivityAt", "statusSinceLowerBound"} {
				eq(t, field(t, s, key), nil, "no-transcript "+key)
			}
			eq(t, field(t, s, "transcriptCollected"), false, "no transcript collection")
			requireAdvisory(t, s, "status-since-unverifiable")
			eq(t, hasAdvisory(t, s, "background-working-stale"), false, "rule 5 cannot substitute record age for activity")
			eq(t, field(t, s, "live"), tc.live, "remaining evidence decides liveness")
			eq(t, field(t, s, "liveReason"), tc.reason, "remaining evidence reason")
			if tc.advisory != "" {
				requireAdvisory(t, s, tc.advisory)
			}
			checkTime(t, s, updated, false)
		})
	}
}

func TestBackgroundDirectoryUsesRecordSessionID(t *testing.T) {
	f := newFixture(t)
	p := f.transcript("own", "thread", object{"timestamp": stamp(instant())})
	_, r := f.job("arbitrary-dir", "actual-session-id", "blocked", instant(), p)
	r["daemonShort"] = "different-short"
	f.json(filepath.Join(f.home, ".claude", "jobs", "arbitrary-dir", "state.json"), r)
	f.mkdir(filepath.Join(f.home, ".claude", "jobs", "recordless"))
	f.json(filepath.Join(f.home, ".claude", "jobs", "pins.json"), object{})
	s := session(t, f.sessions(), "actual-session-id")
	requireAdvisory(t, s, "shortid-mismatch")
	eq(t, field(t, s, "shortId"), "different-short", "record short id, not directory assumption")
}

func TestMalformedRecordDoesNotBlankSet(t *testing.T) {
	f := newFixture(t)
	f.registry("good", "idle", "", instant(), f.ownedPID())
	f.write(filepath.Join(f.home, ".claude", "sessions", "999999.json"), []byte("{bad JSON\n"))
	f.write(filepath.Join(f.home, ".claude", "jobs", "broken", "state.json"), []byte("{\n"))
	f.write(filepath.Join(f.home, ".claude", "sessions", "unrelated.key"), []byte("not JSON"))
	snap := f.sessions()
	eq(t, ids(t, snap), []string{"good"}, "malformed siblings do not erase good session")
	eq(t, field(t, session(t, snap, "good"), "live"), true, "good liveness")
	sources := obj(t, snap["sources"])
	eq(t, sourceCounter(t, sourceByID(t, sources, "registry"), "parseErrors"), float64(1), "registry malformed diagnosed")
	eq(t, sourceCounter(t, sourceByID(t, sources, "jobs"), "parseErrors"), float64(1), "jobs malformed diagnosed")
}

func TestHangingClaudeCannotDegradePIDLiveness(t *testing.T) {
	f := newFixture(t)
	alive := f.ownedPID()
	dead := f.deadPID()
	f.registry("live", "idle", "", instant(), alive)
	f.registry("dead", "idle", "", instant(), dead)
	path, row := f.registry("live-pane", "idle", "", instant(), f.ownedPID())
	row["tmux"] = "bb-session:@51.%107"
	f.json(path, row)
	f.panes("bb-session\t@51\t%107\t0")
	f.write(filepath.Join(f.home, "hang-claude"), nil)
	start := time.Now()
	snap := f.sessions("--refresh", "--timeout=300ms", "--include-dead")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("hung corroboration took %s despite 300ms bound", d)
	}
	eq(t, field(t, session(t, snap, "live"), "live"), true, "hung CLI does not degrade positive pid evidence")
	eq(t, field(t, session(t, snap, "dead"), "live"), false, "hung CLI does not override dead pid")
	paneSession := session(t, snap, "live-pane")
	eq(t, field(t, paneSession, "live"), true, "hung CLI does not degrade positive pid and pane evidence")
	eq(t, field(t, paneSession, "paneAlive"), true, "pane gate survives hung corroboration")
	eq(t, field(t, paneSession, "liveReason"), "proc+pane", "both gates remain authoritative")
	if b, err := os.ReadFile(filepath.Join(f.home, "claude-pids")); err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("wedge fake was never invoked: %v", err)
	}
	eq(t, field(t, sourceByID(t, obj(t, snap["sources"]), "agents-cli"), "ok"), false, "hung source diagnosed")
}

func TestCLIOnlyInteractiveHasUnknownTime(t *testing.T) {
	f := newFixture(t)
	pid := f.ownedPID()
	row := agent("cli-only", pid, "waiting")
	row["waitingFor"] = "permission prompt"
	f.agents(row)
	s := session(t, f.sessions("--refresh"), "cli-only")
	eq(t, field(t, s, "live"), true, "CLI only uses owned pid")
	eq(t, field(t, s, "waitReason"), "permission-prompt", "CLI waiting reason")
	for _, k := range []string{"statusSince", "statusAgeMs", "blockedForMs"} {
		eq(t, field(t, s, k), nil, k)
	}
	requireAdvisory(t, s, "status-since-unavailable")
}

func TestCLIFlagsAndEmptySet(t *testing.T) {
	f := newFixture(t)
	eq(t, ids(t, f.sessions()), []string{}, "empty readable fixture is success")
	f.registry("idle", "idle", "", instant(), f.ownedPID())
	f.registry("waiting", "waiting", "input needed", instant(), f.ownedPID())
	p := f.transcript("own", "job-thread", object{"timestamp": stamp(instant())})
	f.job("job", "job", "blocked", instant(), p)
	eq(t, ids(t, f.sessions("--waiting")), []string{"job", "waiting"}, "waiting includes background attention")
	eq(t, ids(t, f.sessions("--kind=background")), []string{"job"}, "kind flag")
	eq(t, ids(t, f.sessions("--cwd-prefix="+filepath.Join(f.home, "work", "jobs"))), []string{"job"}, "cwd prefix flag")
	for _, v := range list(t, f.sessions("--no-transcript")["sessions"]) {
		s := obj(t, v)
		eq(t, field(t, s, "transcriptCollected"), false, "explicit enrichment off")
		eq(t, field(t, s, "lastActivityAt"), nil, "no timing transcript read")
	}
	_, _, code := f.run("sessions", "--definitely-invalid-flag")
	eq(t, code, 2, "usage exit")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type running struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	done    chan struct{}
	stderr  lockedBuffer
	waitErr error
}

func (f *fixture) start(args ...string) *running {
	f.t.Helper()
	r := &running{cmd: exec.Command(f.binary, args...), lines: make(chan []byte, 256), done: make(chan struct{})}
	r.cmd.Env = f.env
	r.cmd.Dir = f.home
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.cmd.Stderr = &r.stderr
	r.cmd.WaitDelay = time.Second
	var err error
	r.stdin, err = r.cmd.StdinPipe()
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		f.t.Fatal(err)
	}
	if err = r.cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			select {
			case r.lines <- append([]byte(nil), scanner.Bytes()...):
			case <-r.done:
				return
			}
		}
		close(r.lines)
	}()
	go func() { r.waitErr = r.cmd.Wait(); close(r.done) }()
	f.t.Cleanup(func() {
		_ = r.stdin.Close()
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			<-r.done
		}
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	})
	return r
}

type socketClient struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
}

func (f *fixture) connect() *socketClient {
	f.t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", filepath.Join(f.socket, "observer.sock"), 100*time.Millisecond)
		if err == nil {
			f.t.Cleanup(func() { _ = c.Close() })
			return &socketClient{f.t, c, bufio.NewReader(c)}
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("socket did not become reachable: %v", last)
	return nil
}
func (s *socketClient) send(v object) {
	s.t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(s.conn).Encode(v); err != nil {
		s.t.Fatal(err)
	}
}
func (s *socketClient) read() object {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	b, err := s.reader.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("socket frame: %v", err)
	}
	return decode(s.t, b)
}
func (s *socketClient) until(kind, req string) object {
	s.t.Helper()
	for i := 0; i < 50; i++ {
		r := s.read()
		if r["type"] == kind && (req == "" || r["reqId"] == req) {
			return r
		}
		if r["type"] == "error" {
			s.t.Fatalf("socket error: %v", r)
		}
	}
	s.t.Fatal("too many unrelated socket frames")
	return nil
}

func snapshotPayload(t *testing.T, frame object) object {
	t.Helper()
	eq(t, field(t, frame, "type"), "snapshot", "snapshot frame type")
	eq(t, field(t, frame, "observedAt"), nil, "snapshot timing lives in nested envelope")
	eq(t, field(t, frame, "sources"), nil, "snapshot sources live in nested envelope")
	payload := obj(t, field(t, frame, "snapshot"))
	for _, key := range []string{"schema", "tt", "observedAt", "counts", "sources", "warnings", "sessions"} {
		field(t, payload, key)
	}
	eq(t, field(t, payload, "schema"), float64(1), "nested envelope schema")
	return payload
}

func (f *fixture) observe() (*running, *socketClient) {
	f.t.Helper()
	r := f.start("observe", "--socket-dir="+f.socket, "--status-line=never", "--timeout=500ms")
	s := f.connect()
	hello := s.until("hello", "")
	eq(f.t, field(f.t, hello, "protocol"), float64(1), "socket protocol")
	eq(f.t, field(f.t, hello, "notCapable"), []any{"time-transitions"}, "event-only capability declaration")
	// §5.3 requires the FIRST frame after hello, even for an empty fixture.
	initial := s.read()
	snapshotPayload(f.t, initial)
	eq(f.t, field(f.t, initial, "reqId"), nil, "initial snapshot is unsolicited")
	return r, s
}

type mcpClient struct {
	t       *testing.T
	process *running
	id      int
}

func (f *fixture) mcp(local bool) *mcpClient {
	f.t.Helper()
	args := []string{"mcp", "--socket-dir=" + f.socket, "--timeout=500ms", "--publish-job-text"}
	if local {
		args = append(args, "--local")
	}
	m := &mcpClient{t: f.t, process: f.start(args...)}
	init := m.call("initialize", object{"protocolVersion": "2025-06-18", "capabilities": object{}, "clientInfo": object{"name": "tt-blackbox", "version": "1"}})
	eq(f.t, field(f.t, init, "protocolVersion"), "2025-06-18", "MCP initialize")
	m.send(object{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return m
}
func (m *mcpClient) send(v object) {
	m.t.Helper()
	if err := json.NewEncoder(m.process.stdin).Encode(v); err != nil {
		m.t.Fatal(err)
	}
}
func (m *mcpClient) call(method string, params object) object {
	m.t.Helper()
	m.id++
	m.send(object{"jsonrpc": "2.0", "id": m.id, "method": method, "params": params})
	deadline := time.NewTimer(7 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case b, ok := <-m.process.lines:
			if !ok {
				m.t.Fatalf("MCP exited: %s", m.process.stderr.String())
			}
			r := decode(m.t, b)
			if r["id"] != float64(m.id) {
				continue
			}
			if e, ok := r["error"]; ok {
				m.t.Fatalf("MCP %s JSON-RPC error: %v", method, e)
			}
			return obj(m.t, field(m.t, r, "result"))
		case <-deadline.C:
			m.t.Fatalf("MCP %s timed out: %s", method, m.process.stderr.String())
		}
	}
}
func (m *mcpClient) tool(name string, args object) object {
	m.t.Helper()
	r := m.call("tools/call", object{"name": name, "arguments": args})
	if r["isError"] == true {
		m.t.Fatalf("MCP tool %s failed: %v", name, r)
	}
	if len(list(m.t, field(m.t, r, "content"))) == 0 {
		m.t.Error("MCP requires human content beside structuredContent")
	}
	return obj(m.t, field(m.t, r, "structuredContent"))
}

func TestSocketPushesInPlaceRegistryChange(t *testing.T) {
	f := newFixture(t)
	path, r := f.registry("watch", "idle", "", instant(), f.ownedPID())
	_, s := f.observe()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	r["status"] = "waiting"
	r["waitingFor"] = "permission prompt"
	r["statusUpdatedAt"] = instant().UnixMilli()
	start := time.Now()
	f.json(path, r)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("invalid fixture: in-place write replaced inode")
	}
	event := s.until("session_changed", "")
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("AC10 in-place event latency %s exceeds ~1s allowance", elapsed)
	}
	eq(t, field(t, event, "sessionId"), "watch", "event identity")
	row := obj(t, field(t, event, "session"))
	eq(t, field(t, row, "status"), "waiting", "event contains full updated session")
	eq(t, field(t, row, "waitReason"), "permission-prompt", "pushed reason")
	s.send(object{"type": "ping", "reqId": "ping-test"})
	s.until("pong", "ping-test")
}

func TestSocketPushesRenameJobTerminalAndResume(t *testing.T) {
	f := newFixture(t)
	activity := instant()
	transcript := f.transcript("own", "thread", object{"timestamp": stamp(activity)})
	path, r := f.job("watch-job", "watch-job", "blocked", activity, transcript)
	_, s := f.observe()
	for i := 0; i < 2; i++ {
		r["state"] = "done"
		r["tempo"] = "done"
		r["updatedAt"] = stamp(instant())
		tmp := path + ".publish"
		f.json(tmp, r)
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(before, after) {
			t.Fatal("invalid fixture: rename preserved inode")
		}
		event := s.until("session_removed", "")
		eq(t, field(t, event, "sessionId"), "watch-job", "terminal removed id")
		eq(t, field(t, event, "reason"), "job-terminal", "terminal reason")
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("AC10 rename event took %s", elapsed)
		}
		r["state"] = "blocked"
		r["tempo"] = "blocked"
		r["updatedAt"] = stamp(instant())
		f.json(tmp, r)
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		add := s.until("session_added", "")
		eq(t, field(t, obj(t, field(t, add, "session")), "sessionId"), "watch-job", "resume remains watched")
	}
}

func TestSocketDiscoversFirstJobRecord(t *testing.T) {
	f := newFixture(t)
	f.mkdir(filepath.Join(f.home, ".claude", "jobs", "first"))
	_, s := f.observe()
	updated := instant()
	p := f.transcript("own", "first-thread", object{"timestamp": stamp(updated)})
	// Build the publication outside the watched job directory, then rename into it.
	r := object{"sessionId": "first-session", "daemonShort": "first", "state": "blocked", "tempo": "blocked", "updatedAt": stamp(updated), "createdAt": stamp(updated.Add(-time.Hour)), "linkScanPath": p}
	tmp := filepath.Join(f.home, "new-job.json")
	f.json(tmp, r)
	start := time.Now()
	if err := os.Rename(tmp, filepath.Join(f.home, ".claude", "jobs", "first", "state.json")); err != nil {
		t.Fatal(err)
	}
	event := s.until("session_added", "")
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("AC10 first-record event took %s", elapsed)
	}
	row := obj(t, field(t, event, "session"))
	eq(t, field(t, row, "sessionId"), "first-session", "new job discovered")
	eq(t, parsedTime(t, field(t, row, "lastActivityAt")), updated, "new job initial timing read")
}

func TestSocketJobTranscriptRepointInvalidatesOldTiming(t *testing.T) {
	f := newFixture(t)
	updated := instant().Add(-time.Hour)
	oldActivity := updated.Add(-5 * 24 * time.Hour)
	oldPath := f.transcript("old", "old-thread", object{"timestamp": stamp(oldActivity)})
	path, record := f.job("repoint", "repoint-job", "blocked", updated, oldPath)
	_, socket := f.observe()
	socket.send(object{"type": "snapshot", "reqId": "before-repoint", "enrich": true})
	before := session(t, snapshotPayload(t, socket.until("snapshot", "before-repoint")), "repoint-job")
	eq(t, parsedTime(t, field(t, before, "statusSinceLowerBound")), oldActivity, "old timing bound exists before repoint")
	requireAdvisory(t, before, "status-since-record-later-than-activity")
	newActivity := instant()
	newPath := f.transcript("new", "new-thread", object{"timestamp": stamp(newActivity)})
	// The new thread is complete before publication; no append will rescue it.
	record["linkScanPath"] = newPath
	record["updatedAt"] = stamp(newActivity)
	tmp := path + ".publish"
	f.json(tmp, record)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	row := obj(t, field(t, socket.until("session_changed", ""), "session"))
	eq(t, field(t, row, "sessionId"), "repoint-job", "repoint event identity")
	eq(t, parsedTime(t, field(t, row, "lastActivityAt")), newActivity, "new thread timing read before publish")
	eq(t, field(t, row, "statusSinceLowerBound"), nil, "old thread bound invalidated")
	eq(t, hasAdvisory(t, row, "status-since-record-later-than-activity"), false, "old thread advisory invalidated")
	checkTime(t, row, newActivity, true)
}

func TestMCPToolContracts(t *testing.T) {
	f := newFixture(t)
	f.registry("mcp-session", "waiting", "input needed", instant(), f.ownedPID())
	m := f.mcp(true)
	tools := m.call("tools/list", object{})
	found := map[string]bool{}
	for _, v := range list(t, field(t, tools, "tools")) {
		tool := obj(t, v)
		name := fmt.Sprint(tool["name"])
		found[name] = true
		a := obj(t, field(t, tool, "annotations"))
		for k, want := range map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false} {
			eq(t, field(t, a, k), want, name+" "+k)
		}
	}
	for _, name := range []string{"list_sessions", "get_session", "session_counts", "describe_sources", "doctor"} {
		if !found[name] {
			t.Errorf("architecture 5.4 requires tool %q (README/help disagree on doctor)", name)
		}
	}
	snap := m.tool("list_sessions", object{"includeTranscript": true, "includeDead": false, "includeUnknownLiveness": true, "limit": 200})
	eq(t, ids(t, snap), []string{"mcp-session"}, "MCP list")
	get := m.tool("get_session", object{"sessionId": "mcp-session"})
	eq(t, field(t, obj(t, get["session"]), "sessionId"), "mcp-session", "MCP get")
	counts := m.tool("session_counts", object{})
	eq(t, field(t, obj(t, counts["counts"]), "attention"), float64(1), "MCP attention count")
	m.tool("describe_sources", object{})
	if found["doctor"] {
		m.tool("doctor", object{})
	}
}

func normalizeSession(t *testing.T, s object) object {
	t.Helper()
	b, _ := json.Marshal(s)
	n := decode(t, b)
	// Independently validate derived durations before rebasing only clock values.
	if field(t, s, "statusSince") != nil {
		since := parsedTime(t, s["statusSince"])
		checkTime(t, s, since, s["attention"] == true)
		n["statusAgeMs"] = float64(0)
		if s["attention"] == true {
			n["blockedForMs"] = float64(0)
		}
	}
	if field(t, s, "recordUpdatedAt") != nil && field(t, s, "recordAgeMs") != nil {
		observed := parsedTime(t, field(t, s, "observedAt"))
		updated := parsedTime(t, s["recordUpdatedAt"])
		eq(t, s["recordAgeMs"], float64(observed.Sub(updated).Milliseconds()), "recordAgeMs derives from observedAt")
		n["recordAgeMs"] = float64(0)
	}
	n["observedAt"] = "REFERENCE-INSTANT"
	return n
}

func compareSurface(t *testing.T, label string, a, b object) {
	t.Helper()
	eq(t, ids(t, b), ids(t, a), label+" identities")
	eq(t, field(t, b, "counts"), field(t, a, "counts"), label+" counts")
	for _, id := range ids(t, a) {
		left := normalizeSession(t, session(t, a, id))
		right := normalizeSession(t, session(t, b, id))
		if !reflect.DeepEqual(left, right) {
			keys := []string{}
			for k, v := range left {
				if !reflect.DeepEqual(v, right[k]) {
					keys = append(keys, k)
				}
			}
			for k := range right {
				if _, ok := left[k]; !ok {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				t.Errorf("AC11 %s %s.%s: CLI=%#v other=%#v", label, id, k, left[k], right[k])
			}
		}
	}
	// Public subprocesses expose no common injected clock. Validate all ages
	// above, then compare canonical JSON including session order and every
	// envelope field except the separately sampled source diagnostics.
	normalize := func(snapshot object) object {
		n := object{}
		for key, value := range snapshot {
			n[key] = value
		}
		parsedTime(t, field(t, snapshot, "observedAt"))
		n["observedAt"] = "REFERENCE-INSTANT"
		obj(t, field(t, snapshot, "sources"))
		delete(n, "sources")
		rows := []any{}
		for _, value := range list(t, field(t, snapshot, "sessions")) {
			rows = append(rows, normalizeSession(t, obj(t, value)))
		}
		n["sessions"] = rows
		return n
	}
	left, right := normalize(a), normalize(b)
	leftBytes, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		for key, value := range left {
			if !reflect.DeepEqual(value, right[key]) {
				t.Errorf("AC11 %s normalized envelope field %q differs", label, key)
			}
		}
		for key := range right {
			if _, ok := left[key]; !ok {
				t.Errorf("AC11 %s unexpected envelope field %q", label, key)
			}
		}
	}
}

func TestCLIAndSocketAndMCPAgree(t *testing.T) {
	f := newFixture(t)
	since := instant().Add(-time.Hour)
	pid := f.ownedPID()
	registryPath, registryRow := f.registry("same-interactive", "waiting", "input needed", since, pid)
	registryRow["tmux"] = "bb-session:@51.%107"
	f.json(registryPath, registryRow)
	f.panes("bb-session\t@51\t%107\t0")
	p := f.transcript("actual", "same-interactive", object{"type": "agent-color", "agentColor": "green"}, object{"timestamp": stamp(since)})
	f.mkdir(filepath.Join(filepath.Dir(p), "same-interactive", "subagents"))
	jobPath := f.transcript("own", "same-thread", object{"timestamp": stamp(since)})
	f.job("same-job", "same-job", "blocked", since, jobPath)
	f.agents(agent("same-interactive", pid, "waiting"), backgroundAgent("same-job", "same-job", "blocked"))
	cli := f.sessions("--enrich")
	_, socket := f.observe()
	socket.send(object{"type": "snapshot", "reqId": "equal-data", "enrich": true})
	stream := snapshotPayload(t, socket.until("snapshot", "equal-data"))
	m := f.mcp(false)
	attached := m.tool("list_sessions", object{"includeTranscript": true, "includeDead": false, "includeUnknownLiveness": true, "limit": 200, "refresh": false})
	local := f.mcp(true).tool("list_sessions", object{"includeTranscript": true, "includeDead": false, "includeUnknownLiveness": true, "limit": 200, "refresh": false})
	compareSurface(t, "socket", cli, stream)
	compareSurface(t, "socket-attached MCP", cli, attached)
	compareSurface(t, "local MCP", cli, local)
	for _, snap := range []object{cli, stream, attached, local} {
		interactive := session(t, snap, "same-interactive")
		eq(t, field(t, interactive, "live"), true, "every surface reports a live pid with an existing pane")
		eq(t, field(t, interactive, "paneAlive"), true, "every surface retains positive pane evidence")
		eq(t, field(t, interactive, "liveReason"), "proc+pane", "every surface uses both gates")
		eq(t, field(t, snap, "schema"), float64(1), "envelope schema")
		field(t, snap, "sources")
		field(t, snap, "warnings")
		field(t, snap, "tt")
	}
}

func TestDoctorReportsDegradedFixture(t *testing.T) {
	f := newFixture(t)
	f.registry("doctor-good", "idle", "", instant(), f.ownedPID())
	f.write(filepath.Join(f.home, ".claude", "sessions", "bad.json"), []byte("{malformed\n"))
	out, stderr, code := f.run("sessions", "--doctor", "--json")
	if code != 3 && code != 4 {
		t.Errorf("doctor must warn/fail on malformed fixture, exit=%d stderr=%s", code, stderr)
	}
	report := decode(t, out)
	checks := list(t, field(t, report, "checks"))
	if len(checks) == 0 {
		t.Fatal("doctor returned no checks")
	}
	unhealthy := false
	for _, v := range checks {
		check := obj(t, v)
		if check["level"] == "warn" || check["level"] == "fail" || check["status"] == "warn" || check["status"] == "fail" {
			unhealthy = true
		}
	}
	if !unhealthy {
		t.Errorf("doctor has no warning/failure for malformed input: %s", out)
	}
	// Human doctor is also a public surface; its layout is intentionally not asserted.
	human, errout, exit := f.run("sessions", "--doctor")
	if exit != 3 && exit != 4 {
		t.Errorf("human doctor exit=%d", exit)
	}
	if len(bytes.TrimSpace(append(human, errout...))) == 0 {
		t.Error("human doctor produced no diagnosis")
	}
}

type fileEvidence struct {
	Mode   os.FileMode
	Size   int64
	Mtime  int64
	Inode  uint64
	Digest [32]byte
}

func claudeTree(t *testing.T, root string) map[string]fileEvidence {
	t.Helper()
	out := map[string]fileEvidence{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		e := fileEvidence{Mode: info.Mode(), Size: info.Size(), Mtime: info.ModTime().UnixNano()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			e.Inode = st.Ino
		}
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.Digest = sha256.Sum256(b)
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublicSurfacesDoNotModifyClaudeFixtures(t *testing.T) {
	f := newFixture(t)
	pid := f.ownedPID()
	f.registry("read-only", "waiting", "input needed", instant(), pid)
	p := f.transcript("own", "read-only", object{"timestamp": stamp(instant()), "type": "agent-color", "agentColor": "cyan"})
	f.job("read-job", "read-job", "blocked", instant(), p)
	f.agents(agent("read-only", pid, "waiting"), backgroundAgent("read-job", "read-job", "blocked"))
	root := filepath.Join(f.home, ".claude")
	before := claudeTree(t, root)
	f.sessions()
	f.run("sessions", "--doctor", "--json")
	f.mcp(true).tool("list_sessions", object{"includeTranscript": true, "includeDead": true, "limit": 200})
	server, socket := f.observe()
	socket.send(object{"type": "snapshot", "reqId": "containment", "enrich": true})
	socket.until("snapshot", "containment")
	_ = server.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-server.done:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not stop on SIGTERM")
	}
	eq(t, claudeTree(t, root), before, "read-only Claude tree: names, bytes, size, mtime, mode and inode")
}
