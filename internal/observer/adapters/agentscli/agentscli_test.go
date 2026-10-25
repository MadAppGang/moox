package agentscli_test

// The adapter's own contract: the decode against measured output, the spawn
// bound, and §4.3(b)'s cache and lock.
//
// The cross-process cases are the point of this file. They are the only finding
// all four round-2 reviewers raised, and revision 1's verification "passed"
// because it tested the SEQUENTIAL case — so every simultaneity assertion here
// runs N independent Sources against ONE state directory, which is what a
// process is from the lock's point of view: a separate cache reader with no
// shared memory.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/agentscli"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

const fixtureDir = "../../testdata/agents"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// runner is the spawn seam: it answers with fixture bytes and counts how many
// times it was asked. The count IS the assertion in every simultaneity test.
type runner struct {
	payload []byte
	err     error
	// delay holds the "spawn" open, so a second caller is genuinely concurrent
	// rather than merely later.
	delay  time.Duration
	spawns atomic.Int64
}

func (r *runner) run(ctx context.Context) ([]byte, error) {
	r.spawns.Add(1)
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.payload, nil
}

func (r *runner) count() int64 { return r.spawns.Load() }

// claudeRootFor is the Claude tree every Source over a given state directory
// describes. It is DERIVED from the state directory rather than freshly
// allocated per Source, because the cache is scoped to the tree it describes:
// sixteen Sources standing in for sixteen processes on one machine must agree
// about which machine that is, or every cache read is a legitimate miss and the
// simultaneity tests measure nothing. (They did, for one run.)
func claudeRootFor(stateDir string) string { return stateDir + "-claude-root" }

// newSource builds a Source over a temp state directory with the fixture as its
// output. Every knob a test might vary is a parameter of cfg so that no test
// depends on a production default it did not name.
func newSource(t *testing.T, stateDir string, r *runner, mutate func(*agentscli.Config)) *agentscli.Source {
	t.Helper()
	cfg := agentscli.Config{
		StateDir:     stateDir,
		ClaudeRoot:   claudeRootFor(stateDir),
		SpawnTimeout: 2 * time.Second,
		StaleAfterMs: 30_000,
		LockWait:     250 * time.Millisecond,
		TT:           "v0-test",
		Run:          r.run,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return agentscli.New(cfg)
}

// ---------------------------------------------------------------------------
// the decode, against output measured today
// ---------------------------------------------------------------------------

func TestDecodesTodaysRealListing(t *testing.T) {
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, t.TempDir(), r, nil)

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok false: %v", res.Err)
	}
	if got := len(res.Entries); got != 39 {
		t.Fatalf("entries = %d, want 39", got)
	}

	var interactive, background int
	for _, e := range res.Entries {
		if e.PID != nil {
			interactive++
			if e.Status == "" {
				t.Errorf("%s: interactive entry with no status", e.SessionID)
			}
			if e.State != "" {
				t.Errorf("%s: interactive entry carries state %q — state and status are different fields", e.SessionID, e.State)
			}
			continue
		}
		background++
		if e.State == "" {
			t.Errorf("%s: background entry with no state", e.SessionID)
		}
		if e.Status != "" {
			t.Errorf("%s: background entry carries status %q", e.SessionID, e.Status)
		}
		if e.ID == "" {
			t.Errorf("%s: background entry with no short id", e.SessionID)
		}
	}
	if interactive != 36 || background != 3 {
		t.Fatalf("interactive/background = %d/%d, want 36/3", interactive, background)
	}

	// startedAt is EPOCH MILLIS on 39/39 measured entries. A shared RFC3339
	// helper would return nil for every one of them, silently, which is exactly
	// the class of defect this assertion exists for.
	for _, e := range res.Entries {
		if e.StartedAt == nil {
			t.Fatalf("%s: startedAt is nil — the epoch-millis decode is broken", e.SessionID)
		}
		if e.StartedAt.Year() < 2020 || e.StartedAt.Year() > 2100 {
			t.Fatalf("%s: startedAt = %s, which is not a plausible instant", e.SessionID, e.StartedAt)
		}
	}

	// Sorted by session id ascending, as the port requires. The vendor's own
	// order is background-first today, which is a fact about today.
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].SessionID > res.Entries[i].SessionID {
			t.Fatalf("entries are not sorted by session id at %d", i)
		}
	}
}

