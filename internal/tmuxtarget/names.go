package tmuxtarget

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// How tmux stores the names and titles a restore sets (read in the 3.6a and
// 3.8 sources; CHANGES 3.6b→3.7: "Sanitize pane titles and window and session
// names more consistently and strictly"):
//
//   - Session and window names are vis-encoded on every supported version:
//     utf8_stravis(VIS_OCTAL|VIS_CSTYLE|VIS_TAB|VIS_NL) — in 3.6
//     session_check_name and window_set_name, from 3.7 clean_name. A
//     backslash is stored as two, a tab as `\t`, a newline as `\n`, other
//     control bytes and invalid UTF-8 as `\ooo`; valid UTF-8 and printable
//     ASCII (space included) are kept. A name read back from tmux is thus
//     already encoded, and creating it again verbatim encodes it a second
//     time: `a\b` → `a\\b` → `a\\\\b`, one doubling per restore (PV14
//     measured `a\b` stored `a\\b` on 3.6a and on 3.8).
//     (Exception, measured on 3.6a: `new-window -n` stores its name verbatim;
//     rename-window and automatic-rename go through window_set_name. A
//     restore sets every window name with rename-window last, so it gets the
//     encoded form there too.)
//   - Before 3.7 a session name also has ':' and '.' turned into '_'
//     (session_check_name): "a.b" is stored "a_b", and "a:b" then collides
//     with it ("duplicate session: a_b").
//   - A pane title is encoded the same way from 3.7 (screen_set_title →
//     clean_name), and stored verbatim before (3.6 screen_set_title).

// MapsSessionSeparators reports whether tmux v stores ':' and '.' in a
// session name as '_' (before 3.7).
func (v Version) MapsSessionSeparators() bool { return !v.AtLeast(3, 7) }

// EncodesTitles reports whether tmux v vis-encodes a pane title (3.7+).
func (v Version) EncodesTitles() bool { return v.AtLeast(3, 7) }

// NameArg is the argument that makes tmux v store a session or window name
// exactly as it printed it: the name decoded (UnvisName), which tmux's
// encoding turns back into the same bytes. From 3.7 tmux refuses a name with
// a control character or invalid UTF-8 ("invalid session name"), which only
// a 3.6 server stores (`\t`, `\n`, `\001`, `\377`): there only the doubled
// backslashes are decoded, and the name comes back with those escapes as
// text instead of not at all.
func NameArg(v Version, name string) string {
	d := UnvisName(name)
	if v.MapsSessionSeparators() || (utf8.ValidString(d) && !hasControl(d)) {
		return d
	}
	return unvis(name, false)
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if isControl(s[i]) {
			return true
		}
	}
	return false
}

func isControl(c byte) bool { return c < ' ' || c == 0x7f }

// StoredSessionName is the name tmux v holds after `new-session -s arg` (or
// rename-session).
func StoredSessionName(v Version, arg string) string {
	if v.MapsSessionSeparators() {
		arg = sessionSep.Replace(arg) // bytewise: invalid UTF-8 reaches VisName intact
	}
	return VisName(arg)
}

var sessionSep = strings.NewReplacer(":", "_", ".", "_")

// TitleArg is the Lit that sets a pane title back to s, where s is the title
// as tmux v reports it (#{pane_title}, which a snapshot holds): halved
// backslash pairs on 3.7+ (StoredTitle), s itself before.
func TitleArg(v Version, s string) Lit {
	if !v.EncodesTitles() {
		return Literal(s)
	}
	return StoredTitle(s)
}

// VisName is tmux's utf8_stravis(VIS_OCTAL|VIS_CSTYLE|VIS_TAB|VIS_NL)
// (utf8.c, compat/vis.c) for a name, which never holds a NUL.
func VisName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError && size > 1 {
			b.WriteString(s[i : i+size]) // a complete UTF-8 character is kept
			i += size
			continue
		}
		c := s[i]
		i++
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == ' ' || (c > ' ' && c < 0x7f):
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\a':
			b.WriteString(`\a`)
		case c == '\v':
			b.WriteString(`\v`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\f':
			b.WriteString(`\f`)
		default:
			o := strconv.FormatInt(int64(c), 8)
			b.WriteString(`\` + strings.Repeat("0", 3-len(o)) + o)
		}
	}
	return b.String()
}

// cEscapes are VIS_CSTYLE's one-letter escapes and the doubled backslash.
var cEscapes = map[byte]byte{'\\': '\\', 'n': '\n', 'r': '\r', 'b': '\b', 'a': '\a', 'v': '\v', 't': '\t', 'f': '\f'}

// UnvisName decodes what VisName encodes: VisName(UnvisName(s)) == s for any
// s VisName produced. A backslash that starts no known escape is kept, and
// `\000` is never decoded (an argv cannot carry a NUL).
func UnvisName(s string) string { return unvis(s, true) }

// unvis decodes s; with all false only the doubled backslash is decoded.
func unvis(s string, all bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		n := s[i+1]
		if i+3 < len(s) && isOctal(n) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil && v != 0 && all {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		if d, ok := cEscapes[n]; ok && (all || n == '\\') {
			b.WriteByte(d)
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
