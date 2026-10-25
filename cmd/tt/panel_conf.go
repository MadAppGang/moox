package main

// panel_conf.go loads panel.conf, the file that says how each row of the
// session panel is drawn (architecture §2.1, §7.2).
//
// The file is optional and READ-ONLY to tt: tt never creates, fixes or
// rewrites it, and the README shows the built-in text for copying. A broken
// edit can never leave the panel without a format — panelfmt.ParseConfig
// falls back to the built-in templates on any fatal problem — and the first
// problem is said on a footer line of its own, so a typo is visible on the
// panel rather than swallowed.
//
// Reloading is followed, not watched. followConfig runs where the panel
// already wakes up: at construction (so the FIRST frame uses the file), when
// the panel opens, and on every observer event and render tick, beside
// followSavedMode. Each call is one os.Stat; the file is read and parsed only
// when its size or mtime moved. No timer is added for it.

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

// defaultPanelConf is the built-in format (§6.6), embedded as text so that
// it is parsed by exactly the code that parses a user's file. It is golden-
// tested: a change here is a change to what every panel looks like.
const defaultPanelConf = `# ~/.config/tt/panel.conf — how each row of the tt session panel is drawn.
# Edits apply on the panel's next update; ` + "`tt panel-render --check`" + ` lints this file.
# {token[:option[:option]]}   {{ and }} are literal braces   {>} is the spring (fills the gap)
# A token with nothing to say vanishes together with the separator before it.

row       = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx:nobar}"
meta      = "     {group}"

attn_row  = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx:nobar}"
attn_why  = "{waiting} · {why}"
attn_meta = "     {group}"

# Dim lines under a row, 0 to 3, indented under the code and carrying the row's
# /color bar: a band row's attn_why (why it waits), any other row the latest
# thing its session said. meta and attn_meta add one more line after them when
# there is room; the built-in ones name a band row's home group, and the real
# group of a row folded into "other". For more, try
# meta = "     {group} · {model:short} · {sub} · {apierr}".
text_lines = 3

# Context count colour: green below ctx_warn, honey up to ctx_alert, red above it.
ctx_warn  = 200k
ctx_alert = 400k

# Context window per model, for the bar ({ctx} draws the count and the bar,
# {ctxbar} the bar alone). The first rule whose pattern (path.Match, on the
# model id) matches wins; rules in your own file are tried before these.
window claude-opus-5*   = 1M
window claude-sonnet-5* = 1M
window claude-fable-5*  = 1M
window claude-*         = 200k

# {status:code} is the status code, {runtime:badge} the time since start. To add
# the context bar back, end both rows with {ctx} instead of {ctx:nobar}.
# {status:badge} joins code and runtime in one split badge. {status:icon} draws
# an emoji instead, 1 or 2 columns each (no U+FE0F, no joined emoji), set per
# status with icon_busy icon_work icon_waiting icon_blocked icon_idle icon_shell
# icon_unclear icon_ended, for example icon_idle = "·". {status:chip} is the
# first rebuild's chip.
`

// panelConfName is how the footer note names the file.
const panelConfName = "panel.conf"

// panelConfReadLimit bounds a read one byte past the engine's 64 KiB limit,
// so a larger file is read far enough to be reported as too large and no
// further.
const panelConfReadLimit = 64<<10 + 1

// panelConfFallback is appended to a fatal problem: the whole file was set
// aside, and the reader should know the rows they see are the built-in ones.
const panelConfFallback = " — using the built-in format"

// panelDefaultSet is the built-in format, parsed once. Defaults panics on any
// problem in the built-in text, which a test covers, so a bad default can
// never ship.
var panelDefaultSet = panelfmt.Defaults([]byte(defaultPanelConf), panelRegistry, panelFormatOptions())

// panelConfPath is where panel.conf lives, "" for "built-in only". It is a
// seam: TestMain sets it to "" so a developer's own file can never change a
// test.
var panelConfPath = defaultPanelConfPath

// readPanelConf reads the file, bounded. A seam so a test can count reads.
var readPanelConf = func(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, panelConfReadLimit))
}

// defaultPanelConfPath is $XDG_CONFIG_HOME/tt/panel.conf, else
// ~/.config/tt/panel.conf. A relative XDG_CONFIG_HOME is ignored, as the XDG
// specification says it must be.
func defaultPanelConfPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "tt", panelConfName)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tt", panelConfName)
}

// panelConfStamp is what decides whether the file must be read again: where
// it is, whether it is there, and its size and mtime. Plain comparable values
// (the mtime in nanoseconds, not a time.Time whose location pointer would
// make two equal instants compare unequal).
type panelConfStamp struct {
	path    string
	present bool
	mtime   int64
	size    int64
	err     string // a stat failure other than "not there"
}

// panelConf is the panel's current format and where it came from. It lives on
// panelMemo, a pointer, so it survives bubbletea's copies of the model.
type panelConf struct {
	stamp   panelConfStamp
	checked bool
	set     *panelfmt.Set // never nil
	note    string        // the first problem, formatted; "" when clean
	gen     uint64        // bumped on every applied change → panelFrameKey.confGen
}

func newPanelConf() *panelConf { return &panelConf{set: panelDefaultSet} }

// checkReload stats the file and, only when its stamp moved, reads and parses
// it again. It reports whether the format changed.
func (c *panelConf) checkReload() bool {
	st := statPanelConf(panelConfPath())
	if c.checked && st == c.stamp {
		return false
	}
	c.checked, c.stamp = true, st
	c.set, c.note = loadPanelConf(st)
	c.gen++
	return true
}

// statPanelConf is the stamp of the file at path.
func statPanelConf(path string) panelConfStamp {
	st := panelConfStamp{path: path}
	if path == "" {
		return st
	}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st
	case err != nil:
		st.err = err.Error()
		return st
	}
	st.present, st.mtime, st.size = true, info.ModTime().UnixNano(), info.Size()
	return st
}

// loadPanelConf is the format a stamp stands for, and its footer note. An
// absent file is the built-in format with nothing to say; an unreadable one
// is the built-in format with the reason.
func loadPanelConf(st panelConfStamp) (*panelfmt.Set, string) {
	switch {
	case st.err != "":
		return panelDefaultSet, panelConfName + ": " + st.err + panelConfFallback
	case !st.present:
		return panelDefaultSet, ""
	}
	src, err := readPanelConf(st.path)
	if err != nil {
		return panelDefaultSet, panelConfName + ": " + err.Error() + panelConfFallback
	}
	set := panelfmt.ParseConfig(src, panelRegistry, panelFormatOptions(), panelDefaultSet)
	return set, panelConfNote(set.Problems)
}

// panelConfNote is the one footer line a file's problems get. When any
// problem is fatal it is the first FATAL one, because that is what explains
// why the built-in format is showing; otherwise the first soft one, whose
// token is also drawn in place as {raw?}.
func panelConfNote(problems []panelfmt.Problem) string {
	for _, p := range problems {
		if p.Fatal {
			return p.Format(panelConfName) + panelConfFallback
		}
	}
	if len(problems) > 0 {
		return problems[0].Format(panelConfName)
	}
	return ""
}

// followConfig adopts an edited panel.conf. It is called at construction,
// on open, and beside every followSavedMode (R6).
func (p *panel) followConfig() {
	if p.memo == nil {
		return
	}
	if p.memo.conf == nil {
		p.memo.conf = newPanelConf()
	}
	p.memo.conf.checkReload()
}

// confSet is the format this panel draws with: the loaded one, or the
// built-in one for a panel with no memo.
func (p panel) confSet() *panelfmt.Set {
	if p.memo == nil || p.memo.conf == nil || p.memo.conf.set == nil {
		return panelDefaultSet
	}
	return p.memo.conf.set
}

// configNote is the footer line a problem in panel.conf gets, or "".
func (p panel) configNote() string {
	if p.memo == nil || p.memo.conf == nil {
		return ""
	}
	return p.memo.conf.note
}

// confGen identifies the loaded format, for the frame key.
func (p panel) confGen() uint64 {
	if p.memo == nil || p.memo.conf == nil {
		return 0
	}
	return p.memo.conf.gen
}
