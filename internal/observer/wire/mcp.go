package wire

// S2's vocabulary: JSON-RPC 2.0's envelope, MCP's initialize/tools shapes, and
// the four tool payloads.
//
// It is here rather than in mcpserve for §5.1's reason — this package is the
// ONE place anything becomes JSON, and the ban on json tags outside it is
// enforced by an AST test. What mcpserve owns instead: the transport, the
// dispatch, the tool CATALOGUE (names, descriptions and their input schemas,
// which are JSON text rather than Go structs) and the policy.
//
// The load-bearing property: `list_sessions`'s structuredContent IS a
// wire.Snapshot — the same value `tt sessions --json` prints and the same value
// the socket's snapshot frame nests. AC11's four-surface identity is therefore
// a property of that value, not a field mapping anybody maintains. Nothing in
// this file re-declares a session, a source or a count.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// JSONRPCVersion is the only version this server speaks, on both directions.
const JSONRPCVersion = "2.0"

// The JSON-RPC 2.0 error codes used by this server. They are the PROTOCOL's
// errors: a tool that refuses its arguments is NOT one of these, it is a tool
// result with isError true, because a model has to be able to read the refusal
// and fix its call (the same distinction §7.4 draws between a protocol error
// and a degraded answer).
const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCInvalidParams  = -32602
	RPCInternalError  = -32603
)

// ErrBadRPC is what a line that is not a JSON-RPC 2.0 request produces.
var ErrBadRPC = fmt.Errorf("wire: bad json-rpc request")

// RPCRequest is one line from the client.
//
// ID is a json.RawMessage because JSON-RPC allows a string, a number or null
// and the server must echo it BACK UNCHANGED. Decoding it into a Go type and
// re-encoding would turn a client's `1` into `1` and its `"1"` into `"1"` only
// by luck; keeping the bytes makes the echo exact by construction.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// IsNotification reports whether this request wants no answer. JSON-RPC says a
// notification is a request with NO id; a null id is a request with an id whose
// value is null, and both are answered by staying silent here — a server that
// replied to `"id":null` would be answering something no client is waiting for.
func (r RPCRequest) IsNotification() bool {
	id := bytes.TrimSpace(r.ID)
	return len(id) == 0 || string(id) == "null"
}

// DecodeRPCRequest parses one line.
//
// Unknown FIELDS are ignored, matching the socket's client vocabulary: a newer
// client sending a key this build does not know should still be served. A
// missing or wrong `jsonrpc`, or an empty method, is refused — those are the
// two things that distinguish a JSON-RPC request from an arbitrary object.
func DecodeRPCRequest(data []byte) (RPCRequest, error) {
	var r RPCRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return RPCRequest{}, fmt.Errorf("%w: %s", ErrBadRPC, err)
	}
	if r.JSONRPC != JSONRPCVersion {
		return RPCRequest{}, fmt.Errorf("%w: jsonrpc is %q, want %q", ErrBadRPC, r.JSONRPC, JSONRPCVersion)
	}
	if r.Method == "" {
		return RPCRequest{}, fmt.Errorf("%w: no method", ErrBadRPC)
	}
	return r, nil
}

// RPCError is the error member of a response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// RPCResponse is one answer.
//
// It marshals BY HAND rather than through struct tags, and that is the whole
// reason this type has a MarshalJSON: JSON-RPC 2.0 says a response carries
// `result` XOR `error`, and §5.1 bans omitempty across this package — so tags
// alone would emit `"error":null` on every successful call. Advisory solves the
// same problem the same way, so there is one idiom here rather than two.
type RPCResponse struct {
	ID     json.RawMessage
	Result any
	Error  *RPCError
}

// MarshalJSON writes jsonrpc, id, and exactly one of result and error.
func (r RPCResponse) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":`)
	buf.WriteString(`"` + JSONRPCVersion + `"`)
	buf.WriteString(`,"id":`)
	if len(bytes.TrimSpace(r.ID)) == 0 {
		buf.WriteString("null")
	} else {
		buf.Write(r.ID)
	}
	if r.Error != nil {
		b, err := json.Marshal(r.Error)
		if err != nil {
			return nil, fmt.Errorf("wire: encode rpc error: %w", err)
		}
		buf.WriteString(`,"error":`)
		buf.Write(b)
		buf.WriteString("}")
		return buf.Bytes(), nil
	}
	b, err := json.Marshal(r.Result)
	if err != nil {
		return nil, fmt.Errorf("wire: encode rpc result: %w", err)
	}
	buf.WriteString(`,"result":`)
	buf.Write(b)
	buf.WriteString("}")
	return buf.Bytes(), nil
}

// NewRPCResult builds a successful response for the given request id.
func NewRPCResult(id json.RawMessage, result any) RPCResponse {
	return RPCResponse{ID: id, Result: result}
}

// NewRPCFailure builds an error response.
func NewRPCFailure(id json.RawMessage, code int, message string) RPCResponse {
	return RPCResponse{ID: id, Error: &RPCError{Code: code, Message: message}}
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

// MCPPeer names a participant. Both `clientInfo` and `serverInfo` have this
// shape.
type MCPPeer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams is what the client sends. Its capabilities are deliberately
// not decoded: this server offers tools and nothing else, so there is no
// client capability it would behave differently for, and decoding a field
// nothing reads would be a shape to keep in step for no reason.
type InitializeParams struct {
	ProtocolVersion string  `json:"protocolVersion"`
	ClientInfo      MCPPeer `json:"clientInfo"`
}

// MCPToolsCapability is the tools capability. ListChanged is FALSE and says so
// explicitly: this server's tool set is fixed at build time, and a consumer
// that reads `false` knows it never has to re-list. §5.4's "MCP is pull-only"
// is the same decision one level down — nothing here ever notifies.
type MCPToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// MCPCapabilities is what the server offers. Tools only: no resources, no
// prompts, no sampling. §5.4 rejects a resource surface by name — it would
// double the schema for a push a Claude session cannot usefully react to
// mid-turn, and the socket (S3) is the push surface.
type MCPCapabilities struct {
	Tools MCPToolsCapability `json:"tools"`
}

// InitializeResult is the handshake answer.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    MCPCapabilities `json:"capabilities"`
	ServerInfo      MCPPeer         `json:"serverInfo"`
	Instructions    string          `json:"instructions"`
}

// ---------------------------------------------------------------------------
// tools/list and tools/call
// ---------------------------------------------------------------------------

// ToolAnnotations is §5.4's four hints, on EVERY tool. They are not defaults
// waiting to be overridden: this surface is read-only by decision, and a tool
// that needed different values would be a tool this observer does not offer.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

// ReadOnlyAnnotations is the one value every tool carries.
func ReadOnlyAnnotations() ToolAnnotations {
	return ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: false,
		IdempotentHint:  true,
		OpenWorldHint:   false,
	}
}

// Tool is one entry in tools/list.
//
// InputSchema is json.RawMessage because a JSON Schema is JSON, not a Go
// struct: expressing `{"type":"array","items":{"enum":[…]}}` as nested structs
// under the omitempty ban would emit a key for every schema keyword the
// vocabulary has, on every property. The schemas are literals in mcpserve,
// validated by a test that parses each one and checks the keys §5.4 specifies.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations ToolAnnotations `json:"annotations"`
}

// ToolsListResult is the tools/list answer.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams is a tools/call request's params.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// TextContent is the human half of a tool answer.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult is one tool answer: a COMPACT human summary beside the
// structured payload (§5.4). The text never carries the full record set —
// counts plus the attention list is the whole of it, because a model that is
// handed 40 sessions as prose has been given the payload twice.
type CallToolResult struct {
	Content           []TextContent `json:"content"`
	StructuredContent any           `json:"structuredContent"`
	IsError           bool          `json:"isError"`
}

// NewToolAnswer is a successful tool result.
func NewToolAnswer(text string, structured any) CallToolResult {
	return CallToolResult{
		Content:           []TextContent{{Type: "text", Text: text}},
		StructuredContent: structured,
		IsError:           false,
	}
}

// NewToolRefusal is a tool result the model can act on: isError, the reason as
// text, and the same reason structured so a program need not parse prose.
//
// It is NOT a JSON-RPC error. §5.4's `bad_request` is a refusal of ARGUMENTS,
// and a JSON-RPC error would be handed to the transport rather than to the
// caller that can correct them.
func NewToolRefusal(code, detail string) CallToolResult {
	return CallToolResult{
		Content:           []TextContent{{Type: "text", Text: code + ": " + detail}},
		StructuredContent: ToolError{Error: code, Detail: detail},
		IsError:           true,
	}
}

// ToolError is a refusal, structured.
type ToolError struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// The refusal codes this surface uses. They are the socket's vocabulary
// (§5.3's three codes) minus the ones that cannot happen on a pull surface, so
// a consumer that speaks both surfaces learns one set of words.
const (
	ToolErrBadRequest  = "bad_request"
	ToolErrUnsupported = "unsupported"
	ToolErrNameAmbig   = "name-ambiguous"
)

// ---------------------------------------------------------------------------
// tool arguments
// ---------------------------------------------------------------------------

// ListSessionsArgs is §5.4's list_sessions input.
//
// IncludeUnknownLiveness and Limit are POINTERS because their defaults are not
// their zero values: a missing includeUnknownLiveness means TRUE (M7's "default
// filtering is live != false", made visible in the schema), and a missing limit
// means 50. A non-pointer bool would silently turn every call that omitted the
// key into the one filtering rule M7 forbids.
type ListSessionsArgs struct {
	Status                 []string `json:"status"`
	Kind                   string   `json:"kind"`
	WaitingOnly            bool     `json:"waitingOnly"`
	CWDPrefix              string   `json:"cwdPrefix"`
	IncludeDead            bool     `json:"includeDead"`
	IncludeSpawned         bool     `json:"includeSpawned"`
	IncludeUnknownLiveness *bool    `json:"includeUnknownLiveness"`
	IncludeTranscript      bool     `json:"includeTranscript"`
	Refresh                bool     `json:"refresh"`
	Limit                  *int     `json:"limit"`
}

// GetSessionArgs is §5.4's get_session input: exactly one of the three.
type GetSessionArgs struct {
	SessionID string `json:"sessionId"`
	PID       *int   `json:"pid"`
	Name      string `json:"name"`
}

// Given counts how many selectors were supplied, which is what makes "exactly
// one" checkable in one place rather than in three branches.
func (a GetSessionArgs) Given() int {
	n := 0
	if a.SessionID != "" {
		n++
	}
	if a.PID != nil {
		n++
	}
	if a.Name != "" {
		n++
	}
	return n
}

// RefreshArgs is the input of the two tools that accept `refresh`.
type RefreshArgs struct {
	Refresh bool `json:"refresh"`
}

// NoArgs is the input of the tools that take none. It exists so that
// `additionalProperties: false` is ENFORCED for them too: decoding into a
// struct with no fields and DisallowUnknownFields refuses `{"limit":5}` on
// describe_sources instead of ignoring it.
type NoArgs struct{}

