package mcpserve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// SupportedProtocols is §5.4's list, and it is a CONST bumped deliberately.
//
// Revision 0 echoed whatever the client sent "because the CC version moved
// twice in one day", which conflated two independent version spaces: Claude
// Code's application version has nothing to do with the MCP protocol version,
// and echoing an unknown one advertises semantics this implementation may not
// have. The rule:
//
//	client's protocolVersion in SupportedProtocols -> return that exact version
//	otherwise                                      -> return SupportedProtocols[0]
var SupportedProtocols = []string{"2025-06-18"}

// maxLineBytes bounds one JSON-RPC line. It matches the socket's frame bound so
// that a payload one surface is willing to emit is one every surface can read.
const maxLineBytes = 1 << 20

// DefaultServerName is what serverInfo reports.
const DefaultServerName = "tt"

// instructions is the one paragraph a host may show a model before it calls
// anything. It states the boundary rather than the capability list, because the
// capability list is the tool catalogue and the boundary is not discoverable
// from it.
const instructions = "tt observes the Claude Code sessions on this machine. Every tool is read-only: " +
	"nothing here answers a prompt, sends input to a session, renames, attaches, kills, or reads a " +
	"transcript body. Ages and timestamps are computed at observation time and travel with the answer " +
	"as observedAt; nothing needs to do clock arithmetic against them."

// Snapshotter is the pull half of the incoming port — the only required one.
type Snapshotter interface {
	Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error)
}

// Enricher is optional: a source that can run §7.3's transcript pass. A source
// that cannot answers by not implementing it rather than by returning an error
// nobody can act on — the status answer is complete without a colour.
type Enricher interface {
	Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error)
}

// SourceRefresher is the preferred refresh half, for jsonout's reason: the
// incoming port's Refresh carries no SnapshotOptions, so the snapshot it
// returns is built with the defaults and would silently discard this call's
// filters. Refreshing the SOURCE and then asking the question the arguments
// describe honours them in one rebuild.
type SourceRefresher interface {
	RefreshSources(ctx context.Context, sources []string) observer.RefreshOutcome
}

// Refresher is the fallback half.
type Refresher interface {
	Refresh(ctx context.Context, sources []string) (*observer.Snapshot, observer.RefreshOutcome, error)
}

// Deps are the narrow ports. Consumer-defined, for P2 finding 2's reason: a
// local *observer.Observer and a *sockclient.Client are different types with
// different method sets, and an adapter demanding one fat interface could be
// given neither without stubs that lie.
type Deps struct {
	// Core is required.
	Core Snapshotter
	// Enrich is optional.
	Enrich Enricher
	// Refresh is optional and may satisfy either refresh interface.
	Refresh any
}

// Config is everything the server needs that is not a port.
type Config struct {
	// TT is tt's version string, resolved once by the composition root.
	TT string
	// Name is serverInfo.name. Empty means DefaultServerName.
	Name string
	// Surface describes where this process's data comes from, for
	// describe_sources. It is data rather than a branch: this package never
	// asks which SessionSource it holds.
	Surface wire.SurfaceWatch
	// Doctor runs §9.5's diagnosis, or is nil. Nil means this build does not
	// offer the `doctor` tool at all: it is dropped from tools/list and
	// refused if called anyway. A function rather than a value because a
	// diagnosis is a fresh set of reads every time — a captured report would
	// describe the machine as it was when `tt mcp` started, which for a server
	// a Claude session holds open for hours is the wrong answer by hours.
	Doctor func(ctx context.Context) observer.DoctorReport
	// PublishJobText turns the free-text job fields — `needs` and `detail` —
	// back ON for this surface.
	//
	// It is false by default, which is the conservative half of a judgement
	// call: those two strings come out of the same job record whose `intent`,
	// `output` and `suggestedReply` jobsfs refuses to read at all, and on this
	// machine `needs` measured as a verbatim restatement of an instruction the
	// user had typed. Every LOCAL surface still shows them in full — this is
	// the one surface whose audience is other agents rather than the user, so
	// it is the one where a sentence the user wrote about one session should
	// not be handed to all the others.
	//
	// With it false, `needs` and `detail` carry "[withheld: N chars]": presence
	// and length survive, so "this session needs you" still works, and the
	// sentence does not. `tt mcp --publish-job-text` is the one step that
	// reverses it.
	PublishJobText bool
	// Server reports the running observer's pid file, or nil. It is a function
	// because a server can be started or stopped while `tt mcp` is attached to
	// a Claude session, and a value captured at startup would go stale.
	Server func() *wire.PidFile
	Deps   Deps
}