func TestDecodesWaitingForWhenPresent(t *testing.T) {
	r := &runner{payload: fixture(t, "real-37-with-waitingfor.json")}
	src := newSource(t, t.TempDir(), r, nil)

	res, _ := src.List(context.Background())
	var found int
	for _, e := range res.Entries {
		if e.WaitingFor != "" {
			found++
			if e.WaitingFor != "input needed" {
				t.Errorf("waitingFor = %q, want %q", e.WaitingFor, "input needed")
			}
			if e.Status != "waiting" {
				t.Errorf("the waitingFor entry has status %q, want waiting", e.Status)
			}
		}
	}
	if found != 1 {
		t.Fatalf("entries carrying waitingFor = %d, want 1 (§0.3)", found)
	}
}

func TestAnEmptyListingIsASuccessfulAnswer(t *testing.T) {
	r := &runner{payload: fixture(t, "empty.json")}
	src := newSource(t, t.TempDir(), r, nil)

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok false for an empty listing: %v", res.Err)
	}
	if len(res.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(res.Entries))
	}
}

func TestMalformedOutputIsAFailedListingNotAnEmptyOne(t *testing.T) {
	r := &runner{payload: fixture(t, "malformed.json")}
	src := newSource(t, t.TempDir(), r, nil)

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List returned an error; a source failure is a value: %v", err)
	}
	if res.OK {
		t.Fatal("ok true for unparseable output: an empty listing with ok true is " +
			"positive evidence of death for every background job on the machine")
	}
	if res.Err == nil {
		t.Fatal("no Err on a failed listing")
	}
	if len(res.Entries) != 0 {
		t.Fatalf("entries = %d on a failed listing", len(res.Entries))
	}
}

func TestAnEntryWithoutASessionIDIsSkippedNotFatal(t *testing.T) {
	r := &runner{payload: []byte(`[{"pid":1,"kind":"interactive","status":"idle"},
	                               {"sessionId":"aaaa","pid":2,"kind":"interactive","status":"busy"}]`)}
	src := newSource(t, t.TempDir(), r, nil)

	res, _ := src.List(context.Background())
	if !res.OK {
		t.Fatalf("ok false: %v", res.Err)
	}
	if len(res.Entries) != 1 || res.Entries[0].SessionID != "aaaa" {
		t.Fatalf("entries = %+v, want only the one with a join key", res.Entries)
	}
}

// ---------------------------------------------------------------------------
// the spawn: the `hangs` fixture, and a command that is not there
// ---------------------------------------------------------------------------

func TestTheDefaultSpawnTimeoutIsFiveSeconds(t *testing.T) {
	// §4.3(a)'s bound is a documented number, and the tests below deliberately
	// run with a short one so the suite stays fast. Pinning the default here is
	// what stops "we proved the mechanism" from drifting into "we shipped 50 ms".
	if agentscli.DefaultSpawnTimeout != 5*time.Second {
		t.Fatalf("DefaultSpawnTimeout = %s, want 5s", agentscli.DefaultSpawnTimeout)
	}
	if agentscli.DefaultLockWait <= 130*time.Millisecond {
		t.Fatalf("DefaultLockWait = %s, which is not longer than the measured 130ms spawn",
			agentscli.DefaultLockWait)
	}
}

func TestTheHangsFixtureIsKilledAtTheSpawnTimeout(t *testing.T) {
	hangs, err := filepath.Abs(filepath.Join(fixtureDir, "hangs"))
	if err != nil {
		t.Fatal(err)
	}
	src := agentscli.New(agentscli.Config{
		Command:      hangs,
		Args:         []string{},
		StateDir:     t.TempDir(),
		SpawnTimeout: 250 * time.Millisecond,
		StaleAfterMs: 30_000,
		LockWait:     50 * time.Millisecond,
	})

	start := time.Now()
	res, err := src.List(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("List returned an error rather than a degraded answer: %v", err)
	}
	if res.OK {
		t.Fatal("ok true from a child that never exits")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want a wrapped context.DeadlineExceeded so the core can "+
			"report `timeout` rather than a generic failure", res.Err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the spawn took %s: the timeout did not kill the child", elapsed)
	}
	if res.Cache != observer.AgentsCacheMiss {
		t.Fatalf("agentsCache = %q, want %q: the lock was taken and a spawn was attempted",
			res.Cache, observer.AgentsCacheMiss)
	}

	// The lock must not survive a timed-out spawn. A lock left behind would
	// block every other process on the machine until its mtime aged out.
	if _, err := os.Stat(filepath.Join(src.StateDir(), agentscli.LockFileName)); !os.IsNotExist(err) {
		t.Fatalf("the lock file survived a timed-out spawn: %v", err)
	}
	// Nor may a failed spawn publish a cache: the next process must spawn, not
	// read an empty answer.
	if _, err := os.Stat(filepath.Join(src.StateDir(), agentscli.CacheFileName)); !os.IsNotExist(err) {
		t.Fatal("a failed spawn published a cache file")
	}
}

func TestACommandThatIsNotThereIsAFailedListing(t *testing.T) {
	src := agentscli.New(agentscli.Config{
		Command:      filepath.Join(t.TempDir(), "no-such-claude"),
		Args:         agentscli.DefaultArgs(),
		StateDir:     t.TempDir(),
		SpawnTimeout: time.Second,
		StaleAfterMs: 30_000,
	})

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.OK {
		t.Fatal("ok true with claude absent from PATH")
	}
	if res.Err == nil {
		t.Fatal("no Err")
	}
	if errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("an absent command reported as a timeout: %v", res.Err)
	}
}

func TestTheCommandLineIsReportedVerbatim(t *testing.T) {
	src := agentscli.New(agentscli.Config{StateDir: t.TempDir()})
	if got := src.CommandLine(); got != "claude agents --json" {
		t.Fatalf("CommandLine = %q, want %q", got, "claude agents --json")
	}
}

// ---------------------------------------------------------------------------
// §4.3(b) — the cache
// ---------------------------------------------------------------------------

func TestASecondCallHitsTheCacheAndDoesNotSpawn(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, nil)

	first, _ := src.List(context.Background())
	if first.Cache != observer.AgentsCacheMiss || first.Cached {
		t.Fatalf("first call: cache = %q cached = %v, want miss/false", first.Cache, first.Cached)
	}

	second, _ := src.List(context.Background())
	if second.Cache != observer.AgentsCacheHit || !second.Cached {
		t.Fatalf("second call: cache = %q cached = %v, want hit/true", second.Cache, second.Cached)
	}
	if r.count() != 1 {
		t.Fatalf("spawns = %d, want 1", r.count())
	}
	if len(second.Entries) != len(first.Entries) {
		t.Fatalf("the cached listing has %d entries, the spawned one %d",
			len(second.Entries), len(first.Entries))
	}
	// The cached result keeps the OBSERVED instant, not the read instant: a
	// cache that restamped its own answer would report a 30-second-old listing
	// as fresh forever.
	if !second.ObservedAt.Equal(first.ObservedAt) {
		t.Fatalf("cached observedAt = %s, want the spawn's %s", second.ObservedAt, first.ObservedAt)
	}
}

func TestAStaleCacheIsAMiss(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	// A 1 ms freshness window makes every second call a miss without a fake
	// clock, and without sleeping for 30 s.
	src := newSource(t, dir, r, func(c *agentscli.Config) { c.StaleAfterMs = 1 })

	_, _ = src.List(context.Background())
	time.Sleep(5 * time.Millisecond)
	second, _ := src.List(context.Background())

	if second.Cache != observer.AgentsCacheMiss {
		t.Fatalf("cache = %q, want miss for a listing older than staleAfterMs", second.Cache)
	}
	if r.count() != 2 {
		t.Fatalf("spawns = %d, want 2", r.count())
	}
}

func TestASchemaMismatchedCacheIsTreatedAsAMiss(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, nil)

	// A cache written by a build whose wire schema is not ours. M6: a long-lived
	// process from an older build is the EXPECTED case, since §10 installs only
	// at P9 while the user runs tt daily.
	writeEnvelope(t, dir, wire.Schema+1, time.Now(), fixture(t, "real-39.json"))

	res, _ := src.List(context.Background())
	if res.Cache != observer.AgentsCacheMiss {
		t.Fatalf("cache = %q, want miss for schema %d", res.Cache, wire.Schema+1)
	}
	if r.count() != 1 {
		t.Fatalf("spawns = %d, want 1: a schema mismatch must spawn normally", r.count())
	}

	// And the file it wrote back carries OUR schema, so the next process agrees.
	env := readEnvelope(t, dir)
	var schema int
	if err := json.Unmarshal(env["schema"], &schema); err != nil {
		t.Fatalf("the published cache has no readable schema: %v", err)
	}
	if schema != wire.Schema {
		t.Fatalf("published schema = %d, want %d", schema, wire.Schema)
	}
}

func TestAnUnparseableCacheIsAMiss(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, nil)

	if err := os.WriteFile(filepath.Join(dir, agentscli.CacheFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := src.List(context.Background())
	if res.Cache != observer.AgentsCacheMiss || r.count() != 1 {
		t.Fatalf("cache = %q spawns = %d, want miss/1", res.Cache, r.count())
	}
}

func TestTheCacheCarriesTheVendorBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	payload := fixture(t, "real-39.json")
	r := &runner{payload: payload}
	src := newSource(t, dir, r, nil)

	_, _ = src.List(context.Background())

	env := readEnvelope(t, dir)
	var cached, original []any
	if err := json.Unmarshal(env["payload"], &cached); err != nil {
		t.Fatalf("the cached payload is not the listing: %v", err)
	}
	if err := json.Unmarshal(payload, &original); err != nil {
		t.Fatal(err)
	}
	if len(cached) != len(original) {
		t.Fatalf("cached %d entries, the vendor produced %d — the cache is re-encoding",
			len(cached), len(original))
	}
	var tt string
	if err := json.Unmarshal(env["tt"], &tt); err != nil || tt != "v0-test" {
		t.Fatalf("tt = %q (%v), want the build that wrote it", tt, err)
	}
}

// ---------------------------------------------------------------------------
// §4.3(b) — the lock. H2, the only unanimous round-2 finding.
// ---------------------------------------------------------------------------

func TestSixteenSimultaneousColdStartsProduceAtMostOneSpawn(t *testing.T) {
	const n = 16
	dir := t.TempDir()
	// One runner, shared, because the question is how many CHILDREN the machine
	// starts — not how many each process starts.
	r := &runner{payload: fixture(t, "real-39.json"), delay: 20 * time.Millisecond}

	sources := make([]*agentscli.Source, n)
	for i := range sources {
		// A separate Source per "process": separate memory, one shared state
		// directory. That is exactly what sixteen `tt mcp` processes are to the
		// lock, and it is the case revision 1's sequential verification missed.
		sources[i] = newSource(t, dir, r, nil)
	}

	results := make([]observer.AgentsResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range sources {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], _ = sources[i].List(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	if got := r.count(); got > 1 {
		t.Fatalf("spawns = %d for %d simultaneous cold starts, want <= 1", got, n)
	}
	if r.count() == 0 {
		t.Fatal("spawns = 0: nobody took the lock, so the bound is vacuous")
	}

	var byOutcome = map[string]int{}
	for _, res := range results {
		byOutcome[res.Cache]++
	}
	if byOutcome[observer.AgentsCacheMiss] != 1 {
		t.Fatalf("outcomes = %v, want exactly one miss", byOutcome)
	}
	// Every other process either waited for the answer or went without. Neither
	// spawned, which is the whole claim.
	if byOutcome[observer.AgentsCacheMiss]+byOutcome[observer.AgentsCacheHit]+
		byOutcome[observer.AgentsCacheHitAfterWait]+byOutcome[observer.AgentsCacheContended] != n {
		t.Fatalf("outcomes = %v, which includes something other than the four §4.3(b) cases", byOutcome)
	}
	for _, res := range results {
		if res.Cache == observer.AgentsCacheContended && res.OK {
			t.Fatal("a contended process reported ok true: it has no listing to vouch for")
		}
	}
}

func TestStaggeredColdStartsOverTwiceStaleAfterProduceAtMostTwoSpawns(t *testing.T) {
	const (
		n            = 16
		staleAfterMs = 60
	)
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}

	// Spread over 2 x staleAfterMs. §4.3(b)'s bound: at most
	// ceil(T / staleAfterMs) spawns over an interval T, for any N.
	//
	// The schedule is ABSOLUTE — each caller waits until begin + i*step — rather
	// than a sleep between sequential calls. A relative loop measures T as
	// "2 x staleAfterMs plus sixteen calls' worth of work", which is a longer
	// interval and is therefore allowed a third spawn by the very bound being
	// tested. It cost one failing run to notice, and the failure was the test's.
	window := time.Duration(2*staleAfterMs) * time.Millisecond
	step := window / n
	begin := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		src := newSource(t, dir, r, func(c *agentscli.Config) { c.StaleAfterMs = staleAfterMs })
		wg.Add(1)
		go func(i int, src *agentscli.Source) {
			defer wg.Done()
			time.Sleep(time.Until(begin.Add(time.Duration(i) * step)))
			if _, err := src.List(context.Background()); err != nil {
				t.Errorf("List: %v", err)
			}
		}(i, src)
	}
	wg.Wait()
	span := time.Since(begin)

	if got := r.count(); got > 2 {
		t.Fatalf("spawns = %d over %s (2x staleAfterMs = %s) with %d processes, want <= 2",
			got, span, window, n)
	}
	if r.count() == 0 {
		t.Fatal("spawns = 0: the bound is vacuous")
	}
}

func TestAContendedProcessAnswersWithoutCorroborationRatherThanSpawning(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, func(c *agentscli.Config) {
		// Shorter than the lock we are about to plant is old, so the wait
		// expires without the lock ever looking abandoned.
		c.LockWait = 60 * time.Millisecond
	})

	// Another process holds the lock and has published nothing.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, agentscli.LockFileName)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := src.List(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if r.count() != 0 {
		t.Fatalf("spawns = %d: a contended process must NOT spawn — that is the stampede", r.count())
	}
	if res.Cache != observer.AgentsCacheContended {
		t.Fatalf("cache = %q, want contended", res.Cache)
	}
	if res.OK {
		t.Fatal("ok true without a listing")
	}
	if res.WaitedMs <= 0 {
		t.Fatalf("waitedMs = %d, want the wait it actually spent", res.WaitedMs)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("waited %s: a contended process must not block on the holder", elapsed)
	}
}

func TestAContendedProcessAdoptsTheHoldersAnswerWhenItLandsInTime(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, func(c *agentscli.Config) { c.LockWait = 2 * time.Second })

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentscli.LockFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The "holder" publishes 30 ms in.
	go func() {
		time.Sleep(30 * time.Millisecond)
		writeEnvelope(t, dir, wire.Schema, time.Now(), fixture(t, "real-39.json"))
	}()

	res, _ := src.List(context.Background())
	if res.Cache != observer.AgentsCacheHitAfterWait {
		t.Fatalf("cache = %q, want hit-after-wait", res.Cache)
	}
	if !res.OK || len(res.Entries) != 39 {
		t.Fatalf("ok = %v entries = %d, want true/39", res.OK, len(res.Entries))
	}
	if r.count() != 0 {
		t.Fatalf("spawns = %d, want 0: the holder's answer was adopted", r.count())
	}
}

func TestAnAbandonedLockIsRemovedAndRetriedOnce(t *testing.T) {
	dir := t.TempDir()
	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, dir, r, func(c *agentscli.Config) {
		c.SpawnTimeout = 50 * time.Millisecond
		c.LockWait = 50 * time.Millisecond
	})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, agentscli.LockFileName)
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Older than spawnTimeout + 1 s: its holder cannot still be spawning, so it
	// was killed mid-spawn. Without this rule one dead process locks out the
	// machine permanently.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	res, _ := src.List(context.Background())
	if res.Cache != observer.AgentsCacheMiss {
		t.Fatalf("cache = %q, want miss: an abandoned lock must not block a spawn", res.Cache)
	}
	if r.count() != 1 {
		t.Fatalf("spawns = %d, want 1", r.count())
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("the lock survived: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC13's runtime half
// ---------------------------------------------------------------------------

func TestTheCacheRefusesToWriteInsideTheClaudeTree(t *testing.T) {
	claudeRoot := t.TempDir()
	inside := filepath.Join(claudeRoot, "state", "tt")
	r := &runner{payload: fixture(t, "real-39.json")}
	src := agentscli.New(agentscli.Config{
		StateDir:     inside,
		ClaudeRoot:   claudeRoot,
		SpawnTimeout: time.Second,
		StaleAfterMs: 30_000,
		Run:          r.run,
	})

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The listing still works — the cache is a convenience, never a source of
	// truth — but nothing was written.
	if !res.OK || len(res.Entries) != 39 {
		t.Fatalf("ok = %v entries = %d: a refused cache must still answer", res.OK, len(res.Entries))
	}
	if res.Cache != observer.AgentsCacheUnwritable {
		t.Fatalf("cache = %q, want unwritable", res.Cache)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("the state directory was created inside the Claude tree: %v", err)
	}
	entries, err := os.ReadDir(claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the Claude tree gained %d entries", len(entries))
	}
}

func TestTheDefaultStateDirIsUnderLocalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	want := filepath.Join(home, ".local", "state", "tt")
	if got := agentscli.DefaultStateDir(); got != want {
		t.Fatalf("DefaultStateDir = %q, want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	if got := agentscli.DefaultStateDir(); got != filepath.Join("/tmp/xdg-state", "tt") {
		t.Fatalf("DefaultStateDir with XDG_STATE_HOME = %q", got)
	}
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func writeEnvelope(t *testing.T, dir string, schema int, at time.Time, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"schema":     schema,
		"tt":         "v0-planted",
		"claudeRoot": claudeRootFor(dir),
		"observedAt": observer.FormatTime(at),
		"payload":    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Written through a temp file and renamed, the way the adapter publishes, so
	// a reader polling for the file never sees a partial one.
	tmp := filepath.Join(dir, "planted.tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, agentscli.CacheFileName)); err != nil {
		t.Fatal(err)
	}
}

func readEnvelope(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, agentscli.CacheFileName))
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("the cache is not JSON: %v", err)
	}
	return env
}

// ---------------------------------------------------------------------------
// the live-run finding: a cache describes ONE Claude tree
// ---------------------------------------------------------------------------

func TestACacheDescribingAnotherClaudeTreeIsAMiss(t *testing.T) {
	// `claude agents --json` reads CLAUDE_CONFIG_DIR, so a listing taken against
	// a different tree describes different machine state. One shared cache file
	// would otherwise serve it to the next ordinary run for up to staleAfterMs —
	// which is exactly what happened to the author's own cache when the hermetic
	// CLI test published a one-entry fixture listing into it.
	dir := t.TempDir()
	rootA, rootB := filepath.Join(dir, "claude-a"), filepath.Join(dir, "claude-b")
	state := filepath.Join(dir, "state")

	r := &runner{payload: fixture(t, "real-39.json")}
	srcA := newSource(t, state, r, func(c *agentscli.Config) { c.ClaudeRoot = rootA })
	srcB := newSource(t, state, r, func(c *agentscli.Config) { c.ClaudeRoot = rootB })

	if res, _ := srcA.List(context.Background()); res.Cache != observer.AgentsCacheMiss {
		t.Fatalf("first call: cache = %q, want miss", res.Cache)
	}
	// Same state directory, same instant, different tree: a MISS.
	resB, _ := srcB.List(context.Background())
	if resB.Cache != observer.AgentsCacheMiss {
		t.Fatalf("cache = %q for a listing about another Claude tree, want miss", resB.Cache)
	}
	if r.count() != 2 {
		t.Fatalf("spawns = %d, want 2: each tree needs its own listing", r.count())
	}
	// And A now misses too, because B published its own. Two trees sharing one
	// cache file cost spawns; they never exchange answers.
	againA, _ := srcA.List(context.Background())
	if againA.Cache != observer.AgentsCacheMiss {
		t.Fatalf("cache = %q, want miss after the other tree published", againA.Cache)
	}
}

// The live failure of 2026-10-21: `tt observe` was started inside a git
// worktree that was later removed, so every spawn inherited a deleted working
// directory and `claude agents --json` refused each one ("The current working
// directory was deleted"). The panel then said "corroboration 8h50m old" for
// as long as the server lived. A long-lived process cannot keep its cwd alive,
// so the spawn must not depend on it.
func TestTheSpawnDoesNotInheritADeletedWorkingDirectory(t *testing.T) {
	cmd, err := filepath.Abs(filepath.Join(fixtureDir, "needs-live-cwd"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	src := agentscli.New(agentscli.Config{
		Command:      cmd,
		Args:         []string{},
		StateDir:     stateDir,
		SpawnTimeout: 5 * time.Second,
		StaleAfterMs: 30_000,
		LockWait:     50 * time.Millisecond,
	})
	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok false from a spawn whose parent's cwd was deleted: %v", res.Err)
	}
}