// DecodeToolArgs parses one tool's arguments strictly.
//
// Strict is the point: every schema in §5.4 says `additionalProperties: false`,
// and a decoder that ignored unknown keys would advertise a contract it does
// not keep — a caller misspelling `waitingOnly` would silently get every
// session instead of the three it asked for. Absent arguments (`null` or no
// key at all) are the zero value, which each tool then resolves against its
// documented defaults.
//
// The keys are checked against the struct's json names DIRECTLY rather than
// through json.Decoder.DisallowUnknownFields, because encoding/json matches
// field names CASE-INSENSITIVELY: `{"waitingonly":true}` satisfies
// DisallowUnknownFields and would be accepted by a decoder that trusted it,
// while the published schema says no such property exists. Deriving the names
// by reflection is what keeps this check from becoming a second hand-maintained
// list of the arguments.
func DecodeToolArgs(data []byte, dst any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return fmt.Errorf("wire: tool arguments: %w", err)
	}
	known := jsonFieldNames(dst)
	unknown := make([]string, 0, len(raw))
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("wire: tool arguments: unknown %s: %s",
			plural(len(unknown), "property", "properties"), strings.Join(unknown, ", "))
	}

	if err := json.Unmarshal(trimmed, dst); err != nil {
		return fmt.Errorf("wire: tool arguments: %w", err)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// tool payloads
// ---------------------------------------------------------------------------

// GetSessionResult is get_session's answer. A session that does not exist is
// `null` WITH the sources block, which is an answer rather than an error: "no
// session by that name" and "we could not read anything" must not look alike.
type GetSessionResult struct {
	Session *Session          `json:"session"`
	Sources map[string]Source `json:"sources"`
}

// Candidate is one entry of a name-ambiguous refusal.
type Candidate struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	CWD       string `json:"cwd"`
	Kind      string `json:"kind"`
}

// NameAmbiguous is §5.4's answer to a name that matches more than one session.
// Never an arbitrary pick: names are REUSED — that is why formerNames exists —
// so choosing one would be a plausible-looking wrong answer.
type NameAmbiguous struct {
	Error      string      `json:"error"`
	Candidates []Candidate `json:"candidates"`
}

// SurfaceWatch is how THIS surface gets its data, for describe_sources.
//
// It is not `sources.<id>.watch`, which describes what the OBSERVER armed and
// travels inside the envelope. This describes the attachment: whether this
// process is reading a running `tt observe` over the socket or watching for
// itself, and the one note §7.5(e) requires when a server was refused. §5.4's
// describe_sources names a `watch` and a `server` key and does not say what is
// in them; these are the two facts a consumer can act on.
type SurfaceWatch struct {
	Mode     string `json:"mode"`
	Attached bool   `json:"attached"`
	Socket   string `json:"socket"`
	Note     string `json:"note"`
}

// DescribeSourcesResult is describe_sources' answer: §5.4's five keys.
type DescribeSourcesResult struct {
	Sources    map[string]Source `json:"sources"`
	Warnings   []Advisory        `json:"warnings"`
	Watch      SurfaceWatch      `json:"watch"`
	Server     *PidFile          `json:"server"`
	NotCapable []string          `json:"notCapable"`
}

// NewToolResult is session_counts' answer, projected from an ALREADY ENCODED
// envelope.
//
// It reads a wire.Snapshot rather than a domain one for the same reason
// NewAttention does: a projection built from the domain could show a value the
// JSON surfaces do not have, and then `session_counts` and `list_sessions`
// would disagree about the same session. Here they cannot — the attention list
// is a subset of the very sessions list_sessions would have printed.
func NewToolResult(s Snapshot) ToolResult {
	out := ToolResult{
		Counts:     s.Counts,
		Attention:  make([]AttentionSession, 0),
		Sources:    s.Sources,
		Warnings:   s.Warnings,
		NotCapable: copyStrings(s.NotCapable),
	}
	for _, sess := range s.Sessions {
		if sess.Attention {
			out.Attention = append(out.Attention, NewAttention(sess))
		}
	}
	return out
}

// NewCandidates is the name-ambiguous candidate list, in the snapshot's order.
func NewCandidates(sessions []Session) []Candidate {
	out := make([]Candidate, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, Candidate{
			SessionID: s.SessionID,
			Name:      s.Name,
			CWD:       s.CWD,
			Kind:      s.Kind,
		})
	}
	return out
}
