package mcpserve

// The tool catalogue: four names, four descriptions and four input schemas.
//
// The schemas are JSON TEXT rather than Go structs, and deliberately so. A
// JSON Schema is JSON; expressing one as nested Go structs under §5.1's
// omitempty ban would emit a key for every schema keyword the vocabulary has on
// every property, and §5.4 specifies these schemas as literals that a reviewer
// should be able to diff against the document by eye. TestEveryToolSchemaIsWell
// Formed parses each one and asserts the properties, the enums, the defaults
// and `additionalProperties: false`.
//
// The names are §5.4's: `list_sessions`, `get_session`, `session_counts`,
// `describe_sources`. They carry no `mcp__…__` prefix — that prefix is applied
// by the HOST when it exposes a server's tools, and a server that baked it in
// would produce `mcp__tt__mcp__tt__list_sessions` on any host that does.

import "encoding/json"

// The tool names.
const (
	ToolListSessions    = "list_sessions"
	ToolGetSession      = "get_session"
	ToolSessionCounts   = "session_counts"
	ToolDescribeSources = "describe_sources"
	// ToolDoctor is §9.5's, added by P10. It is advertised ONLY when a
	// diagnostic function is wired (Config.Doctor); a build without one does
	// not list it, because telling a model its diagnostics ran when they did
	// not is worse than not offering them.
	ToolDoctor = "doctor"
)

// listSessionsSchema is §5.4's schema, verbatim in structure, with the
// descriptions a model actually reads.
//
// Two defaults are the whole of M7 and §9.2(a) made visible rather than buried:
// `includeUnknownLiveness` defaults TRUE, so a session whose liveness cannot be
// established is never hidden; `includeTranscript` defaults FALSE, which
// differs from `tt sessions --json` on purpose — the enrichment pass is the only
// expensive thing tt does and most callers want status, not colour.
const listSessionsSchema = `{
  "type": "object",
  "properties": {
    "status": {
      "type": "array",
      "items": {"type": "string", "enum": ["idle","busy","shell","waiting","blocked","working","done","failed","unknown"]},
      "description": "keep only sessions in one of these statuses"
    },
    "kind": {
      "type": "string",
      "enum": ["interactive","background","unknown"],
      "description": "keep only interactive or only background sessions"
    },
    "waitingOnly": {"type": "boolean", "default": false, "description": "keep only sessions that need attention"},
    "cwdPrefix": {"type": "string", "description": "keep only sessions whose cwd is under this path"},
    "includeDead": {"type": "boolean", "default": false, "description": "include sessions with positive evidence of death"},
    "includeSpawned": {"type": "boolean", "default": false, "description": "include sessions another live session launched (spawnedBy set), e.g. an MCP helper's headless child"},
    "includeUnknownLiveness": {"type": "boolean", "default": true, "description": "include sessions whose liveness could not be established"},
    "includeTranscript": {"type": "boolean", "default": false, "description": "run the transcript enrichment pass (agent colour, api error count, subagent count)"},
    "refresh": {"type": "boolean", "default": false, "description": "force a ` + "`claude agents --json`" + ` corroboration spawn (~130ms) before answering"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "default": 50, "description": "truncate sessions[]; counts always describe the whole filtered view"}
  },
  "additionalProperties": false
}`

// getSessionSchema takes exactly one selector. Zero or two is a bad_request:
// answering either would be a guess at which session was meant.
const getSessionSchema = `{
  "type": "object",
  "properties": {
    "sessionId": {"type": "string", "description": "the session id, the join key"},
    "pid": {"type": "integer", "description": "the process id of an interactive session"},
    "name": {"type": "string", "description": "the session's CURRENT name; former names are never matched"}
  },
  "oneOf": [{"required": ["sessionId"]}, {"required": ["pid"]}, {"required": ["name"]}],
  "additionalProperties": false
}`

const sessionCountsSchema = `{
  "type": "object",
  "properties": {
    "refresh": {"type": "boolean", "default": false, "description": "force a corroboration spawn before answering"}
  },
  "additionalProperties": false
}`

const describeSourcesSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

// doctorSchema takes nothing. A doctor with options would be a doctor a caller
// could accidentally narrow, and a narrowed diagnosis reported as a whole one
// is the failure this tool exists to prevent.
const doctorSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

// The descriptions. They say what the tool answers and what it will never do,
// because "read-only" is the property a model should not have to infer from an
// annotation it may not be shown.
const (
	listSessionsDescription = "List the Claude Code sessions on this machine, with status, liveness, " +
		"how long each has been in that status, and what a blocked one needs. Read-only: it observes, " +
		"it never answers a prompt, sends input, or changes any session. Returns the same envelope " +
		"`tt sessions --json` prints, with sessions filtered."

	getSessionDescription = "Look up ONE session by sessionId, pid or name (exactly one). Returns the session " +
		"and the source reports, or session: null when nothing matches — which is an answer, not an error. " +
		"A name matching more than one session is refused with the candidates rather than resolved by guessing."

	sessionCountsDescription = "Answer \"is anyone else blocked, and on what?\" without the full record set: " +
		"the counts by status, kind and liveness, plus one compact entry per session needing attention."

	describeSourcesDescription = "Report what this observer can and cannot see: each source's freshness, " +
		"errors and watch accounting, the warnings, whether this process is attached to a running " +
		"`tt observe` or watching for itself, and what it is structurally not capable of noticing."

	doctorDescription = "Diagnose the FORMATS rather than the sessions: read one live example of each " +
		"file Claude Code writes and report, per assumption, whether it still holds — ok, warn " +
		"(a feature degrades), fail (a feature is broken) or skip (the check could not run). " +
		"Use this when tt's answers look wrong or empty rather than when a session does. " +
		"Read-only, and it spawns `claude agents --json` exactly once. Takes a few hundred " +
		"milliseconds, or a few seconds if that binary is slow."
)

// schemaOf is the raw schema for a tool name, or nil.
func schemaOf(name string) json.RawMessage {
	switch name {
	case ToolListSessions:
		return json.RawMessage(listSessionsSchema)
	case ToolGetSession:
		return json.RawMessage(getSessionSchema)
	case ToolSessionCounts:
		return json.RawMessage(sessionCountsSchema)
	case ToolDescribeSources:
		return json.RawMessage(describeSourcesSchema)
	case ToolDoctor:
		return json.RawMessage(doctorSchema)
	default:
		return nil
	}
}

// descriptionOf is the description for a tool name.
func descriptionOf(name string) string {
	switch name {
	case ToolListSessions:
		return listSessionsDescription
	case ToolGetSession:
		return getSessionDescription
	case ToolSessionCounts:
		return sessionCountsDescription
	case ToolDescribeSources:
		return describeSourcesDescription
	case ToolDoctor:
		return doctorDescription
	default:
		return ""
	}
}

// ToolNames is the catalogue, in the order tools/list reports it: the one a
// caller reaches for first, then the narrowing lookup, then the projection,
// then the diagnostic.
//
// `doctor` is §9.5's, and it is LAST for the same reason it is last in the
// user-facing flags: it answers a question about tt itself rather than about
// the machine's sessions, and a model reaching for it first would be reaching
// past every tool that answers what was actually asked.
//
// It is in the catalogue but not necessarily in a given server's tools/list:
// Server.toolNames drops it when no diagnostic function is wired, so a build
// that cannot diagnose does not claim it can.
var ToolNames = []string{
	ToolListSessions,
	ToolGetSession,
	ToolSessionCounts,
	ToolDescribeSources,
	ToolDoctor,
}
