package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// safeSID is B34's charset (R33): a session id is appended to an eval'd
// command, so anything outside it could run something.
var safeSID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// SafeSID reports whether s may be used as a session id (B34).
func SafeSID(s string) bool { return safeSID.MatchString(s) }

// Validate is the publish gate (B18): a snapshot that fails it is never
// published (B19) and never restored. Duplicates are checked by KEY, never by
// whole line. It returns every problem it finds, wrapped in ErrInvalid.
//
// A layout whose leaf count differs from the window's pane count is NOT an
// error (3.8 floating panes can legitimately differ); the engine reports it as
// a warning, because counting leaves needs the layout parser and this package
// is a leaf.
func Validate(s *Snapshot) error {
	if s == nil {
		return fmt.Errorf("%w: nil snapshot", ErrInvalid)
	}
	var probs []string
	add := func(f string, a ...any) { probs = append(probs, fmt.Sprintf(f, a...)) }

	if s.Format != FormatName {
		add("format %q", s.Format)
	}
	if s.Version < 1 || s.Version > Version {
		add("version %d", s.Version)
	}
	if len(s.Windows) == 0 {
		add("no windows")
	}
	if len(s.Panes) == 0 {
		add("no panes")
	}
	if s.Client == nil {
		add("no client record")
	}

	sessions := map[string]Session{}
	for _, se := range s.Sessions {
		if se.Name == "" {
			add("session with an empty name")
			continue
		}
		if _, dup := sessions[se.Name]; dup {
			add("duplicate session %q", se.Name)
		}
		sessions[se.Name] = se
	}
	for _, se := range s.Sessions {
		if se.GroupedWith == "" {
			continue
		}
		p, ok := sessions[se.GroupedWith]
		if !ok || p.GroupedWith != "" {
			add("session %q is grouped with %q, which is not a primary session", se.Name, se.GroupedWith)
		}
	}

	type wkey struct {
		s string
		i int
	}
	windows := map[wkey]int{}
	for _, w := range s.Windows {
		k := wkey{w.Session, w.Index}
		if _, dup := windows[k]; dup {
			add("duplicate window %s:%d", w.Session, w.Index)
		}
		windows[k] = 0
		se, ok := sessions[w.Session]
		switch {
		case !ok:
			add("window %s:%d names no session", w.Session, w.Index)
		case se.GroupedWith != "":
			add("window %s:%d belongs to grouped secondary %q", w.Session, w.Index, w.Session)
		}
		switch w.LayoutFormat {
		case LayoutV1, LayoutV2:
		default:
			add("window %s:%d layout_format %q", w.Session, w.Index, w.LayoutFormat)
		}
		// v2 iff the layout starts with '{'; a v1 layout starts with its hex
		// checksum (R3-L5).
		if isV2 := strings.HasPrefix(w.Layout, "{"); isV2 != (w.LayoutFormat == LayoutV2) {
			add("window %s:%d layout does not match layout_format %q", w.Session, w.Index, w.LayoutFormat)
		}
	}

	panes := map[string]bool{}
	for _, p := range s.Panes {
		k := PaneKey(p.Session, p.Window, p.Index)
		if p.Key != k {
			add("pane key %q does not match its fields (%s)", p.Key, k)
		}
		if panes[k] {
			add("duplicate pane %s", k)
		}
		panes[k] = true
		wk := wkey{p.Session, p.Window}
		if _, ok := windows[wk]; !ok {
			add("pane %s names no window", k)
		} else {
			windows[wk]++
		}
		if sid := p.SID(); sid != "" && !SafeSID(sid) {
			add("pane %s session id %q is not in [A-Za-z0-9_.-]", k, sid)
		}
	}
	for k, n := range windows {
		if n == 0 {
			add("window %s:%d has no panes", k.s, k.i)
		}
	}
	if len(probs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(probs, "; "))
}

// Sids is the snapshot's set of session ids, in pane order, de-duplicated.
func (s *Snapshot) Sids() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Panes {
		if sid := p.SID(); sid != "" && !seen[sid] {
			seen[sid] = true
			out = append(out, sid)
		}
	}
	return out
}

// RestoredPanes counts the rows that are restored as panes: every role but
// tt-observer, which the plan drops (§6.2 pins compare this count).
func (s *Snapshot) RestoredPanes() int {
	n := 0
	for _, p := range s.Panes {
		if p.Role != RoleTTObserver {
			n++
		}
	}
	return n
}

// Fingerprint is snapshotFP (§6.1, PR-H5): sha256 over the canonical JSON
// with created_at, producer, server and the WHOLE contents object removed,
// plus the contents file's sha256 (never its name, which carries a
// timestamp). Equal fingerprints mean "nothing to publish" (S8).
func Fingerprint(s *Snapshot) string {
	c := *s
	c.CreatedAt, c.Producer = "", ""
	c.Server, c.Contents = nil, nil
	body, err := json.Marshal(&c)
	if err != nil {
		// Marshal of this type cannot fail; a fingerprint that never matches
		// is the safe answer if it ever did (the snapshot is published).
		return "unhashable:" + err.Error()
	}
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0})
	if s.Contents != nil {
		h.Write([]byte(s.Contents.SHA256))
	}
	return hex.EncodeToString(h.Sum(nil))
}
