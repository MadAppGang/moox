package jobsfs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// The job record's key names, declared once and read BY NAME. Key order is not
// stable (measured fact 12 for the registry; the same JSON writer produces
// both trees), and §5.1 bans json struct tags anywhere outside the wire
// package, so a typed decode is unavailable even where it would be tempting.
//
// Nineteen of these are universal over the 7 measured records; name,
// nameSource, cliVersion, tokens, needs and reapedMidWorkAt are optional.
const (
	keySessionID       = "sessionId"
	keyDaemonShort     = "daemonShort"
	keyState           = "state"
	keyDetail          = "detail"
	keyNeeds           = "needs"
	keyTempo           = "tempo"
	keyTokens          = "tokens"
	keyCWD             = "cwd"
	keyName            = "name"
	keyNameSource      = "nameSource"
	keyCLIVersion      = "cliVersion"
	keyChildren        = "children"
	keyCreatedAt       = "createdAt"
	keyUpdatedAt       = "updatedAt"
	keyReapedMidWorkAt = "reapedMidWorkAt"
	keyLinkScanPath    = "linkScanPath"
	keyResumeSessionID = "resumeSessionId"
)

// The keys a job record carries that this adapter DELIBERATELY never reads.
// Declared so the "no user content leaves the record" test can name them, and
// so that a later reader can see the omission is a decision rather than an
// oversight: `intent` and `output` carry the user's own prompt and the agent's
// assistant messages (§11 assumption 8), and `suggestedReply` carries a draft
// reply written for the user. observer.JobRecord has no field for any of them,
// which is the real guarantee — a field that exists is a field a later phase
// can leak.
//
// `needs` and `detail` are NOT on this list, and that is now a decision with a
// second half rather than an omission. They are read, because a blocked
// agent's `needs` is the whole answer to "what is it waiting for" on every
// surface the user reads. But they carry the same class of content — a live
// value measured as "open new terminal and test `ck` command; report if it
// works or breaks", which is an instruction the user typed — so the boundary
// for them is drawn at the AUDIENCE instead: wire.Snapshot.RedactJobText
// withholds both from the MCP surface, whose readers are other agents, while
// every local surface shows them in full. `tt mcp --publish-job-text` is the
// one switch that turns that off.
var NeverRead = []string{"intent", "output", "suggestedReply", "respawnFlags", "providerEnv"}

// parseRecord decodes one state.json by key name.
//
// It returns an error only for the two conditions that make a record unusable
// rather than merely incomplete: the bytes are not a JSON object (a truncated
// or half-written file), and there is no join key. Everything else degrades to
// a zero value, because the format is undocumented and the NFR is "prefer no
// opinion over a wrong answer".
//
// Note what is NOT validated here: `state`. An unrecognised state is mapped to
// StatusUnknown with the literal kept in RawStatus, and §3.5 rule 7 answers
// live: null for it. Refusing the record instead would make a vendor adding a
// fifth state delete three background agents from the output.
func parseRecord(dir, path string, data []byte) (observer.JobRecord, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return observer.JobRecord{}, fmt.Errorf("job record %s: %w", path, err)
	}

	rec := observer.JobRecord{Dir: dir, Path: path}

	rec.SessionID = jsonString(raw[keySessionID])
	if rec.SessionID == "" {
		// The join key is inside the record and is NEVER derived from the
		// directory name. The directory is an 8-char prefix; reconstructing a
		// UUID from it is not possible, and inventing one is the bug class
		// measured fact 6 records.
		return observer.JobRecord{}, fmt.Errorf("job record %s: no %s", path, keySessionID)
	}

	rec.DaemonShort = jsonString(raw[keyDaemonShort])
	rec.State = jsonString(raw[keyState])
	rec.Detail = jsonString(raw[keyDetail])
	rec.Needs = jsonString(raw[keyNeeds])
	rec.Tempo = jsonString(raw[keyTempo])
	rec.Tokens = jsonInt64Ptr(raw[keyTokens])
	rec.CWD = jsonString(raw[keyCWD])
	rec.Name = jsonString(raw[keyName])
	rec.NameSource = jsonString(raw[keyNameSource])
	rec.CLIVersion = jsonString(raw[keyCLIVersion])
	rec.Children = jsonChildren(raw[keyChildren])
	rec.CreatedAt = jsonRFC3339(raw[keyCreatedAt])
	rec.UpdatedAt = jsonRFC3339(raw[keyUpdatedAt])
	rec.ReapedMidWorkAt = jsonRFC3339(raw[keyReapedMidWorkAt])
	rec.LinkScanPath = jsonString(raw[keyLinkScanPath])
	rec.ResumeSessionID = jsonString(raw[keyResumeSessionID])

	return rec, nil
}

// jsonString reads a string value, returning "" for an absent key, a JSON
// null, or a value of any other type. A number where a string was expected is
// a format change, not a crash.
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

// jsonInt64Ptr reads an optional integer. Nil for absent, null, or anything
// that is not an integral number — including the object shape, because a token
// COUNT summed out of a breakdown object would be a number we invented.
func jsonInt64Ptr(v json.RawMessage) *int64 {
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
	return &i
}

// jsonRFC3339 reads one of the job record's timestamps. Unlike the registry's
// epoch millis, these are already RFC3339 strings with milliseconds
// ("2026-08-17T08:09:36.697Z", measured on 7/7 records), and they are converted
// once — here — to the domain's representation: UTC, truncated to millisecond
// precision.
//
// Nil for an absent key, a null, a non-string, or an unparseable value. A
// timestamp we cannot read is null rather than the zero time, because a session
// reported as being in its current status since year 1 is a wrong answer where
// null is an honest one.
func jsonRFC3339(v json.RawMessage) *time.Time {
	s := jsonString(v)
	if s == "" {
		return nil
	}
	t, err := observer.ParseTime(s)
	if err != nil {
		return nil
	}
	if t.UnixMilli() <= 0 {
		return nil
	}
	return &t
}

// jsonChildren reads the optional `children` key.
//
// MEASURED DIVERGENCE FROM THE ARCHITECTURE, and this is the second time the
// same class of assumption has been wrong (formerNames was the first).
//
// §3.4 lists `Children` under "jobs win — no other source carries them" and the
// domain field is []string, documented as "child session ids". P0's finding 9
// recorded that none of the seven measured records carried any, and asked P3 to
// confirm against a real one. One now does, and it is NOT a list of session
// ids:
//
//	"children": [{"id":"158","href":"https://github.com/…/pull/158","kind":"pr"}]
//
// They are ARTIFACTS the job produced — a pull request, in the measured case —
// not child sessions. Reducing them to their `id` would publish "158" in a
// field documented as carrying session ids, which is the worst available
// outcome: a plausible-looking wrong answer.
//
// So: both shapes are accepted, and the object shape is reduced to its `href`
// when it has one, falling back to `kind:id`. An href is self-describing and
// cannot be mistaken for a session id. The domain field's comment is corrected
// to match. Anything that is neither shape yields nil, silently, per the NFR.
func jsonChildren(v json.RawMessage) []string {
	if len(v) == 0 {
		return nil
	}

	var asStrings []string
	if err := json.Unmarshal(v, &asStrings); err == nil {
		if len(asStrings) == 0 {
			return nil
		}
		return asStrings
	}

	var asObjects []map[string]json.RawMessage
	if err := json.Unmarshal(v, &asObjects); err != nil {
		return nil
	}
	out := make([]string, 0, len(asObjects))
	for _, o := range asObjects {
		if href := jsonString(o["href"]); href != "" {
			out = append(out, href)
			continue
		}
		kind, id := jsonString(o["kind"]), jsonString(o["id"])
		switch {
		case kind != "" && id != "":
			out = append(out, kind+":"+id)
		case id != "":
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