// Server is one stdio MCP endpoint.
type Server struct {
	cfg Config
}

// New validates the wiring.
func New(cfg Config) (*Server, error) {
	if cfg.Deps.Core == nil {
		return nil, errors.New("mcpserve: no session source")
	}
	if cfg.Name == "" {
		cfg.Name = DefaultServerName
	}
	return &Server{cfg: cfg}, nil
}

// Serve reads newline-delimited JSON-RPC from in and writes answers to out
// until in ends or ctx is cancelled.
//
// Requests are handled SEQUENTIALLY, on this goroutine. That is a decision, not
// an oversight: every tool here is a bounded read of an already-published
// snapshot, the measured cost is microseconds unless `refresh` or
// `includeTranscript` is asked for, and one goroutine means one writer — so the
// interleaving hazard that a concurrent implementation would need a write mutex
// to avoid simply does not exist. A slow call delays this connection's next
// call and nothing else on the machine.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 4096), maxLineBytes)
	enc := json.NewEncoder(out)

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, answer := s.Handle(ctx, line)
		if !answer {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcpserve: writing a response: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("mcpserve: reading stdin: %w", err)
	}
	return nil
}

// Handle answers one line. The bool is "there is an answer to write":
// notifications are silent, which is JSON-RPC's rule and not a shortcut.
//
// It is exported so a test can drive the protocol without a pipe, and so the
// dispatch is testable as a function rather than through a goroutine.
func (s *Server) Handle(ctx context.Context, line []byte) (wire.RPCResponse, bool) {
	if !json.Valid(line) {
		return wire.NewRPCFailure(nil, wire.RPCParseError, "the line is not valid JSON"), true
	}
	req, err := wire.DecodeRPCRequest(line)
	if err != nil {
		// A malformed request still gets an id back when it had one, so a
		// client can fail the right call rather than the whole session.
		return wire.NewRPCFailure(idOf(line), wire.RPCInvalidRequest, err.Error()), true
	}

	switch req.Method {
	case "initialize":
		return s.initialize(req), true

	case "ping":
		// MCP's liveness check. An empty result is the whole of it.
		return wire.NewRPCResult(req.ID, struct{}{}), !req.IsNotification()

	case "tools/list":
		return wire.NewRPCResult(req.ID, s.tools()), !req.IsNotification()

	case "tools/call":
		return s.callTool(ctx, req), !req.IsNotification()

	default:
		if req.IsNotification() {
			// Every notification is silent, including `notifications/
			// initialized` and any this build does not know: degrade silently
			// on unknown data.
			return wire.RPCResponse{}, false
		}
		return wire.NewRPCFailure(req.ID, wire.RPCMethodNotFound,
			fmt.Sprintf("unknown method %q", req.Method)), true
	}
}

// initialize negotiates the protocol version and declares the capabilities.
func (s *Server) initialize(req wire.RPCRequest) wire.RPCResponse {
	var params wire.InitializeParams
	if len(req.Params) > 0 {
		// A params block that will not decode is not fatal: the negotiation
		// falls back to the version this build prefers, which is exactly what
		// an unrecognised version does.
		_ = json.Unmarshal(req.Params, &params)
	}
	return wire.NewRPCResult(req.ID, wire.InitializeResult{
		ProtocolVersion: negotiate(params.ProtocolVersion),
		Capabilities:    wire.MCPCapabilities{Tools: wire.MCPToolsCapability{ListChanged: false}},
		ServerInfo:      wire.MCPPeer{Name: s.cfg.Name, Version: s.cfg.TT},
		Instructions:    instructions,
	})
}

// negotiate applies §5.4's two-line rule.
func negotiate(asked string) string {
	for _, v := range SupportedProtocols {
		if v == asked {
			return v
		}
	}
	return SupportedProtocols[0]
}

