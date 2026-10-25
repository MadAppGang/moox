package transcriptfs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/MadAppGang/moox/internal/observer"
)

// This file is the 200 KB ENRICHMENT pass: §8.3 items 3, 4, 5 and 6, and the
// three measured facts they exist for.
//
// It reads SIX things out of a transcript and nothing else:
//
//	agentColor    from `{"type":"agent-color","agentColor":"…"}`, LAST-WINS
//	apiErrors     from `isApiErrorMessage: true`, NEVER from is_error
//	sessionKind   from the record's own `sessionKind`, as corroboration
//	subagents     by COUNTING FILES in a sibling directory (subagents.go)
//	context       input + cache_creation + cache_read tokens and the bounded
//	              model id, from the NEWEST qualifying assistant `message.usage`
//	compactions   a COUNT of `compact_boundary` records, plus the newest one's
//	              `compactMetadata.postTokens`
//
// Everything else a transcript contains — every prompt, every assistant
// message's content, every tool result — is not decoded, not copied, and has
// no field on observer.TranscriptFacts to arrive in. The context read opens an
// assistant record's `message` object only to take two numbers-and-an-id out of
// it: `content` is never decoded into anything, not even a discarded string.
// That is §5.4's exposure rule made structural rather than reviewed.

// The keys this pass reads. Naming them as constants is not tidiness: three
// separate greps for `"color"` missed that the colour key is `agentColor`, and
// a literal buried in a switch is what made that possible.
const (
	// typeKey selects the record type. Reading it is what separates the
	// colour RECORD from the 58 lines on pid 98693 that merely CONTAIN the
	// string `agentColor` — a substring grep overcounts by 72 %.
	typeKey = "type"
	// agentColorType is the only record type whose colour belongs to this
	// session. Every bare `"color"` key in a transcript belongs to a SUBAGENT
	// (measured: the bare key appears in `agent-name` records, in subagent
	// transcripts under `subagents/`, and inside tool schemas) and is ignored
	// here by construction — this pass never asks for it.
	agentColorType = "agent-color"
	// agentColorKey is the colour. NOT "color" (measured fact 5).
	agentColorKey = "agentColor"
	// apiErrorKey is the API-error signal. NOT `is_error` (measured fact 11):
	// `is_error` fires on RECOVERED tool failures and appears 8 243 times
	// across 1 803 transcripts machine-wide, against 1 103 `isApiErrorMessage`
	// across 961. Counting `is_error` would be wrong by ~7.5x, and wrong in the
	// direction that reports healthy sessions as broken.
	apiErrorKey = "isApiErrorMessage"
	// sessionKindKey corroborates Kind. It never overrides it.
	sessionKindKey = "sessionKind"

	// assistantType marks a model turn: the only record that carries usage.
	assistantType = "assistant"
	// sidechainKey marks a record that belongs to a side conversation rather
	// than the main thread. Its usage is not this session's context.
	sidechainKey = "isSidechain"
	// messageKey holds the model turn. It is opened for modelKey and usageKey
	// only; its `content` is never decoded.
	messageKey = "message"
	modelKey   = "model"
	usageKey   = "usage"
	// The three counts that together are "what the model last saw". Output
	// tokens are excluded on purpose (FR6): they are what it wrote, and only
	// become context on the next turn, where they arrive as cache reads.
	inputTokensKey   = "input_tokens"
	cacheCreationKey = "cache_creation_input_tokens"
	cacheReadKey     = "cache_read_input_tokens"
	// syntheticModel is written on records the client made up (an interrupt,
	// an API error placeholder). They carry an all-zero usage, and 9 of them
	// were measured in the 40 newest tails: read as the newest usage, each
	// would report a fake context of 0.
	syntheticModel = "<synthetic>"

	// systemType + compactSubtype is a compaction boundary. Measured shape:
	// `{"type":"system","subtype":"compact_boundary","compactMetadata":
	// {"trigger":"manual","preTokens":680872,"postTokens":21149,…}}`.
	systemType     = "system"
	subtypeKey     = "subtype"
	compactSubtype = "compact_boundary"
	compactMetaKey = "compactMetadata"
	postTokensKey  = "postTokens"

	// maxPlausibleTokens bounds a context count. It is ~100x the largest
	// window any measured model has, so it refuses only a value that is not a
	// token count at all, and it keeps the three-way sum far from overflow.
	maxPlausibleTokens = 100_000_000
)

// MaxModelBytes bounds the model identifier that may cross this boundary.
//
// Model is a string field on TranscriptFacts that comes out of a transcript's
// own bytes, so — exactly like MaxAgentColorBytes — the §5.4 guarantee cannot
// rest on the type. Measured ids (claude-opus-5-5, gpt-6-astra,
// qwen/qwen3.8-max-0902, google/gemma-4-26b-a4b-qat, …) are all under 32
// bytes. An id that is too long or outside the charset is DROPPED, never
// truncated; the token counts beside it are kept.
const MaxModelBytes = 64

// MaxSessionKindBytes bounds SessionKind, the third string that comes out of
// a transcript's own bytes. Until this bound it was the one such string with
// none. Measured machine-wide: 20 820 occurrences, every one of them "bg".
// Over-long or whitespace-bearing values are dropped, like the colour.
const MaxSessionKindBytes = 32

// MaxAgentColorBytes bounds the colour string that may cross this boundary.
//
// §5.4 allows a closed set of facts out of a transcript and a test asserts that
// TranscriptFacts has no string field capable of carrying message content.
// AgentColor is a string field, so the guarantee cannot rest on the type alone:
// a vendor change (or a hand-edited transcript) that put a paragraph in
// `agentColor` would carry it onto the wire. Measured values are colour NAMES
// — purple, green, blue, yellow, pink, orange — so 64 bytes is ample for a name
// or a hex triple and far short of a sentence. An over-long or whitespace-
// bearing value is DROPPED rather than truncated: a truncated colour is a
// value we would then have to explain, and "no opinion" is the NFR's answer.
const MaxAgentColorBytes = 64

// ReadFacts is the enrichment read: a bounded tail of the transcript plus a
// count of the sibling subagents directory.
//
// tailBytes of 0 or less means FactsTailBytes, so a caller with no opinion does
// not have to hold the constant. 200 KB is a measurement: the agent-color
// record was found for all 35 transcripts on the machine that carry one, 0
// misses, against a largest file of 30.7 MB. A full scan is not an option —
// the tree is 630 MB.
//
// When the tail does not reach far enough back to see a colour record the
// answer is `ColorFound: false, AgentColor: ""` with **no error** and
// `Truncated: true`. That triple is the whole of AC12: "we did not see one" and
// "there is not one" must stay distinguishable, because the first is a reason
// to widen the tail and the second is a fact about the session.
//
// A malformed line is skipped, never fatal. One unparseable record in one
// transcript must not be able to blank the enrichment for the whole machine —
// that is the failure this repository has already shipped once.
func (s *Source) ReadFacts(ctx context.Context, loc observer.TranscriptLocation, tailBytes int) (observer.TranscriptFacts, error) {
	if err := ctx.Err(); err != nil {
		return observer.TranscriptFacts{}, err
	}
	if !loc.Found {
		// The caller already knows the target is absent and has its own
		// advisory for it. Not a failure.
		return observer.TranscriptFacts{}, nil
	}
	if tailBytes <= 0 {
		tailBytes = FactsTailBytes
	}

	rel, ok := s.relOf(loc.Path)
	if !ok {
		return observer.TranscriptFacts{}, ErrOutsideProjectsRoot
	}

	tail, reachedStart, err := s.readTail(rel, tailBytes)
	if err != nil {
		return observer.TranscriptFacts{}, err
	}

	facts := observer.TranscriptFacts{
		Path:      loc.Path,
		SizeBytes: loc.SizeBytes,
		TailBytes: len(tail),
		Truncated: !reachedStart,
	}
	scanFacts(tail, reachedStart, &facts)

	// The subagent count is a DIRECTORY LISTING, not a transcript read, and it
	// is deliberately last: it is the one fact here that does not come out of
	// the tail, and a reader of this function should see that the two are
	// independent. An absent directory leaves Subagents nil and
	// SubagentsKnown false — "we cannot say", which is not the same answer as
	// a genuine zero (AC4).
	facts.Subagents, facts.SubagentsKnown = s.countSubagents(rel)

	return facts, nil
}

// scanFacts walks the tail's lines FORWARDS and fills the fields it finds.
//
// Forwards, not backwards, for two reasons that pull the same way: apiErrors is
// a COUNT over the whole tail so every line has to be visited anyway, and
// "last-wins" for the colour is then literally the last assignment rather than
// a reversed loop that has to be read twice to be believed.
//
// The FIRST line is discarded unless the tail reached offset 0, because a tail
// taken mid-file almost always begins in the middle of a record. A half object
// would fail to decode and be skipped anyway; discarding it explicitly is what
// stops a fragment that happens to parse from ever being counted.
//
// The context count is the one fact that wants the NEWEST record rather than a
// sum or a last assignment, and the newest assistant record is often not a
// usable one (a synthetic placeholder, an all-zero usage). So the forward pass
// only notes WHERE each main-thread assistant record is, and contextFacts then
// walks those positions newest-first over the same tail. There is no fixed
// window of candidates to be evicted from: any number of unusable records
// after a good one still leaves the good one reachable.
func scanFacts(tail []byte, reachedStart bool, facts *observer.TranscriptFacts) {
	lines := bytes.Split(tail, []byte{'\n'})
	if !reachedStart && len(lines) > 0 {
		lines = lines[1:]
	}

	var (
		assistantAt   []int // line indexes of main-thread assistant records
		lastCompactAt = -1  // line index of the newest compact_boundary
		postTokens    *int64
	)

	for i, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		if isTrue(rec[apiErrorKey]) {
			facts.APIErrors++
		}
		if kind, ok := decodeString(rec[sessionKindKey]); ok && validSessionKind(kind) {
			// Last-wins, like the colour: a transcript resumed as a background
			// job carries the newer label later in the file. A label we refuse
			// does not clear one we have, by the colour's argument.
			k := kind
			facts.SessionKind = &k
		}

		recType, _ := decodeString(rec[typeKey])
		switch recType {
		case assistantType:
			// Position only. Nothing of the record is decoded here, and no
			// bytes are kept beyond the index into a tail we already hold.
			if !isTrue(rec[sidechainKey]) {
				assistantAt = append(assistantAt, i)
			}
			continue
		case systemType:
			if st, ok := decodeString(rec[subtypeKey]); ok && st == compactSubtype {
				facts.Compactions++
				lastCompactAt = i
				// Per boundary, not sticky: a newer compaction that recorded
				// no postTokens must not inherit an older one's figure.
				postTokens = compactPostTokens(rec[compactMetaKey])
			}
			continue
		case agentColorType:
		default:
			// The type gate is the whole of AC6. A record is a colour record
			// only if it SAYS it is one; a record of any other type carrying
			// an agentColor key — measured, 58 of 139 lines on pid 98693 — is
			// not one, and neither is a bare `"color"` key, which always
			// belongs to a subagent.
			continue
		}
		colour, ok := decodeString(rec[agentColorKey])
		if !ok || !validColor(colour) {
			// A colour record whose colour we will not carry does NOT clear a
			// colour we already have: an unusable newer value is no evidence
			// that the older one stopped applying.
			continue
		}
		facts.AgentColor = colour
		facts.ColorFound = true
	}

	usedAt := contextFacts(lines, assistantAt, facts)
	if lastCompactAt > usedAt {
		// The newest compaction is newer than the newest usable usage (or
		// there is no usable usage in the tail at all): the context the usage
		// describes has been replaced. postTokens is the size of what replaced
		// it; without one the honest answer is "not measurable", not a stale
		// pre-compaction figure.
		facts.ContextTokens = postTokens
	}
}

// contextFacts walks the main-thread assistant records NEWEST FIRST and takes
// the context size and model from the first one that qualifies. It returns
// that record's line index, or -1 when none qualified.
//
// A record qualifies when its model is not the synthetic placeholder and its
// usage carries all three input counts as non-negative integers summing to a
// plausible, non-zero total. The model is then kept only if validModel accepts
// it: an id we refuse costs the id, never the numbers.
//
// The walk is bounded by the tail: it touches at most every assistant record
// already in memory, and in the common case exactly one, because the newest
// record usually qualifies.
func contextFacts(lines [][]byte, assistantAt []int, facts *observer.TranscriptFacts) int {
	for k := len(assistantAt) - 1; k >= 0; k-- {
		idx := assistantAt[k]
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(bytes.TrimSpace(lines[idx]), &rec); err != nil {
			continue
		}
		// Keys only: the values stay raw bytes, so `content` is never decoded.
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(rec[messageKey], &msg); err != nil {
			continue
		}
		model, modelIsString := decodeString(msg[modelKey])
		if modelIsString && model == syntheticModel {
			continue
		}
		used, ok := contextUsed(msg[usageKey])
		if !ok {
			continue
		}
		facts.ContextTokens = &used
		if modelIsString && validModel(model) {
			facts.Model = model
		}
		return idx
	}
	return -1
}

// contextUsed is input + cache_creation + cache_read from one usage object,
// and whether that sum is a usable context size.
//
// All three counts must be present: a sum over whichever keys happened to be
// there is a number that means nothing (measured: 653 of 653 usage objects in
// 60 recent tails carry all three, across 14 model ids).
func contextUsed(raw json.RawMessage) (int64, bool) {
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(raw, &usage); err != nil || usage == nil {
		return 0, false
	}
	var used int64
	for _, key := range []string{inputTokensKey, cacheCreationKey, cacheReadKey} {
		n, ok := decodeTokenCount(usage[key])
		if !ok {
			return 0, false
		}
		used += n
	}
	if used <= 0 || used > maxPlausibleTokens {
		return 0, false
	}
	return used, true
}

// compactPostTokens is a compaction's postTokens, or nil when the metadata is
// absent, malformed, or records a count that is not a positive plausible one.
func compactPostTokens(raw json.RawMessage) *int64 {
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil || meta == nil {
		return nil
	}
	n, ok := decodeTokenCount(meta[postTokensKey])
	if !ok || n == 0 {
		return nil
	}
	return &n
}

// decodeTokenCount decodes a raw value as a JSON integer in
// [0, maxPlausibleTokens]. A string, a fraction, a negative number or null is
// not a count. Each term is bounded before any sum, so a sum cannot overflow.
func decodeTokenCount(raw json.RawMessage) (int64, bool) {
	// null has to be refused by hand: encoding/json treats it as a no-op into
	// an int64, which would read "not recorded" as a measured 0.
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if n < 0 || n > maxPlausibleTokens {
		return 0, false
	}
	return n, true
}

// isTrue reports whether a raw value is the JSON literal `true`.
//
// The check is on the literal rather than via a bool decode so that `"true"`,
// `1` and `{}` are all NOT true. A vendor that starts writing this key as a
// string has changed the format, and guessing at a changed format is the thing
// this codebase keeps measuring itself to have got wrong.
func isTrue(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "true"
}

// decodeString decodes a raw value as a JSON string, reporting false for
// absent, null, or any non-string shape.
func decodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// validColor is MaxAgentColorBytes' enforcement plus a shape check.
//
// Whitespace and control characters are refused because a colour name has
// none, and because they are what a sentence has. This is the only exposure
// bound in the package that is not structural, so it is a named function with
// a test rather than an inline length check.
func validColor(v string) bool {
	if v == "" || len(v) > MaxAgentColorBytes {
		return false
	}
	return !strings.ContainsFunc(v, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	})
}

// validSessionKind is MaxSessionKindBytes' enforcement, with validColor's
// shape rule: no whitespace and no control bytes.
func validSessionKind(v string) bool {
	if v == "" || len(v) > MaxSessionKindBytes {
		return false
	}
	return !strings.ContainsFunc(v, func(r rune) bool {
		return r <= ' ' || r == 0x7f
	})
}

// validModel is MaxModelBytes' enforcement plus a charset check.
//
// The charset is what a model id is made of — letters, digits and
// `. _ : @ / - [ ]` (the brackets for ids like `claude-opus-5-5[1m]`) — and
// nothing a sentence needs: no space, no quote, no control byte, no non-ASCII.
// Like validColor, a refused value is dropped whole rather than cut to fit.
func validModel(v string) bool {
	if v == "" || len(v) > MaxModelBytes {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("._:@/-[]", c) >= 0:
		default:
			return false
		}
	}
	return true
}
