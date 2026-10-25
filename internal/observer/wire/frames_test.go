package wire

// P7's additions to the codec: the frame constructors, the client vocabulary
// and the pid file. The existing wire_test.go covers the session/snapshot
// round trip, which these do not touch.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
)

// Every constructor names its own type, and nothing else does. A frame built
// with the wrong type string is a frame a consumer switches on incorrectly,
// and the mistake is invisible at review time.
func TestEveryFrameConstructorNamesItsOwnType(t *testing.T) {
	cases := []struct {
		want  string
		frame Frame
	}{
		{FrameHello, NewHello(Options{TT: "v1"}, 7, nil, nil, nil)},
		{FrameSnapshot, NewSnapshotFrame(nil, Snapshot{})},
		{FrameSessionAdded, NewSessionAdded(Session{SessionID: "s"})},
		{FrameSessionChanged, NewSessionChanged(Session{SessionID: "s"}, []string{"status"})},
		{FrameSessionRemoved, NewSessionRemoved("s", string(observer.KindInteractive), observer.RemovedFilteredOut)},
		{FrameSources, NewSourcesFrame(nil, nil, CauseRefresh)},
		{FrameWatchDegraded, NewWatchDegraded("", WatchDegradedOnDemand, "boom")},
		{FramePong, NewPong(nil, nil)},
		{FrameResync, NewResync(ReasonSlowConsumer)},
		{FrameError, NewError(nil, CodeBadRequest, "nope")},
		{FrameBye, NewBye(ReasonShutdown)},
	}
	for _, c := range cases {
		if c.frame.Type != c.want {
			t.Errorf("constructor produced type %q, want %q", c.frame.Type, c.want)
		}
	}
	if len(cases) != 11 {
		t.Fatalf("%d server frame types covered; §5.3 defines 11", len(cases))
	}
}