// tools is the catalogue. Every tool carries §5.4's four annotations, from one
// constructor, because "this surface is read-only" is a property of the surface
// and not a per-tool choice somebody could get wrong once.
// toolNames is the catalogue this server actually offers.
func (s *Server) toolNames() []string {
	out := make([]string, 0, len(ToolNames))
	for _, name := range ToolNames {
		if name == ToolDoctor && s.cfg.Doctor == nil {
			continue
		}
		out = append(out, name)
	}
	return out
}

func (s *Server) tools() wire.ToolsListResult {
	names := s.toolNames()
	out := wire.ToolsListResult{Tools: make([]wire.Tool, 0, len(names))}
	for _, name := range names {
		out.Tools = append(out.Tools, wire.Tool{
			Name:        name,
			Description: descriptionOf(name),
			InputSchema: schemaOf(name),
			Annotations: wire.ReadOnlyAnnotations(),
		})
	}
	return out
}

// callTool dispatches one tools/call.
func (s *Server) callTool(ctx context.Context, req wire.RPCRequest) wire.RPCResponse {
	var params wire.CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return wire.NewRPCFailure(req.ID, wire.RPCInvalidParams, err.Error())
	}

	var result wire.CallToolResult
	switch params.Name {
	case ToolListSessions:
		result = s.listSessions(ctx, params.Arguments)
	case ToolGetSession:
		result = s.getSession(ctx, params.Arguments)
	case ToolSessionCounts:
		result = s.sessionCounts(ctx, params.Arguments)
	case ToolDescribeSources:
		result = s.describeSources(ctx, params.Arguments)
	case ToolDoctor:
		if s.cfg.Doctor == nil {
			// Refused as an unknown tool, not as a failed one: this server
			// never advertised it, so a caller asking for it is asking for
			// something outside the catalogue it was given.
			return wire.NewRPCFailure(req.ID, wire.RPCInvalidParams,
				fmt.Sprintf("unknown tool %q", params.Name))
		}
		result = s.doctor(ctx, params.Arguments)
	default:
		// An unknown TOOL is a protocol-level mistake rather than a bad
		// argument: the caller asked for something that is not in the
		// catalogue it was given, and there is nothing for a model to correct
		// inside the call.
		return wire.NewRPCFailure(req.ID, wire.RPCInvalidParams,
			fmt.Sprintf("unknown tool %q", params.Name))
	}
	return wire.NewRPCResult(req.ID, result)
}

// ---------------------------------------------------------------------------
// list_sessions
// ---------------------------------------------------------------------------

func (s *Server) listSessions(ctx context.Context, args json.RawMessage) wire.CallToolResult {
	var a wire.ListSessionsArgs
	if err := wire.DecodeToolArgs(args, &a); err != nil {
		return wire.NewToolRefusal(wire.ToolErrBadRequest, err.Error())
	}

	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = a.IncludeDead
	if a.IncludeUnknownLiveness != nil {
		opts.IncludeUnknownLiveness = *a.IncludeUnknownLiveness
	}
	// FALSE by default, unlike S1 (§9.2a): the enrichment pass is the only
	// expensive part and most callers want status, not colour.
	opts.IncludeTranscripts = a.IncludeTranscript
	opts.Filter.WaitingOnly = a.WaitingOnly
	opts.Filter.CWDPrefix = a.CWDPrefix
	// FALSE by default, like every other surface: a session another session
	// launched is that session's working part, and a model asking "what is
	// running" means the ones a person started.
	opts.Filter.IncludeSpawned = a.IncludeSpawned

	if a.Kind != "" {
		k, ok := parseKind(a.Kind)
		if !ok {
			return wire.NewToolRefusal(wire.ToolErrBadRequest,
				fmt.Sprintf("kind %q is not interactive, background or unknown", a.Kind))
		}
		opts.Filter.Kinds = []observer.Kind{k}
	}
	for _, raw := range a.Status {
		st, ok := parseStatus(raw)
		if !ok {
			return wire.NewToolRefusal(wire.ToolErrBadRequest,
				fmt.Sprintf("status %q is not one of the nine", raw))
		}
		opts.Filter.Statuses = append(opts.Filter.Statuses, st)
	}

	limit := 50
	if a.Limit != nil {
		limit = *a.Limit
		if limit < 1 || limit > 200 {
			return wire.NewToolRefusal(wire.ToolErrBadRequest,
				fmt.Sprintf("limit %d is outside 1..200", limit))
		}
	}

	rejected := s.refreshIfAsked(ctx, a.Refresh)

	snap, err := s.snapshot(ctx, opts)
	if err != nil {
		return wire.NewToolRefusal(wire.ToolErrUnsupported, err.Error())
	}
	out := s.encode(snap, rejected)

	truncated := 0
	if len(out.Sessions) > limit {
		truncated = len(out.Sessions) - limit
		out.Sessions = out.Sessions[:limit]
	}

	return wire.NewToolAnswer(listText(out, truncated), out)
}

