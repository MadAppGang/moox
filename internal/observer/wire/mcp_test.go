package wire_test

// S2's vocabulary: the JSON-RPC envelope, strict tool arguments and the two
// projections. The tool BEHAVIOUR is in mcpserve's tests; what is pinned here
// is the shape, because the shape is what four surfaces have to agree on.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// A JSON-RPC response carries result XOR error — never both, and never the
// other one as null.
//
// This is the reason RPCResponse has a hand-written MarshalJSON: §5.1 bans
// omitempty across this package, so struct tags alone would emit
// `"error":null` on every successful call, which a strict client is entitled
// to reject.
func TestAnRPCResponseCarriesResultXorErrorAndNeverBothKeys(t *testing.T) {
	ok, err := json.Marshal(wire.NewRPCResult(json.RawMessage(`7`), map[string]string{"a": "b"}))
	if err != nil {
		t.Fatalf("marshalling a result: %v", err)
	}
	if !strings.Contains(string(ok), `"result":`) || strings.Contains(string(ok), `"error"`) {
		t.Fatalf("a successful response is not result-only: %s", ok)
	}

	bad, err := json.Marshal(wire.NewRPCFailure(json.RawMessage(`7`), wire.RPCMethodNotFound, "nope"))
	if err != nil {
		t.Fatalf("marshalling a failure: %v", err)
	}
	if !strings.Contains(string(bad), `"error":`) || strings.Contains(string(bad), `"result"`) {
		t.Fatalf("a failed response is not error-only: %s", bad)
	}
	if !strings.HasPrefix(string(bad), `{"jsonrpc":"2.0","id":7,`) {
		t.Fatalf("the envelope is not jsonrpc/id first: %s", bad)
	}
}

// The id is echoed BYTE FOR BYTE. JSON-RPC allows a string or a number and a
// client matches on what it sent, so `"1"` must not come back as `1`.
func TestTheRequestIDIsEchoedVerbatim(t *testing.T) {
	for _, id := range []string{`1`, `"1"`, `"call-7a32"`, `0`} {
		out, err := json.Marshal(wire.NewRPCResult(json.RawMessage(id), struct{}{}))
		if err != nil {
			t.Fatalf("marshalling id %s: %v", id, err)
		}
		if !strings.Contains(string(out), `"id":`+id+`,`) {
			t.Fatalf("id %s was not echoed verbatim: %s", id, out)
		}
	}
	// A response to something that had no id at all still carries the key,
	// because JSON-RPC requires it and null is the specified value.
	out, err := json.Marshal(wire.NewRPCFailure(nil, wire.RPCParseError, "x"))
	if err != nil {
		t.Fatalf("marshalling a nil id: %v", err)
	}
	if !strings.Contains(string(out), `"id":null`) {
		t.Fatalf("a response with no id does not carry id null: %s", out)
	}
}

// A notification is a request with NO id. A null id is a request whose id is
// null, and both are answered by silence — a server that replied to
// `"id":null` would be answering something no client is waiting for.
func TestNotificationsAreTheRequestsWithNoID(t *testing.T) {
	cases := map[string]bool{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`:          true,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`:  true,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`:                  false,
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{}}`:    false,
		`{"jsonrpc":"2.0","id":0,"method":"ping"}`:                        false,
		`{"jsonrpc":"2.0","method":"tools/list","params":{"cursor":"x"}}`: true,
	}
	for line, want := range cases {
		req, err := wire.DecodeRPCRequest([]byte(line))
		if err != nil {
			t.Fatalf("decoding %s: %v", line, err)
		}
		if got := req.IsNotification(); got != want {
			t.Errorf("IsNotification(%s) = %t, want %t", line, got, want)
		}
	}
}

