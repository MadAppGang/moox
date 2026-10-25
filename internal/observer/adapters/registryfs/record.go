// Package registryfs reads Claude Code's own interactive session registry at
// ~/.claude/sessions/, one JSON record per live session, and returns it in the
// core's vocabulary.
//
// # Read-only, structurally
//
// The adapter holds an fs.FS, not a directory path with write helpers. An fs.FS
// has no write method, so "this adapter never writes to ~/.claude/" is a
// property of the type rather than a rule a reviewer has to check. The root path
// is carried separately, as a string, purely so advisories can name the file
// they are about.
//
// # Provenance
//
// §8.2 of the architecture plans to seed this file from ssnxd/agentbar's
// internal/registry/registry.go (MIT) — specifically its typed entry, its
// colon-safe anchored tmux regex and its kill(pid, 0) liveness. The upstream
// source was NOT available in the environment this was written in, so the code
// here is written from §8.2's description of it and from this machine's measured
// records. Two consequences, both deliberate:
//
//   - The typed entry with json tags could not be copied even if it had been
//     available: §5.1 bans json struct tags anywhere outside the wire package,
//     because one set of tags in one place is what makes the four surfaces agree.
//     Records are therefore decoded by explicit key-NAME lookup, which measured
//     fact 12 requires anyway — registry key order is not stable.
//   - The MIT attribution obligation (THIRD_PARTY.md, §8.2, phase P10) still
//     stands if and when upstream code is actually copied in. Nothing in this
//     file is copied from it today.
package registryfs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// tmuxRefPattern is anchored and colon-safe on purpose. A tmux session name may
// itself contain colons ("mag:api:worker"), so a naive SplitN(raw, ":", 2) picks
// the wrong boundary. Anchoring both ends and requiring the sigils means a
// value that does not have the documented shape fails to parse instead of
// yielding a plausible-looking wrong answer.
//
// The @ and % are PART of the window and pane identifiers (measured fact 7).
// Stripping them produces ids tmux will not match, and it fails silently — no
// error, just a pane that never resolves — which is the worst available failure
// mode for a liveness gate.
var tmuxRefPattern = regexp.MustCompile(`^(.+):(@\d+)\.(%\d+)$`)

// ParseTmux splits a registry tmux reference into its parts, keeping the
// sigils. The second return is false when the value does not have the
// documented "#{session_name}:#{window_id}.#{pane_id}" shape.
//
// A false return is NOT a reason to discard the raw value: the pane-existence
// gate matches the raw string against `tmux list-panes` output verbatim and
// never reassembles it from these parts, so an unparseable-but-present
// reference is still checkable. The parts exist for display.
func ParseTmux(raw string) (observer.TmuxRef, bool) {
	m := tmuxRefPattern.FindStringSubmatch(raw)
	if m == nil {
		return observer.TmuxRef{Raw: raw}, false
	}
	return observer.TmuxRef{Raw: raw, Session: m[1], Window: m[2], Pane: m[3]}, true
}

// The registry's key names, declared once. Every one of these is read by NAME.
// Nineteen are universal (measured on 33/33 live records) and two —
// waitingFor and formerNames — are optional.
const (
	keyPID                 = "pid"
	keySessionID           = "sessionId"
	keyStatus              = "status"
	keyWaitingFor          = "waitingFor"
	keyStatusUpdatedAt     = "statusUpdatedAt"
	keyName                = "name"
	keyNameSource          = "nameSource"
	keyFormerNames         = "formerNames"
	keyCWD                 = "cwd"
	keyStartedAt           = "startedAt"
	keyTmux                = "tmux"
	keyVersion             = "version"
	keyMessagingSocketPath = "messagingSocketPath"
	// kind and jobId are present on every record of a background session's
	// own process ("bg" plus the job it serves), and kind alone on the rest.
	keyKind  = "kind"
	keyJobID = "jobId"
)

// parseRecord decodes one registry record by key name.
//
// It returns an error only for the two conditions that make a record unusable
// rather than merely incomplete: the bytes are not a JSON object (a torn read,
// §6.5), and there is no join key. Everything else degrades to a zero value,
// because the formats here are undocumented and the NFR is "prefer no opinion
// over a wrong answer".
func parseRecord(path string, data []byte) (observer.RegistryRecord, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return observer.RegistryRecord{}, fmt.Errorf("registry record %s: %w", path, err)
	}

	rec := observer.RegistryRecord{Path: path}

	rec.SessionID = jsonString(raw[keySessionID])
	if rec.SessionID == "" {
		// A record with no sessionId cannot be joined with anything and must
		// not be given an invented id — a derived-id assumption is the bug
		// class measured fact 6 records. Counted as a parse error.
		return observer.RegistryRecord{}, fmt.Errorf("registry record %s: no %s", path, keySessionID)
	}

	pid, okPID := jsonInt(raw[keyPID])
	if !okPID || pid <= 0 {
		// The pid is half of the interactive liveness gate (fact 8) and is
		// universal on every measured record. Without it there is nothing to
		// ask kill(2) about, so the record is reported as a parse error rather
		// than as a session whose liveness can never be decided.
		return observer.RegistryRecord{}, fmt.Errorf("registry record %s: no usable %s", path, keyPID)
	}
	rec.PID = pid

	rec.Status = jsonString(raw[keyStatus])
	rec.WaitingFor = jsonString(raw[keyWaitingFor])
	rec.StatusUpdatedAt = jsonEpochMillis(raw[keyStatusUpdatedAt])
	rec.Name = jsonString(raw[keyName])
	rec.NameSource = jsonString(raw[keyNameSource])
	rec.FormerNames = jsonFormerNames(raw[keyFormerNames])
	rec.CWD = jsonString(raw[keyCWD])
	rec.StartedAt = jsonEpochMillis(raw[keyStartedAt])
	rec.Tmux = jsonString(raw[keyTmux])
	if rec.Tmux != "" {
		ref, _ := ParseTmux(rec.Tmux)
		rec.TmuxParts = &ref
	}
	rec.Version = jsonString(raw[keyVersion])
	rec.MessagingSocketPath = jsonString(raw[keyMessagingSocketPath])
	rec.Kind = jsonString(raw[keyKind])
	rec.JobID = jsonString(raw[keyJobID])

	return rec, nil
}

// jsonString reads a string value, returning "" for an absent key, a JSON null,
// or a value of any other type. A number where a string was expected is a
// format change, not a crash.
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

// jsonInt reads an integer value. The second return distinguishes "absent or
// not a number" from a genuine zero, which matters for pid.
func jsonInt(v json.RawMessage) (int, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// jsonEpochMillis reads one of the registry's epoch-millisecond timestamps and
// converts it once, here, to the domain's representation: UTC, truncated to
// millisecond precision. Nothing downstream needs to know which source a
// timestamp came from.
//
// Nil for an absent key, a null, a non-number, or a non-positive value. A zero
// epoch would otherwise report a session as having been in its current status
// since 1970, which is a wrong answer where nil is an honest one.
func jsonEpochMillis(v json.RawMessage) *time.Time {
	ms, ok := jsonInt64(v)
	if !ok || ms <= 0 {
		return nil
	}
	t := observer.TruncateTime(time.UnixMilli(ms))
	return &t
}

func jsonInt64(v json.RawMessage) (int64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

// jsonFormerNames reads the optional formerNames key.
//
// MEASURED DIVERGENCE FROM THE ARCHITECTURE, and the reason this is not a plain
// []string decode. The architecture (§3.1, §3.4) and the domain both describe
// formerNames as a list of names, and the wire field is []string. On this
// machine it is a list of OBJECTS:
//
//	"formerNames": [{"name":"claudish-monitor","until":1789696384602,
//	                 "sessionId":"371ebab5-…"}]
//
// measured on 2 of 33 live records. A []string decode of that returns an error,
// and an adapter that treated the error as "record unreadable" would drop
// exactly the sessions that have been renamed.
//
// Both shapes are therefore accepted and reduced to the names, in record order.
// The `until` and `sessionId` siblings are dropped rather than surfaced: adding
// them would mean a new wire shape, and no acceptance criterion asks for them.
// Anything that is neither shape yields nil, silently, per the NFR.
func jsonFormerNames(v json.RawMessage) []string {
	if len(v) == 0 {
		return nil
	}

	var asStrings []string
	if err := json.Unmarshal(v, &asStrings); err == nil {
		return asStrings
	}

	var asObjects []map[string]json.RawMessage
	if err := json.Unmarshal(v, &asObjects); err != nil {
		return nil
	}
	out := make([]string, 0, len(asObjects))
	for _, o := range asObjects {
		if name := jsonString(o[keyName]); name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