// ---------------------------------------------------------------------------
// get_session
// ---------------------------------------------------------------------------

func (s *Server) getSession(ctx context.Context, args json.RawMessage) wire.CallToolResult {
	var a wire.GetSessionArgs
	if err := wire.DecodeToolArgs(args, &a); err != nil {
		return wire.NewToolRefusal(wire.ToolErrBadRequest, err.Error())
	}
	switch a.Given() {
	case 1:
	case 0:
		return wire.NewToolRefusal(wire.ToolErrBadRequest,
			"give exactly one of sessionId, pid or name")
	default:
		return wire.NewToolRefusal(wire.ToolErrBadRequest,
			"give exactly ONE of sessionId, pid and name; two selectors could disagree")
	}

	// A lookup is not a listing: `get_session` answers about the session you
	// named, including a dead one and one whose liveness could not be
	// established. Hiding a dead session behind the default filter here would
	// make "it is gone" and "it never existed" look alike, which is the
	// distinction AC1 exists to keep.
	//
	// The same holds for a launched session: asked for by id, pid or name, it
	// is answered, and its spawnedBy says whose child it is.
	opts := observer.DefaultSnapshotOptions()
	opts.IncludeDead = true
	opts.IncludeUnknownLiveness = true
	opts.Filter.IncludeSpawned = true

	snap, err := s.snapshot(ctx, opts)
	if err != nil {
		return wire.NewToolRefusal(wire.ToolErrUnsupported, err.Error())
	}
	out := s.encode(snap, nil)

	matches := make([]wire.Session, 0, 2)
	for _, sess := range out.Sessions {
		if matchesSelector(sess, a) {
			matches = append(matches, sess)
		}
	}

	switch {
	case len(matches) == 0:
		// An answer, not an error.
		return wire.NewToolAnswer(
			"no session matches",
			wire.GetSessionResult{Session: nil, Sources: out.Sources})

	case len(matches) > 1:
		// Only a NAME can be ambiguous — sessionId is the join key and a pid
		// identifies one process — so an arbitrary pick would be a
		// plausible-looking wrong answer for the one selector that is known to
		// be reused.
		payload := wire.NameAmbiguous{
			Error:      wire.ToolErrNameAmbig,
			Candidates: wire.NewCandidates(matches),
		}
		return wire.CallToolResult{
			Content: []wire.TextContent{{
				Type: "text",
				Text: fmt.Sprintf("%s: %d sessions match; name them by sessionId",
					wire.ToolErrNameAmbig, len(matches)),
			}},
			StructuredContent: payload,
			IsError:           true,
		}

	default:
		one := matches[0]
		return wire.NewToolAnswer(sessionText(one),
			wire.GetSessionResult{Session: &one, Sources: out.Sources})
	}
}

