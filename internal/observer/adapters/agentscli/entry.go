package agentscli

// Decoding `claude agents --json`, by key name, against output measured today
// rather than against the architecture's description of it.
//
// WHAT WAS MEASURED (2026-09-18, 39 entries, the whole union):
//
//	cwd        string   39/39
//	kind       string   39/39   "interactive" | "background"
//	name       string   39/39
//	sessionId  string   39/39   the join key
//	startedAt  NUMBER   39/39   EPOCH MILLISECONDS, not RFC3339
//	pid        number   36/39   interactive only
//	status     string   36/39   interactive only
//	id         string    3/39   background only, the 8-char short id
//	state      string    3/39   background only
//
// Three of those are traps this file exists to not fall into:
//
//  1. THE PAYLOAD IS A FLAT ARRAY. Not {"agents": [...]}. A struct decode
//     against an object shape would yield zero entries and OK true, which reads
//     as "the vendor lists nothing" — positive evidence of death for every
//     background job on the machine.
//  2. `startedAt` IS EPOCH MILLIS. The job records' timestamps are RFC3339
//     strings and this one is a number; a shared RFC3339 helper returns nil for
//     all 39 entries, silently.
//  3. `state` AND `status` ARE DIFFERENT FIELDS. Background entries carry
//     `state`, interactive ones carry `status`, and they are kept in separate
//     fields all the way into the domain so the core's two mapping tables stay
//     separate. Folding them into one field is the conflation §3.3 refuses.
//
// `waitingFor` is decoded although no live entry carries it right now: §0.3
// measured 1 of 34 entries carrying `waitingFor: "input needed"` on 2026-09-17,
// on the one entry whose status was `waiting`. A field present only while
// waiting is exactly the field a "not reproducible today" reading would drop.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// The entry keys, declared once and read by name. §5.1 bans json struct tags
// outside the wire package, so a typed decode is unavailable even where it
// would be convenient — which is just as well, since key order is not stable
// and the union is not uniform across the two populations.
const (
	keySessionID  = "sessionId"
	keyID         = "id"
	keyPID        = "pid"
	keyKind       = "kind"
	keyStatus     = "status"
	keyState      = "state"
	keyWaitingFor = "waitingFor"
	keyName       = "name"
	keyCWD        = "cwd"
	keyStartedAt  = "startedAt"
)

// decodeEntries parses the vendor's listing.
//
// It fails only when the bytes are not a JSON array — the one condition that
// makes the whole listing unusable. An individual entry with no sessionId is
// SKIPPED rather than fatal: the join key is the one field nothing can be
// reconstructed from, and one malformed entry must not blank the corroboration
// for the other 38.
func decodeEntries(data []byte) ([]observer.AgentsEntry, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("agentscli: the listing is not a JSON array: %w", err)
	}

	out := make([]observer.AgentsEntry, 0, len(items))
	for _, raw := range items {
		id := jsonString(raw[keySessionID])
		if id == "" {
			continue
		}
		out = append(out, observer.AgentsEntry{
			SessionID:  id,
			ID:         jsonString(raw[keyID]),
			PID:        jsonIntPtr(raw[keyPID]),
			Kind:       jsonString(raw[keyKind]),
			Status:     jsonString(raw[keyStatus]),
			State:      jsonString(raw[keyState]),
			WaitingFor: jsonString(raw[keyWaitingFor]),
			Name:       jsonString(raw[keyName]),
			CWD:        jsonString(raw[keyCWD]),
			StartedAt:  jsonEpochMillis(raw[keyStartedAt]),
		})
	}

	// Sorted by session id ascending, as AgentsSource requires. The vendor's own
	// order is background-first today, which is a fact about today: every
	// "first in a fixed order wins" rule downstream needs the order to be OURS.
	sort.SliceStable(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// jsonString reads a string value, returning "" for an absent key, a null, or a
// value of another type. A number where a string was expected is a format
// change, not a crash.
func jsonString(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// jsonIntPtr reads an optional integer. Nil for absent, null or non-integral,
// which is what makes "this entry has no pid" — the whole of how a background
// agent is recognised — distinguishable from "pid 0".
func jsonIntPtr(v json.RawMessage) *int {
	if len(v) == 0 {
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	pid := int(i)
	return &pid
}

// jsonEpochMillis reads `startedAt`, which is epoch MILLISECONDS on 39 of 39
// measured entries.
//
// Nil for absent, null, non-integral or non-positive. A session reported as
// started at the Unix epoch is a wrong answer where null is an honest one, and
// the registry adapter takes the same position on the same shape.
func jsonEpochMillis(v json.RawMessage) *time.Time {
	if len(v) == 0 {
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return nil
	}
	ms, err := n.Int64()
	if err != nil || ms <= 0 {
		return nil
	}
	t := observer.TruncateTime(time.UnixMilli(ms))
	return &t
}