// A line that is JSON but not a JSON-RPC 2.0 request is refused, and the two
// things that make it one are named: the version and the method.
func TestDecodeRPCRequestRefusesWhatIsNotARequest(t *testing.T) {
	for _, line := range []string{
		`{"id":1,"method":"tools/list"}`,
		`{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"method":""}`,
	} {
		if _, err := wire.DecodeRPCRequest([]byte(line)); err == nil {
			t.Errorf("%s was accepted as a request", line)
		}
	}

	// And an unknown FIELD is not a reason to refuse: a newer client sending a
	// key this build does not know should still be served.
	if _, err := wire.DecodeRPCRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"ping","meta":{"trace":"x"}}`)); err != nil {
		t.Fatalf("an unknown field was refused: %v", err)
	}
}

// Tool arguments are decoded STRICTLY, because every schema says
// additionalProperties: false. A caller misspelling `waitingOnly` must be told,
// not silently handed every session on the machine.
func TestToolArgumentsAreDecodedStrictly(t *testing.T) {
	var a wire.ListSessionsArgs
	if err := wire.DecodeToolArgs([]byte(`{"waitingOnly":true,"limit":5}`), &a); err != nil {
		t.Fatalf("a valid argument object was refused: %v", err)
	}
	if !a.WaitingOnly || a.Limit == nil || *a.Limit != 5 {
		t.Fatalf("arguments did not decode: %+v", a)
	}

	var b wire.ListSessionsArgs
	if err := wire.DecodeToolArgs([]byte(`{"waitingonly":true}`), &b); err == nil {
		t.Fatal("a misspelled argument was accepted")
	}

	// Absent arguments are the zero value, not an error: `{"name":"x"}` with no
	// arguments key at all is how a no-argument tool is called.
	var c wire.NoArgs
	for _, raw := range []string{``, `null`, `{}`} {
		if err := wire.DecodeToolArgs([]byte(raw), &c); err != nil {
			t.Fatalf("empty arguments %q were refused: %v", raw, err)
		}
	}
	if err := wire.DecodeToolArgs([]byte(`{"limit":5}`), &c); err == nil {
		t.Fatal("a no-argument tool accepted an argument")
	}
}

// The two defaults that are not their zero values are POINTERS, so "absent"
// and "false"/"0" stay distinguishable. A non-pointer includeUnknownLiveness
// would turn every call that omitted the key into the one filtering rule M7
// forbids.
func TestTheTwoNonZeroDefaultsAreDistinguishableFromAbsent(t *testing.T) {
	var a wire.ListSessionsArgs
	if err := wire.DecodeToolArgs([]byte(`{}`), &a); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if a.IncludeUnknownLiveness != nil || a.Limit != nil {
		t.Fatalf("absent keys did not decode as absent: %+v", a)
	}

	var b wire.ListSessionsArgs
	if err := wire.DecodeToolArgs([]byte(`{"includeUnknownLiveness":false,"limit":1}`), &b); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if b.IncludeUnknownLiveness == nil || *b.IncludeUnknownLiveness {
		t.Fatalf("an explicit false decoded as absent: %+v", b)
	}
}

// get_session's "exactly one" is countable in one place rather than in three
// branches.
func TestGetSessionCountsItsSelectors(t *testing.T) {
	pid := 42
	cases := []struct {
		args wire.GetSessionArgs
		want int
	}{
		{wire.GetSessionArgs{}, 0},
		{wire.GetSessionArgs{SessionID: "a"}, 1},
		{wire.GetSessionArgs{PID: &pid}, 1},
		{wire.GetSessionArgs{Name: "n"}, 1},
		{wire.GetSessionArgs{SessionID: "a", Name: "n"}, 2},
		{wire.GetSessionArgs{SessionID: "a", PID: &pid, Name: "n"}, 3},
	}
	for _, c := range cases {
		if got := c.args.Given(); got != c.want {
			t.Errorf("Given(%+v) = %d, want %d", c.args, got, c.want)
		}
	}
}

// session_counts' payload is a PROJECTION of the envelope, so the two tools
// cannot disagree about the same session: every attention entry is one of the
// sessions list_sessions would have printed, and the counts are the envelope's
// own.
func TestNewToolResultIsAProjectionOfTheEnvelopeAndNeverASecondAnswer(t *testing.T) {
	needs := "open a terminal and test `ck`"
	snap := &observer.Snapshot{
		ObservedAt: time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC),
		Counts:     observer.NewCounts(),
		Sessions: []observer.Session{
			{SessionID: "s1", Name: "quiet", Status: observer.StatusIdle},
			{
				SessionID: "s2", Name: "blocked one", Kind: observer.KindBackground,
				Status: observer.StatusBlocked, Attention: true, Needs: &needs,
			},
		},
		NotCapable: observer.DefaultNotCapable(),
	}
	snap.Counts.Total = 2
	snap.Counts.Attention = 1

	env := wire.NewSnapshot(snap, snap.ObservedAt, wire.Options{TT: "v-test"})
	out := wire.NewToolResult(env)

	if len(out.Attention) != 1 || out.Attention[0].SessionID != "s2" {
		t.Fatalf("the attention projection is not the attention set: %+v", out.Attention)
	}
	if out.Attention[0].Needs == nil || *out.Attention[0].Needs != needs {
		t.Fatal("needs did not survive the projection; for a blocked agent that string IS the answer")
	}
	if out.Counts.Total != env.Counts.Total || out.Counts.Attention != env.Counts.Attention {
		t.Fatalf("the counts are not the envelope's: %+v vs %+v", out.Counts, env.Counts)
	}
	if out.NotCapable == nil || len(out.NotCapable) != 1 {
		t.Fatalf("notCapable did not travel: %+v", out.NotCapable)
	}

	// And the projection cannot carry what the envelope does not have: it reads
	// a wire.Session, so there is no field on it that the JSON surfaces lack.
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	for _, banned := range []string{"intent", "output", "lastPrompt", "text", "content"} {
		if strings.Contains(string(blob), `"`+banned+`"`) {
			t.Fatalf("the projection carries %q: %s", banned, blob)
		}
	}
}

// The candidate list is the four fields §5.4 names, and nothing else.
func TestNameAmbiguousCandidatesCarryFourFields(t *testing.T) {
	cands := wire.NewCandidates([]wire.Session{
		{SessionID: "a", Name: "same", CWD: "/x", Kind: "interactive", PID: nil},
	})
	blob, err := json.Marshal(cands)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var back []map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(back) != 1 || len(back[0]) != 4 {
		t.Fatalf("a candidate is not the four fields: %s", blob)
	}
	for _, key := range []string{"sessionId", "name", "cwd", "kind"} {
		if _, ok := back[0][key]; !ok {
			t.Errorf("a candidate has no %q: %s", key, blob)
		}
	}
}