// matchesSelector applies the ONE selector the caller gave.
//
// Name matching is exact and against the CURRENT name only: formerNames exists
// precisely because names are reused, so matching it would return a session
// that is no longer the one the caller meant.
func matchesSelector(s wire.Session, a wire.GetSessionArgs) bool {
	switch {
	case a.SessionID != "":
		return s.SessionID == a.SessionID
	case a.PID != nil:
		return s.PID != nil && *s.PID == *a.PID
	case a.Name != "":
		return s.Name == a.Name
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// session_counts
// ---------------------------------------------------------------------------

func (s *Server) sessionCounts(ctx context.Context, args json.RawMessage) wire.CallToolResult {
	var a wire.RefreshArgs
	if err := wire.DecodeToolArgs(args, &a); err != nil {
		return wire.NewToolRefusal(wire.ToolErrBadRequest, err.Error())
	}

	rejected := s.refreshIfAsked(ctx, a.Refresh)

	// No enrichment: this projection answers "is anyone else blocked, and on
	// what?", and none of its ten fields comes from a transcript.
	opts := observer.DefaultSnapshotOptions()
	snap, err := s.snapshot(ctx, opts)
	if err != nil {
		return wire.NewToolRefusal(wire.ToolErrUnsupported, err.Error())
	}
	out := wire.NewToolResult(s.encode(snap, rejected))
	return wire.NewToolAnswer(countsText(out), out)
}

// ---------------------------------------------------------------------------
// describe_sources
// ---------------------------------------------------------------------------

func (s *Server) describeSources(ctx context.Context, args json.RawMessage) wire.CallToolResult {
	var a wire.NoArgs
	if err := wire.DecodeToolArgs(args, &a); err != nil {
		return wire.NewToolRefusal(wire.ToolErrBadRequest, err.Error())
	}

	snap, err := s.snapshot(ctx, observer.DefaultSnapshotOptions())
	if err != nil {
		return wire.NewToolRefusal(wire.ToolErrUnsupported, err.Error())
	}
	env := s.encode(snap, nil)

	out := wire.DescribeSourcesResult{
		Sources:    env.Sources,
		Warnings:   env.Warnings,
		Watch:      s.cfg.Surface,
		Server:     s.serverInfo(),
		NotCapable: env.NotCapable,
	}
	return wire.NewToolAnswer(sourcesText(out), out)
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func (s *Server) doctor(ctx context.Context, args json.RawMessage) wire.CallToolResult {
	var a wire.NoArgs
	if err := wire.DecodeToolArgs(args, &a); err != nil {
		return wire.NewToolRefusal(wire.ToolErrBadRequest, err.Error())
	}
	report := wire.NewDoctorReport(s.cfg.Doctor(ctx), wire.Options{TT: s.cfg.TT})
	return wire.NewToolAnswer(doctorText(report), report)
}

// doctorText is the model-facing summary. It leads with the verdict and then
// names ONLY the checks that are not ok, because a model given twenty-two
// passing lines will summarise them and a model given two failing ones will act
// on them. The full evidence is in structuredContent for anything that wants it.
func doctorText(r wire.DoctorReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d checks, %d ok, %d warn, %d fail, %d skipped (%d ms).",
		r.Verdict, len(r.Checks), r.Tally.OK, r.Tally.Warn, r.Tally.Fail, r.Tally.Skip, r.DurationMs)
	for _, c := range r.Checks {
		if c.Status == "ok" {
			continue
		}
		fmt.Fprintf(&b, "\n%s [%s] %s", strings.ToUpper(c.Status), c.ID, c.Detail)
	}
	if r.Tally.Warn == 0 && r.Tally.Fail == 0 {
		b.WriteString("\nEvery format assumption tt reads still holds on this machine.")
	}
	return b.String()
}

func (s *Server) serverInfo() *wire.PidFile {
	if s.cfg.Server == nil {
		return nil
	}
	return s.cfg.Server()
}

// ---------------------------------------------------------------------------
// the shared middle
// ---------------------------------------------------------------------------

// snapshot asks the source and runs the enrichment pass when it was asked for
// and the source can do it.
func (s *Server) snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error) {
	snap, err := s.cfg.Deps.Core.Snapshot(ctx, opts)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errors.New("the session source returned no snapshot")
	}
	if !opts.IncludeTranscripts || s.cfg.Deps.Enrich == nil {
		return snap, nil
	}
	out, err := s.cfg.Deps.Enrich.Enrich(ctx, snap, nil)
	if err != nil || out == nil {
		// Enrichment degrades per session and reports what happened in
		// sources.transcripts; a failed colour pass must not fail the status
		// answer.
		return snap, nil
	}
	return out, nil
}

