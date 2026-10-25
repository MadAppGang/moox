package mcpserve_test

// AC11, complete: the four machine surfaces are the same bytes.
//
// §9.2(a)'s procedure, followed literally, and every emphasis in it is
// load-bearing:
//
//	ONE fixture *observer.Snapshot, built once and never re-read. Each surface
//	    is given a source that answers with THAT value and reads nothing.
//	ONE instant. Every surface encodes with wire.NewSnapshot(snap,
//	    snap.ObservedAt, …) — no surface takes a clock read of its own, which
//	    is the rule three live defects came from breaking — so the fixture's
//	    own t0 IS §9.2(a)'s fake clock.
//	ONE explicit SnapshotOptions{IncludeDead:false, IncludeUnknownLiveness:true,
//	    IncludeTranscripts:true}, passed to all four, so the per-surface
//	    DEFAULTS (S1 collects transcripts, MCP's includeTranscript defaults
//	    false) stay a documented product choice rather than a comparison
//	    hazard.
//	NO filters anywhere.
//
// The four surfaces:
//
//	S1 CLI     `tt sessions --json`   jsonout.Run's stdout
//	S2 MCP     tools/call list_sessions -> the result's structuredContent
//	S3 socket  the snapshot frame's nested `snapshot` payload, read off a real
//	           unix socket as raw bytes
//	S4 client  sockclient's RE-DERIVED snapshot: the frame decoded through
//	           wire.ToDomain into a local *observer.Snapshot and encoded again
//
// S4 is the leg that makes the property mean something. S1..S3 share an
// encoder and a value, so their agreement proves there is one encoder; S4
// went out as JSON, came back through the decoder, and was encoded again, so
// its agreement proves the decoder is that encoder's inverse. A drift in
// either direction fails here rather than in a consumer.
//
// This test binds a unix socket inside os.MkdirTemp and nothing else: no
// process is spawned, no tmux server is started, and nothing is written under
// $HOME or ~/.claude.
//
// # One deliberate change, and what keeps the guarantee meaningful
//
// The MCP surface now WITHHOLDS the two free-text job fields (`needs`,
// `detail`) by default, because that surface's audience is other agents rather
// than the user. That is a projection applied AFTER the encoder, not a second
// encoder — so the four-surface property is asserted in two parts, and neither
// part is weaker than the original single assertion:
//
//	part 1 (this test) drives S2 with PublishJobText, which is the encoder with
//	    nothing removed. All four surfaces are compared byte for byte with
//	    NOTHING excused, exactly as before: one encoder, one value, and a
//	    decoder that is the encoder's inverse.
//	part 2 (the test below) drives S2 with the default and proves the
//	    divergence is EXACTLY the two named members and nothing else — the
//	    remaining ~60 fields still match the CLI byte for byte, and the two
//	    that differ differ by carrying a placeholder rather than by being
//	    dropped, reordered or retyped.
//
// A surface quietly growing a second opinion about any other field still fails
// here, which is the property AC11 was written to protect.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
	"github.com/MadAppGang/moox/internal/observer/adapters/mcpserve"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockclient"
	"github.com/MadAppGang/moox/internal/observer/adapters/sockserve"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// ac11Options is the ONE options value all four surfaces are driven with.
func ac11Options() observer.SnapshotOptions {
	o := observer.DefaultSnapshotOptions()
	o.IncludeDead = false
	o.IncludeUnknownLiveness = true
	o.IncludeTranscripts = true
	return o
}

