// Package mcpserve is S2: the MCP server behind `tt mcp`.
//
// Stdio transport, JSON-RPC 2.0, four tools, all of them READ-ONLY.
//
// # It is a driving adapter
//
// It holds narrow slices of the incoming port and nothing else: no filesystem
// source, no socket, no clock arithmetic and no encoder. Every byte it emits
// about a session comes from wire.NewSnapshot — `list_sessions`'s
// structuredContent IS the envelope `tt sessions --json` prints and the socket
// nests — so AC11's four-surface agreement is a property of that value rather
// than a field mapping somebody maintains. There is no json struct tag in this
// package, which §5.1's AST audit enforces.
//
// # What it deliberately does not offer
//
// No tool writes, injects a prompt, renames, attaches, kills, or reads a
// transcript body (§5.4). Job `intent`, `output` and timeline `text` never
// leave the process, and neither does any transcript message field — only
// `agentColor`, `apiErrors`, `subagents`, `sessionKind`, `sizeBytes`,
// `contextTokens`, `model`, `compactions` and the newest timestamp ever leave
// `transcriptfs` (counts, flags and bounded identifiers), which is a property
// of observer.TranscriptFacts and is asserted there rather than filtered here.
//
// No push, either. MCP tools have no server→client streaming; resources plus
// notifications could technically push, but a Claude session cannot usefully
// react mid-turn and a resource surface would double the schema for no
// consumer. The socket (S3) is the push surface; this is the pull surface.
//
// # Where its data comes from
//
// A SessionSource, and it cannot tell which one (§7.5a). `tt mcp` dials the
// observer socket first and falls back to its own on-demand reads; this package
// contains no branch on which of the two it got. The one place the difference
// is visible is `describe_sources`, whose `watch` block SAYS which it is —
// because a consumer deciding whether to trust an age needs to know whether
// something is pushing.
//
// # Timers and the clock
//
// Neither. Every answer is encoded with the SNAPSHOT'S OWN instant as `now`,
// never a fresh clock read: `statusAgeMs == observedAt − statusSince` is an
// identity §9.2(c) asserts, and a second clock read here would break it by
// however long the read took — a defect that passes every frozen-clock fixture
// and fails only in production.
package mcpserve