// encode is the ONE place this package produces JSON about sessions, and it
// passes the SNAPSHOT'S OWN instant as `now` — never a fresh clock read.
//
// rejected carries a refresh that was refused. It is appended to the envelope's
// warnings because those advisories are REQUEST-SCOPED: this is the only
// surface that has no stderr a caller can see, so a rejection that went nowhere
// would make a degraded answer indistinguishable from a corroborated one. It is
// appended to the freshly built wire.Snapshot and never to the shared domain
// snapshot, so one caller's rejected refresh cannot leak to another.
func (s *Server) encode(snap *observer.Snapshot, rejected []observer.Advisory) wire.Snapshot {
	out := wire.NewSnapshot(snap, snap.ObservedAt, wire.Options{TT: s.cfg.TT})
	for _, a := range rejected {
		out.Warnings = append(out.Warnings, wire.Advisory{Code: a.Code, Fields: a.Fields})
	}
	// The ONE place this surface differs from the other three, and it is here
	// rather than in any tool because all four tools funnel through encode:
	// list_sessions' structuredContent, get_session's session, the human text
	// each of them renders, and session_counts' attention projection — which
	// reads a wire.Session, so it inherits this and cannot show what the JSON
	// does not have.
	if !s.cfg.PublishJobText {
		out.RedactJobText()
	}
	return out
}

// refreshIfAsked performs §7.5(d)'s round trip and returns the request-scoped
// advisories it produced.
//
// A refresh NEVER fails a call. A rate limit, a timeout and a backoff are all
// successful degraded answers: the caller gets the current snapshot plus a note
// saying corroboration was not re-read.
func (s *Server) refreshIfAsked(ctx context.Context, want bool) []observer.Advisory {
	if !want {
		return nil
	}
	switch r := s.cfg.Deps.Refresh.(type) {
	case SourceRefresher:
		return r.RefreshSources(ctx, []string{observer.SourceAgentsCLI}).Advisories
	case Refresher:
		_, outcome, err := r.Refresh(ctx, nil)
		if err != nil {
			return []observer.Advisory{observer.NewAdvisory(observer.AdvRefreshRejected, map[string]any{
				"source": observer.SourceAgentsCLI,
				"reason": observer.RejectError,
			})}
		}
		return outcome.Advisories
	default:
		return []observer.Advisory{observer.NewAdvisory(observer.AdvRefreshRejected, map[string]any{
			"source": observer.SourceAgentsCLI,
			"reason": observer.RejectUnsupported,
		})}
	}
}

// parseKind maps the schema's spelling to the domain vocabulary.
func parseKind(raw string) (observer.Kind, bool) {
	switch observer.Kind(strings.ToLower(strings.TrimSpace(raw))) {
	case observer.KindInteractive:
		return observer.KindInteractive, true
	case observer.KindBackground:
		return observer.KindBackground, true
	case observer.KindUnknown:
		return observer.KindUnknown, true
	default:
		return "", false
	}
}

// parseStatus validates against the nine the schema enumerates.
//
// Validated here, unlike the socket's filter (§5.3 B7 keeps status open because
// it is an OBSERVED set), because this schema publishes an enum: a caller that
// typed a status not in the list it was given deserves to be told, rather than
// to receive an empty answer it will read as "nothing is blocked".
func parseStatus(raw string) (observer.Status, bool) {
	want := observer.Status(strings.ToLower(strings.TrimSpace(raw)))
	for _, st := range observer.AllStatuses {
		if st == want {
			return st, true
		}
	}
	return "", false
}

// idOf digs the id out of a line that failed to parse as a request, so a client
// can match the failure to the call it made.
func idOf(line []byte) json.RawMessage {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}
	return raw["id"]
}

// ---------------------------------------------------------------------------
// the human half
// ---------------------------------------------------------------------------

// §5.4: every tool returns a COMPACT human content[0].text — counts plus the
// attention list, NEVER the full record set. A model handed 40 sessions as
// prose has been given the payload twice, and the prose is the copy that cannot
// be filtered.

func listText(s wire.Snapshot, truncated int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions (live %d, dead %d, unknown %d, attention %d) observed at %s",
		s.Counts.Total, s.Counts.Live, s.Counts.Dead, s.Counts.Unknown, s.Counts.Attention, s.ObservedAt)
	if truncated > 0 {
		fmt.Fprintf(&b, "\nsessions[] holds %d of them; %d were truncated by limit",
			len(s.Sessions), truncated)
	}
	writeAttention(&b, attentionOf(s))
	writeWarnings(&b, s.Warnings)
	return b.String()
}