// ac11Snapshot is the fixture: this file's two-session snapshot, plus the two
// things that would otherwise make the comparison pass for being empty.
//
//   - It is PRE-ENRICHED. Every surface here is driven with
//     IncludeTranscripts:true and none of them is given an Enricher, so the
//     transcript block on the wire can only come from the fixture. Without it
//     the four surfaces would agree on `transcript:null` and AC12's whole
//     field set would go uncompared.
//   - Its registry source carries a WATCH block. That is the one field
//     §9.2(a) excludes by name (mode differs per surface by design), and an
//     absent block would leave the exclusion untested — which is how P7's
//     live run found `watch` null on every surface.
func ac11Snapshot() *observer.Snapshot {
	snap := fixture()

	subagents := 2
	kind := "interactive"
	snap.Sessions[0].TranscriptCollected = true
	snap.Sessions[0].Transcript = &observer.TranscriptFacts{
		Path:           "/fixture/transcripts/s1.jsonl",
		SizeBytes:      204_800,
		TailBytes:      204_800,
		Truncated:      false,
		AgentColor:     "cyan",
		ColorFound:     true,
		APIErrors:      2,
		Subagents:      &subagents,
		SubagentsKnown: true,
		SessionKind:    &kind,
	}
	snap.Sessions[1].TranscriptCollected = true
	// The free-text job fields, present so the surfaces are compared with them
	// carrying content rather than null — which is what makes the withholding
	// test below assert something.
	detail := "blocked on the toolbar mount location"
	snap.Sessions[1].Detail = &detail

	reg := snap.Sources[observer.SourceRegistry]
	reg.Watch = &observer.WatchReport{
		Mode:          observer.WatchModeKqueue,
		ArmedDirs:     1,
		ArmedFiles:    2,
		ArmIterations: 1,
	}
	snap.Sources[observer.SourceRegistry] = reg
	return snap
}

// ac11Source answers with one snapshot forever and reads nothing. §9.2(a)'s
// "never re-read" as a type rather than as a convention.
type ac11Source struct{ snap *observer.Snapshot }

func (s ac11Source) Snapshot(context.Context, observer.SnapshotOptions) (*observer.Snapshot, error) {
	return s.snap, nil
}

// Latest and Subscribe are sockserve's push half. The subscription delivers
// ONE EventSnapshot carrying the same value and then nothing: a connection's
// baseline is written from that event, which is the path a real client takes.
func (s ac11Source) Latest() *observer.Snapshot { return s.snap }

func (s ac11Source) Subscribe(ctx context.Context, _ observer.Filter) (<-chan observer.Event, error) {
	ch := make(chan observer.Event, 1)
	ch <- observer.Event{Type: observer.EventSnapshot, Snapshot: s.snap, At: s.snap.ObservedAt}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// ac11Dir is a temp state directory short enough for sockaddr_un's 104-byte
// sun_path, which t.TempDir()'s test-name-derived path is not.
func ac11Dir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttac11")
	if err != nil {
		t.Fatalf("making a temp state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if n := len(filepath.Join(dir, sockserve.SocketName)); n >= 104 {
		t.Fatalf("the socket path is %d bytes, which sockaddr_un cannot hold", n)
	}
	return dir
}

func TestAC11TheFourSurfacesAreTheSameBytes(t *testing.T) {
	snap := ac11Snapshot()
	opts := ac11Options()

	// ---------------------------------------------------------------- S1 CLI
	var stdout, stderr bytes.Buffer
	if code := jsonout.Run(context.Background(), ac11Source{snap: snap}, jsonout.Options{
		JSON:     true,
		TT:       testTT,
		Snapshot: opts,
	}, &stdout, &stderr); code != jsonout.ExitOK {
		t.Fatalf("the CLI exited %d: %s", code, stderr.String())
	}
	overCLI := bytes.TrimSpace(stdout.Bytes())

	// ---------------------------------------------------------------- S2 MCP
	// PublishJobText: the encoder with nothing removed, which is what this leg
	// is comparing. The default's one divergence has its own test below.
	h := newHarness(t, func(cfg *mcpserve.Config, src *fakeSource) {
		cfg.PublishJobText = true
		src.snap = snap
	})
	res := h.call("list_sessions", map[string]any{
		"includeDead":            opts.IncludeDead,
		"includeUnknownLiveness": opts.IncludeUnknownLiveness,
		"includeTranscript":      opts.IncludeTranscripts,
		"limit":                  200,
	})
	if res.IsError {
		t.Fatalf("list_sessions refused: %s", res.text(t))
	}
	overMCP := bytes.TrimSpace(res.StructuredContent)

	// ------------------------------------------------------------- S3 socket
	dir := ac11Dir(t)
	srv, err := sockserve.New(sockserve.Config{
		Dir:        dir,
		ClaudeRoot: dir + "-claude-root",
		TT:         testTT,
		PID:        4242,
		Deps: sockserve.Deps{
			Events: ac11Source{snap: snap},
			Core:   ac11Source{snap: snap},
		},
	})
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("binding: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("the server did not stop")
		}
	})

	overSocket := socketSnapshotBytes(t, filepath.Join(dir, sockserve.SocketName))

	// ------------------------------------------------------------- S4 client
	// ONE instant, for S4 too. Since code review round 2 (A7) the client
	// derives ages and clock-sensitive liveness at ANSWER time from its own
	// clock (§7.5c); the procedure's single instant is the fixture's own, so
	// the consumer's clock is pinned there. At an equal instant the answer-time
	// derivation is the identity, which is exactly what this leg proves: the
	// decoder is the encoder's inverse.
	c, err := sockclient.Dial(context.Background(), sockclient.Config{Dir: dir, Clock: fixedClock{snap.ObservedAt}})
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	defer func() { _ = c.Close() }()

	local, err := c.Snapshot(context.Background(), opts)
	if err != nil {
		t.Fatalf("the client has no snapshot: %v", err)
	}
	if got := c.UnknownFields(); len(got) != 0 {
		t.Fatalf("the client saw fields it does not know: %+v", got)
	}
	// DecodeSnapshot's inverse, re-encoded: the round trip is the assertion.
	overClient := encodeAC11(t, wire.NewSnapshot(local, local.ObservedAt, wire.Options{TT: testTT}))

	// ------------------------------------------------------------- the proof
	//
	// All four are compared with NOTHING excused. §9.2(a) permits an exclusion
	// allowlist — `sources.<id>.watch`, the transport envelope's type/seq/
	// reqId, the MCP content wrapper, and `notCapable` — and three of those
	// four are stripped by construction here: this compares the socket frame's
	// nested `snapshot` payload and the MCP result's `structuredContent`, not
	// the wrappers around them. The fourth, `watch`, is NOT excused and does
	// not need to be: the client re-derives it from the payload rather than
	// substituting its own mode, so the kqueue block the server armed comes
	// back out of wire.ToDomain unchanged. If a surface ever legitimately
	// reports its own watch mode (describe_sources' SurfaceWatch is the shape
	// that would), THAT member is the one §9.2(a) allows to be dropped here —
	// and nothing else, because anything else differing is a surface that has
	// grown a second opinion about the same value.
	if !bytes.Equal(overCLI, overMCP) {
		t.Fatalf("S1 and S2 disagree.\n cli: %s\n mcp: %s", overCLI, overMCP)
	}
	if !bytes.Equal(overCLI, overSocket) {
		t.Fatalf("S1 and S3 disagree.\n   cli: %s\nsocket: %s", overCLI, overSocket)
	}
	if !bytes.Equal(overCLI, overClient) {
		t.Fatalf("S1 and S4 disagree — the decoder is not the encoder's inverse.\n   cli: %s\nclient: %s",
			overCLI, overClient)
	}

	// And the agreement is not the agreement of four empty envelopes: the
	// fields AC11 names are all present in the bytes that agreed.
	for _, want := range []string{
		`"schema":`, `"tt":"` + testTT + `"`, `"observedAt":`,
		`"counts":{"total":2`, `"warnings":[]`, `"notCapable":["time-transitions"]`,
		`"sessionId":"s1"`, `"sessionId":"s2"`,
		`"watch":{"mode":"kqueue"`, `"agentColor":"cyan"`, `"subagents":2`,
		`"liveReason":"proc+pane"`, `"listedBy":["registry"]`,
		`"stale":false`, `"advisories":[]`, `"sources":["registry"]`,
	} {
		if !bytes.Contains(overCLI, []byte(want)) {
			t.Errorf("the agreed bytes do not contain %s", want)
		}
	}
}

// socketSnapshotBytes reads frames off a RAW connection until the snapshot
// frame arrives and returns its nested `snapshot` payload exactly as it came
// off the wire. Raw rather than through sockclient on purpose: S3 is the
// bytes the server wrote, and decoding them would make this leg a copy of S4.
func socketSnapshotBytes(t *testing.T, path string) []byte {
	t.Helper()
	nc, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dialling the socket: %v", err)
	}
	defer func() { _ = nc.Close() }()
	if err := nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("arming the read bound: %v", err)
	}

	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 0, 4096), sockserve.MaxFrameBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		// Selected on the frame TYPE, not on the presence of a `snapshot`
		// member: §5.1 bans omitempty, so every frame carries every key and
		// hello's `snapshot` is a literal null.
		if string(bytes.TrimSpace(frame["type"])) != `"`+wire.FrameSnapshot+`"` {
			continue
		}
		return bytes.TrimSpace(frame["snapshot"])
	}
	t.Fatalf("no snapshot frame arrived: %v", sc.Err())
	return nil
}

// encodeAC11 encodes one envelope the way every surface does: compact, through
// the standard encoder, with no private formatting choice.
func encodeAC11(t *testing.T, s wire.Snapshot) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(s); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return bytes.TrimSpace(buf.Bytes())
}

// Part 2 of AC11: with the default configuration, the MCP surface differs from
// the CLI in EXACTLY the two free-text job fields and in nothing else.
//
// This is the test that keeps the divergence honest. "S2 is allowed to differ"
// is the kind of exception that, left unasserted, grows: the next field to
// drift would hide behind it. So the comparison here is not relaxed — it is
// made precise. Both envelopes are decoded, the two named members are cleared
// on BOTH, and the re-encoded bytes must be identical; then the members
// themselves are checked from opposite sides, the CLI carrying the sentence and
// the MCP surface carrying a placeholder that still proves a need exists.
func TestAC11TheMCPSurfaceWithholdsOnlyTheFreeTextJobFields(t *testing.T) {
	snap := ac11Snapshot()
	opts := ac11Options()

	var stdout, stderr bytes.Buffer
	if code := jsonout.Run(context.Background(), ac11Source{snap: snap}, jsonout.Options{
		JSON:     true,
		TT:       testTT,
		Snapshot: opts,
	}, &stdout, &stderr); code != jsonout.ExitOK {
		t.Fatalf("the CLI exited %d: %s", code, stderr.String())
	}

	h := newHarness(t, func(_ *mcpserve.Config, src *fakeSource) { src.snap = snap })
	res := h.call("list_sessions", map[string]any{
		"includeDead":            opts.IncludeDead,
		"includeUnknownLiveness": opts.IncludeUnknownLiveness,
		"includeTranscript":      opts.IncludeTranscripts,
		"limit":                  200,
	})
	if res.IsError {
		t.Fatalf("list_sessions refused: %s", res.text(t))
	}

	cli := decodeAC11(t, stdout.Bytes())
	mcp := decodeAC11(t, res.StructuredContent)

	// The two members must differ in the way the design says, from both sides.
	if len(cli.Sessions) != 2 || len(mcp.Sessions) != 2 {
		t.Fatalf("the fixture lost sessions: cli %d, mcp %d", len(cli.Sessions), len(mcp.Sessions))
	}
	for _, f := range []struct {
		name string
		cli  *string
		mcp  *string
	}{
		{"needs", cli.Sessions[1].Needs, mcp.Sessions[1].Needs},
		{"detail", cli.Sessions[1].Detail, mcp.Sessions[1].Detail},
	} {
		if f.cli == nil || *f.cli == "" {
			t.Fatalf("the fixture carries no %s, so this test proves nothing", f.name)
		}
		if f.mcp == nil {
			t.Errorf("%s was dropped rather than withheld: an agent can no longer tell that "+
				"this session needs something", f.name)
			continue
		}
		if *f.mcp == *f.cli {
			t.Errorf("%s reached the MCP surface verbatim: %q", f.name, *f.mcp)
		}
		if !strings.HasPrefix(*f.mcp, wire.JobTextWithheldPrefix) {
			t.Errorf("%s on the MCP surface is %q, want a %q placeholder",
				f.name, *f.mcp, wire.JobTextWithheldPrefix)
		}
	}

	// And EVERYTHING else is still byte-identical.
	for i := range cli.Sessions {
		cli.Sessions[i].Needs, cli.Sessions[i].Detail = nil, nil
		mcp.Sessions[i].Needs, mcp.Sessions[i].Detail = nil, nil
	}
	if a, b := encodeAC11(t, cli), encodeAC11(t, mcp); !bytes.Equal(a, b) {
		t.Fatalf("the surfaces diverge somewhere other than the withheld fields.\n cli: %s\n mcp: %s", a, b)
	}
}

// decodeAC11 is encodeAC11's inverse, for the one comparison that has to
// normalise two named members before comparing the rest.
func decodeAC11(t *testing.T, data []byte) wire.Snapshot {
	t.Helper()
	var out wire.Snapshot
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil {
		t.Fatalf("decoding an envelope: %v", err)
	}
	return out
}

// fixedClock is a consumer clock stopped at one instant.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }
