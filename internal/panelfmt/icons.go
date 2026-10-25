package panelfmt

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The status icon keys (icon_busy … icon_ended). Each names the glyph the
// panel's {status:icon} draws for one status. The value is a quoted string one
// or two display columns wide; anything else is a SOFT problem — the key is
// ignored and the panel keeps its built-in glyph — because a wrong icon never
// makes the panel unusable.
const (
	iconKeyPrefix = "icon_"
	// IconMaxCols is the icon slot's width: an emoji is two columns, and a
	// one-column glyph is padded to it by the panel so names stay aligned.
	IconMaxCols = 2
)

// iconNames are the statuses an icon key can name, in the order the README
// lists them. Set.Icons is keyed by these names, without the prefix.
var iconNames = [...]string{"busy", "work", "waiting", "blocked", "idle", "shell", "unclear", "ended"}

// IconNames returns the statuses an icon_<status> key may name, so the panel
// can hold its default glyph table to exactly the keys the parser accepts.
func IconNames() []string { return append([]string(nil), iconNames[:]...) }

// Codepoints an icon may not carry. Both change a glyph's width in ways that
// terminals (and x/ansi, which the fitter measures with) do not agree on, so a
// row would be measured at one width and painted at another.
const (
	variationSelector16 = '️' // emoji presentation selector
	zeroWidthJoiner     = '‍' // joins emoji into one ZWJ sequence
)

// iconName is the status an icon key names, and whether key is one.
func iconName(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, iconKeyPrefix)
	if !ok {
		return "", false
	}
	for _, n := range iconNames {
		if n == name {
			return n, true
		}
	}
	return "", false
}

// iconValueProblem says what is wrong with an icon value, or "" when it is
// usable. The message ends by saying the built-in glyph is used instead.
func iconValueProblem(key, val string) string {
	const fallback = "; the built-in icon is used"
	switch {
	case strings.ContainsRune(val, variationSelector16):
		return fmt.Sprintf("%s contains U+FE0F (a variation selector): terminals disagree on its width, so drop it%s", key, fallback)
	case strings.ContainsRune(val, zeroWidthJoiner):
		return fmt.Sprintf("%s contains U+200D (a zero-width joiner): terminals disagree on the width of a joined emoji, so use a single one%s", key, fallback)
	}
	if w := ansi.StringWidth(val); w < 1 || w > IconMaxCols {
		return fmt.Sprintf("%s must be 1 or 2 columns wide, not %d%s", key, w, fallback)
	}
	return ""
}

// iconLine parses the value of an icon key starting at raw[i]. It returns the
// offset after the value, or -1 when the rest of the line is not to be read
// (a malformed string, already reported as soft).
func (p *parsed) iconLine(raw string, i, line int, key, name string, probs *problems) int {
	p.iconLines[name] = line
	// A rejected value must not leave an earlier valid one standing: the
	// later line wins, as it does for every other duplicate key.
	delete(p.icons, name)
	val, _, end, bad := readString(raw, i)
	if bad != nil {
		probs.soft(line, bad.Col, fmt.Sprintf(`%s takes a quoted glyph, like %s = "*": %s; the built-in icon is used`, key, key, bad.Msg))
		return -1
	}
	if msg := iconValueProblem(key, val); msg != "" {
		probs.soft(line, runeCol(raw, i), msg)
		return end
	}
	p.icons[name] = val
	return end
}