func countsText(r wire.ToolResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions (live %d, dead %d, unknown %d, attention %d)",
		r.Counts.Total, r.Counts.Live, r.Counts.Dead, r.Counts.Unknown, r.Counts.Attention)
	writeAttention(&b, r.Attention)
	writeWarnings(&b, r.Warnings)
	return b.String()
}

func sessionText(s wire.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) %s", nonEmpty(s.Name, "unnamed"), s.Kind, statusOf(s))
	if s.StatusAgeMs != nil {
		fmt.Fprintf(&b, " for %s", humanMs(*s.StatusAgeMs))
	}
	fmt.Fprintf(&b, ", live %s, cwd %s", triState(s.Live), nonEmpty(s.CWD, "-"))
	if s.Needs != nil && *s.Needs != "" {
		fmt.Fprintf(&b, "\nneeds: %s", *s.Needs)
	}
	return b.String()
}

func sourcesText(r wire.DescribeSourcesResult) string {
	var b strings.Builder
	ids := make([]string, 0, len(r.Sources))
	for id := range r.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(&b, "watch %s (attached %t)", nonEmpty(r.Watch.Mode, "unknown"), r.Watch.Attached)
	if r.Watch.Note != "" {
		fmt.Fprintf(&b, ": %s", r.Watch.Note)
	}
	if r.Server != nil {
		fmt.Fprintf(&b, "\nobserver pid %d since %s", r.Server.PID, r.Server.StartedAt)
	}
	for _, id := range ids {
		src := r.Sources[id]
		fmt.Fprintf(&b, "\n%s: ok %t, stale %t", id, src.OK, src.Stale)
		if src.LastError != nil {
			fmt.Fprintf(&b, ", lastError %s", *src.LastError)
		}
	}
	if len(r.NotCapable) > 0 {
		fmt.Fprintf(&b, "\nnot capable of noticing: %s", strings.Join(r.NotCapable, ", "))
	}
	writeWarnings(&b, r.Warnings)
	return b.String()
}

// attentionOf is the attention projection of an envelope, through the one
// constructor, so the text and the structured payload cannot disagree.
func attentionOf(s wire.Snapshot) []wire.AttentionSession {
	return wire.NewToolResult(s).Attention
}

func writeAttention(b *strings.Builder, attention []wire.AttentionSession) {
	for _, a := range attention {
		fmt.Fprintf(b, "\n- %s (%s) %s", nonEmpty(a.Name, "unnamed"), a.Kind, a.Status)
		if a.WaitReason != "" {
			fmt.Fprintf(b, "/%s", a.WaitReason)
		}
		if a.BlockedForMs != nil {
			fmt.Fprintf(b, " for %s", humanMs(*a.BlockedForMs))
		}
		if a.Needs != nil && *a.Needs != "" {
			fmt.Fprintf(b, ": %s", *a.Needs)
		}
	}
}

func writeWarnings(b *strings.Builder, warnings []wire.Advisory) {
	for _, w := range warnings {
		fmt.Fprintf(b, "\nwarning: %s", w.Code)
	}
}

func statusOf(s wire.Session) string {
	if s.WaitReason == "" {
		return s.Status
	}
	return s.Status + "/" + s.WaitReason
}

// humanMs renders a pre-computed duration. It never subtracts two timestamps:
// the number it is given was computed against the snapshot's own instant.
func humanMs(ms int64) string {
	switch {
	case ms < 60_000:
		return fmt.Sprintf("%ds", ms/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm", ms/60_000)
	case ms < 86_400_000:
		return fmt.Sprintf("%dh", ms/3_600_000)
	default:
		return fmt.Sprintf("%dd", ms/86_400_000)
	}
}

// triState renders a *bool as three visibly different things. "unknown" is not
// "no", which is the distinction H4 showed this surface damages most.
func triState(b *bool) string {
	switch {
	case b == nil:
		return "unknown"
	case *b:
		return "yes"
	default:
		return "no"
	}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