// The omitempty ban, on a frame: a pong carries `sessions`-less nulls rather
// than omitting the keys. P0 left this open for P7 and P7 keeps the ban, so
// the assertion belongs with the decision.
func TestAFrameEncodesEveryKeyIncludingTheOnesItsTypeDoesNotUse(t *testing.T) {
	data, err := json.Marshal(NewPong(nil, nil))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	typ := reflect.TypeOf(Frame{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := got[name]; !ok {
			t.Errorf("a pong frame omits %q", name)
		}
	}
	if string(got["session"]) != "null" || string(got["snapshot"]) != "null" {
		t.Fatalf("an unused payload is not null: %s", data)
	}
}

// hello declares both version spaces. §7.5(e) has a client refuse to attach on
// a mismatch of either, so a frame that carried only one would make half that
// check impossible.
func TestHelloCarriesSchemaProtocolAndTheDeclaredLists(t *testing.T) {
	f := NewHello(Options{TT: "v0.4.0"}, 41122,
		[]string{CapSnapshot, CapEnrich}, []string{"time-transitions"},
		map[string]Source{observer.SourceRegistry: {ID: observer.SourceRegistry}})

	if f.Schema != Schema || f.Protocol != Protocol {
		t.Fatalf("hello carries schema %d protocol %d, want %d/%d", f.Schema, f.Protocol, Schema, Protocol)
	}
	if f.TT != "v0.4.0" || f.PID != 41122 {
		t.Fatalf("hello carries tt %q pid %d", f.TT, f.PID)
	}
	if !reflect.DeepEqual(f.Capabilities, []string{CapSnapshot, CapEnrich}) {
		t.Fatalf("capabilities = %v", f.Capabilities)
	}
	if !reflect.DeepEqual(f.NotCapable, []string{"time-transitions"}) {
		t.Fatalf("notCapable = %v", f.NotCapable)
	}

	// The lists are COPIED: a caller that reuses its slice must not be able to
	// mutate a frame it already handed over.
	caps := []string{CapPing}
	g := NewHello(Options{}, 1, caps, nil, nil)
	caps[0] = "mutated"
	if g.Capabilities[0] != CapPing {
		t.Fatal("NewHello aliased the caller's slice")
	}
}

// The unknown-field detector reaches INSIDE a snapshot frame's nested
// envelope. Without the third level, a vendor-driven addition would be named
// when read from `tt sessions --json` and silently dropped when read from the
// socket — the same bytes, two answers, and P8 decodes only the socket.
func TestAnUnknownFieldInsideASnapshotFrameIsNamed(t *testing.T) {
	raw := []byte(`{"type":"snapshot","seq":1,"reqId":null,
	  "snapshot":{"schema":1,"tt":"v1","observedAt":"2026-09-18T02:40:11.204Z",
	    "somethingNew":42,
	    "sessions":[{"sessionId":"s1","alsoNew":"x"}]}}`)

	_, notes, err := DecodeFrame(raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	named := map[string]bool{}
	for _, n := range notes {
		if n.Code != observer.AdvWireUnknownField {
			t.Fatalf("advisory %q, want %q", n.Code, observer.AdvWireUnknownField)
		}
		field, _ := n.Fields["field"].(string)
		named[field] = true
	}
	for _, want := range []string{"snapshot.somethingNew", "sessions[].alsoNew"} {
		if !named[want] {
			t.Fatalf("the unknown field %q was not named; got %v", want, named)
		}
	}
}

// ---------------------------------------------------------------------------
// the client vocabulary
// ---------------------------------------------------------------------------

func TestClientFilterConvertsToTheCoresFilter(t *testing.T) {
	f := &ClientFilter{
		Kind:        []string{"background"},
		Status:      []string{"waiting", "blocked"},
		CWDPrefix:   "/home/user/dotfiles",
		WaitingOnly: true,
	}
	got, err := f.ToDomain()
	if err != nil {
		t.Fatalf("ToDomain: %v", err)
	}
	want := observer.Filter{
		Kinds:       []observer.Kind{observer.KindBackground},
		Statuses:    []observer.Status{observer.StatusWaiting, observer.StatusBlocked},
		CWDPrefix:   "/home/user/dotfiles",
		WaitingOnly: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filter = %+v, want %+v", got, want)
	}
}

// A nil or empty filter is the zero Filter, which matches EVERYTHING: a
// `{"type":"subscribe"}` with no filter widens rather than narrowing to
// nothing.
func TestAnAbsentClientFilterMatchesEverything(t *testing.T) {
	var none *ClientFilter
	got, err := none.ToDomain()
	if err != nil {
		t.Fatalf("ToDomain: %v", err)
	}
	if !reflect.DeepEqual(got, observer.Filter{}) {
		t.Fatalf("filter = %+v, want the zero Filter", got)
	}
}

// Kinds are validated because there are exactly three; statuses are not,
// because status is an OBSERVED set (B7) and a client filtering for one this
// build has never seen must get an empty stream rather than a protocol error.
func TestKindsAreValidatedAndStatusesAreNot(t *testing.T) {
	if _, err := (&ClientFilter{Kind: []string{"intercative"}}).ToDomain(); err == nil {
		t.Fatal("an unknown kind was accepted")
	} else if !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("error = %v, want it to name the kind", err)
	}

	got, err := (&ClientFilter{Status: []string{"contemplating"}}).ToDomain()
	if err != nil {
		t.Fatalf("an unseen status was refused: %v", err)
	}
	if len(got.Statuses) != 1 || got.Statuses[0] != observer.Status("contemplating") {
		t.Fatalf("statuses = %v, want the literal preserved", got.Statuses)
	}

	// All three kinds are spellable, including unknown — which is what a
	// consumer asks for when it wants the sessions we understand least.
	for _, k := range []observer.Kind{observer.KindInteractive, observer.KindBackground, observer.KindUnknown} {
		if _, err := (&ClientFilter{Kind: []string{string(k)}}).ToDomain(); err != nil {
			t.Fatalf("kind %q was refused: %v", k, err)
		}
	}
}

// Enrich is tri-state on the client frame: absent, false and true are three
// different requests (§7.5f), and a plain bool would collapse the first two.
func TestEnrichIsTriStateOnAClientFrame(t *testing.T) {
	cases := []struct {
		line string
		want *bool
	}{
		{`{"type":"snapshot"}`, nil},
		{`{"type":"snapshot","enrich":false}`, ptrTo(false)},
		{`{"type":"snapshot","enrich":true}`, ptrTo(true)},
	}
	for _, c := range cases {
		f, err := DecodeClientFrame([]byte(c.line))
		if err != nil {
			t.Fatalf("DecodeClientFrame(%s): %v", c.line, err)
		}
		switch {
		case c.want == nil && f.Enrich != nil:
			t.Fatalf("%s decoded enrich as %v, want absent", c.line, *f.Enrich)
		case c.want != nil && (f.Enrich == nil || *f.Enrich != *c.want):
			t.Fatalf("%s decoded enrich as %v, want %v", c.line, f.Enrich, *c.want)
		}
	}
}

// An unknown FIELD is ignored — a newer client still gets its subscription —
// while a line with no type at all is a bad request, because there is nothing
// to dispatch on and silence would leave the client unable to tell "refused"
// from "lost".
func TestDecodeClientFrameIgnoresUnknownFieldsAndRefusesATypelessLine(t *testing.T) {
	f, err := DecodeClientFrame([]byte(`{"type":"ping","reqId":"r-1","fromTheFuture":{"a":[1,2]}}`))
	if err != nil {
		t.Fatalf("an unknown field was refused: %v", err)
	}
	if f.Type != FramePing || f.ReqID == nil || *f.ReqID != "r-1" {
		t.Fatalf("frame = %+v", f)
	}

	for _, line := range []string{`{}`, `{"type":"  "}`, `not json`} {
		if _, err := DecodeClientFrame([]byte(line)); err == nil {
			t.Fatalf("%s was accepted", line)
		} else if !strings.Contains(err.Error(), "bad request") {
			t.Fatalf("%s produced %v, want a bad request", line, err)
		}
	}
}

// ---------------------------------------------------------------------------
// the pid file
// ---------------------------------------------------------------------------

func TestThePidFileRoundTripsAndRefusesPidZero(t *testing.T) {
	in := PidFile{
		PID:       41122,
		StartedAt: "2026-09-18T02:40:11.204Z",
		Socket:    "/home/user/.local/state/tt/observer.sock",
		TT:        "v0.4.0",
	}
	data, err := EncodePidFile(in)
	if err != nil {
		t.Fatalf("EncodePidFile: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("the pid file is not newline-terminated: %q", data)
	}
	out, err := DecodePidFile(data)
	if err != nil {
		t.Fatalf("DecodePidFile: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip gave %+v, want %+v", out, in)
	}

	// Pid 0 is refused rather than returned: signalling 0 signals the whole
	// process group, so "the file is malformed" must not become "kill
	// everything".
	for _, bad := range []string{`{"pid":0}`, `{"pid":-1}`, `{`, `{"pid":"nope"}`} {
		if _, err := DecodePidFile([]byte(bad)); err == nil {
			t.Fatalf("%s was accepted as a pid file", bad)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }
