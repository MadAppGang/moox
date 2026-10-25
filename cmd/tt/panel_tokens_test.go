package main

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// §2.7: control characters become spaces, whitespace runs collapse, the ends
// are trimmed — so no ESC reaches the terminal and the width is what is drawn.
func TestSanitizeTextLeavesNothingThatCouldMoveTheCursor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"madbench", "madbench"},
		{"", ""},
		{"   ", ""},
		{"a\tb", "a b"},
		{"line one\r\nline two", "line one line two"},
		{"  padded  ", "padded"},
		{"many    spaces", "many spaces"},
		{"\x1b]0;evil title\x07name", "]0;evil title name"},
		{"\x1b[31mred\x1b[0m", "[31mred [0m"},
		{"c1\u0085next\u009bx", "c1 next x"},
		{"del\x7fx", "del x"},
		{"名前 🚀", "名前 🚀"},
	} {
		got := sanitizeText(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, r := range got {
			if isControlRune(r) {
				t.Errorf("sanitizeText(%q) kept control %U", tc.in, r)
			}
		}
	}
}

// The registry the parser checks is exactly the table the renderer draws
// from: every token has a draw function, and every §3 name is known.
func TestTheRegistryIsTheTokenTable(t *testing.T) {
	want := []string{"gutter", "status", "age", "runtime", "waiting", "name", "group", "project", "tmux", "pane", "colour", "color",
		"ctx", "ctxbar", "tokens", "model", "compact", "wait", "needs", "why", "sub", "apierr", "live", "kind", "id"}
	if len(panelRegistry) != len(want) || len(panelTokens) != len(want) {
		t.Errorf("registry %d, table %d, §3 lists %d", len(panelRegistry), len(panelTokens), len(want))
	}
	for _, name := range want {
		if _, ok := panelRegistry[name]; !ok {
			t.Errorf("§3 token %q is not in the registry", name)
		}
		if tok, ok := panelTokens[name]; !ok || tok.draw == nil {
			t.Errorf("§3 token %q has no draw function", name)
		}
	}
	if len(panelDefaultSet.Problems) != 0 {
		t.Errorf("the built-in panel.conf has problems: %v", panelDefaultSet.Problems)
	}
}

// Chip label and tone for every status, with and without attention. Tone
// follows Attention on every row (R5); an attention row's word is upper case.
func TestTheStatusChipFollowsAttentionOnEveryStatus(t *testing.T) {
	for _, st := range observer.AllStatuses {
		for _, attn := range []bool{false, true} {
			for _, live := range []*bool{ptr(true), nil} {
				s := observer.Session{Status: st, Attention: attn, Live: live}
				label, tone := statusLabel(s), statusTone(s)
				if attn != tone.loud() {
					t.Errorf("%s attention=%v live=%v: tone %s, loud must equal attention", st, attn, live, tone)
				}
				if attn && label != strings.ToUpper(label) || !attn && label != strings.ToLower(label) {
					t.Errorf("%s attention=%v: label %q has the wrong case", st, attn, label)
				}
				if ansi.StringWidth(label) > chipLabelCols || label == "" {
					t.Errorf("%s: label %q does not fit the chip", st, label)
				}
			}
		}
	}
	if got := statusTone(observer.Session{Status: observer.StatusBlocked, Attention: true}); got != chipCoral {
		t.Errorf("blocked + attention is %s, want coral", got)
	}
	if got := statusTone(observer.Session{Status: observer.StatusWaiting, Attention: true}); got != chipHoney {
		t.Errorf("waiting + attention is %s, want honey", got)
	}
	if got := statusLabel(observer.Session{Status: "compacting"}); got != "comp" {
		t.Errorf("an unseen status shows %q, want its first four columns", got)
	}
}

// tokenRow is a session that gives every token something to say.
func tokenRow(now time.Time) panelRow {
	since := now.Add(-104 * time.Minute)
	used := int64(248_000)
	return panelRow{
		class: groupNeedsYou, band: true, label: "authflow",
		s: observer.Session{
			SessionID: "0123456789abcdef", Name: "db-migrate", Kind: observer.KindBackground,
			Status: observer.StatusBlocked, Attention: true, StatusSince: &since, Live: ptr(true),
			WaitReason: observer.WaitReasonInputNeeded, ShortID: ptr("be9663df"),
			Needs: ptr("confirm the plan"), CWD: "/nowhere/authflow",
			Tmux:       &observer.TmuxRef{Raw: "authflow:@3.%9", Session: "authflow", Pane: "%9"},
			Advisories: []observer.Advisory{{Code: "liveness-unknown-no-evidence"}},
			Transcript: &observer.TranscriptFacts{
				AgentColor: "purple", ColorFound: true, ContextTokens: &used,
				Model: "claude-opus-5-5-20260901", Compactions: 2, APIErrors: 3,
				Subagents: ptr(4), SubagentsKnown: true,
			},
		},
	}
}

// renderTokens renders one template over a row, as plain text, wide.
func renderTokens(t *testing.T, tpl string, r panelRow, now time.Time) string {
	t.Helper()
	set := panelfmt.ParseConfig([]byte("row = \""+tpl+"\"\n"), panelRegistry, panelFormatOptions(), panelDefaultSet)
	if len(set.Problems) != 0 {
		t.Fatalf("%q: %v", tpl, set.Problems)
	}
	rc := &rowContext{now: now, ctx: set.Ctx, icons: set.Icons, project: func(cwd string) string {
		return map[string]string{"/nowhere/authflow": "authflow"}[cwd]
	}}
	line, ok := set.Row.Line(200, rowResolver(&r, rc))
	if !ok {
		return ""
	}
	return strings.TrimRight(ansi.Strip(line), " ")
}

// What each §3 token and its options draw.
func TestEveryTokenDrawsWhatTheRegistrySays(t *testing.T) {
	now := fleetNow
	r := tokenRow(now)
	for _, tc := range []struct{ tpl, want string }{
		{"{status}", " BLCK   1h "},
		{"{status:noage}", " BLCK "},
		{"{status:word}", "blocked"},
		{"{status:quiet}", " BLCK   1h "},
		{"{status:chip}", " BLCK   1h "},
		{"{status:icon}", "⛔"},
		{"{status:icon:quiet}", "⛔"},
		{"{age}", "1h44m"},
		{"{age:short}", " 1h"},
		{"{age:short:quiet}", " 1h"},
		{"{gutter}", "▌"},
		{"{gutter:char=|}", "|"},
		{"{name}", "db-migrate"},
		{"{name:colour:bold}", "db-migrate"},
		{"{group}", "authflow"},
		{"{project}", "authflow"},
		{"{tmux}", "authflow"},
		{"{pane}", "%9"},
		{"{pane:full}", "authflow:@3.%9"},
		{"{colour}", "purple"},
		{"{color}", "purple"},
		{"{ctx}", " 248k"},
		{"{ctx:plain}", "248k"},
		{"{tokens}", "248k"},
		{"{model}", "claude-opus-5-5-20260901"},
		{"{model:short}", "opus-5-5"},
		{"{compact}", "⟳2"},
		{"{wait}", "input-needed"},
		{"{needs}", "confirm the plan"},
		{"{why}", "confirm the plan"},
		{"{sub}", "4 sub"},
		{"{sub:n}", "4"},
		{"{apierr}", "3 api err"},
		{"{apierr:n}", "3"},
		{"{kind}", "bg"},
		{"{id}", "be9663df"},
	} {
		if got := renderTokens(t, tc.tpl, r, now); got != strings.TrimRight(tc.want, " ") {
			t.Errorf("%s = %q, want %q", tc.tpl, got, tc.want)
		}
	}
	// {live} only speaks when liveness is unknown.
	if got := renderTokens(t, "{live}", r, now); got != "" {
		t.Errorf("{live} on a known-live row = %q", got)
	}
	r.s.Live = nil
	if got := renderTokens(t, "{name} {live}", r, now); got != "db-migrate live? liveness-unknown-no-evidence" {
		t.Errorf("{live} on an unknown row = %q", got)
	}
}

// A token with nothing to say is Empty, and so vanishes with its separator.
func TestTokensWithNothingToSayVanishWithTheirSeparator(t *testing.T) {
	since := fleetNow.Add(-time.Minute)
	bare := panelRow{class: groupIdle, s: observer.Session{
		SessionID: "0123456789abcdef", Name: "bare", Status: observer.StatusIdle, StatusSince: &since,
	}}
	empty := []string{"group", "project", "tmux", "pane", "colour", "tokens", "model", "compact", "wait",
		"needs", "why", "sub", "apierr", "kind", "id", "ctx:hide"}
	for _, tok := range empty {
		if got := renderTokens(t, "{name} · {"+tok+"}", bare, fleetNow); got != "bare" {
			t.Errorf("{%s} on a bare row left %q", tok, got)
		}
	}
	// The ctx badge says "not measured" rather than vanishing.
	if got := renderTokens(t, "{name}{>}{ctx}", bare, fleetNow); !strings.HasSuffix(got, "—") {
		t.Errorf("{ctx} with no usage = %q, want the — mark", got)
	}
	// A needs on a session that is not blocked is not said.
	bare.s.Needs = ptr("stale text")
	if got := renderTokens(t, "{name} · {needs}", bare, fleetNow); got != "bare" {
		t.Errorf("{needs} on an idle row = %q", got)
	}
	// The group a name already says is not repeated.
	bare.label = "bare"
	if got := renderTokens(t, "{name} · {group}", bare, fleetNow); got != "bare" {
		t.Errorf("{group} repeating the name = %q", got)
	}
}

// §8 width row: whatever a session's text holds — wide glyphs, emoji, TAB, CR,
// an OSC or CSI sequence — every rendered line fits the width, is sanitised,
// and never splits a glyph. A property test over random names and widths.
func TestEveryRowLineFitsItsWidthWhateverTheSessionText(t *testing.T) {
	rng := rand.New(rand.NewSource(20260925))
	alphabet := []string{"a", "z", "-", " ", "名", "前", "🚀", "é", "\t", "\r", "\n", "\x1b]0;x\x07", "\x1b[31m", "\u009b", "·"}
	word := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	p := newPanel(nil, newPanelClock(fleetNow))
	p.now = fleetNow
	set := panelDefaultSet
	for i := 0; i < 400; i++ {
		r := tokenRow(fleetNow)
		r.s.Name, r.label = word(rng.Intn(40)), word(rng.Intn(12))
		r.s.Needs = ptr(word(rng.Intn(120)))
		r.s.Tmux.Pane = word(rng.Intn(8))
		r.band = i%2 == 0
		p.width = 10 + rng.Intn(111)
		lines := p.renderRow(set, &r, p.rowContext(set, nil))
		all := []string{lines.head, lines.meta}
		for _, text := range lines.text {
			all = append(all, text...)
		}
		for _, l := range all {
			plainLine := ansi.Strip(l)
			if w := ansi.StringWidth(l); w > p.width {
				t.Fatalf("width %d: %d columns: %q", p.width, w, plainLine)
			}
			if strings.ContainsAny(plainLine, "\x1b\t\r\n\u009b\a") {
				t.Fatalf("width %d: unsanitised text reached the line: %q", p.width, plainLine)
			}
			if strings.ContainsRune(plainLine, '�') {
				t.Fatalf("width %d: a glyph was split: %q", p.width, plainLine)
			}
		}
		if lines.head == "" {
			t.Fatalf("width %d: a row with no head line", p.width)
		}
	}
}

// §2.4's name room, as feedback 4's L2 row draws the head: a long name absorbs
// the slack and is cut first, and the status code, the runtime and the count
// never give way. The head holds exactly the gutter, the code badge, a space,
// the runtime badge, two spaces, the name badge's lead cell, the name, the
// spring's one column and the 6-column count badge. With the bar opted back
// into ({ctx}), the name gives up the bar's six columns and nothing else.
func TestALongNameGivesWayAndTheBadgeAndTheCountDoNot(t *testing.T) {
	p := newPanel(nil, newPanelClock(fleetNow))
	p.width, p.now = panelWidth, fleetNow
	since := fleetNow.Add(-3 * time.Minute)
	started := fleetNow.Add(-(3*time.Hour + 12*time.Minute))
	used := int64(417_000)
	s := observer.Session{SessionID: "x", Name: strings.Repeat("n", 40), Status: observer.StatusBusy, StatusSince: &since, StartedAt: &started,
		Transcript: &observer.TranscriptFacts{Model: "claude-opus-5-5", ContextTokens: &used}}
	fl := parseFrame([]string{"title", strings.TrimRight(ansi.Strip(p.rowHead(s, "orbit-demo")), " ")})
	r := fl[1]
	if r.kind != lineRow || r.code != "RUN" || r.runtime != "3h12m" || r.ctx != "417k" || !strings.HasSuffix(r.name, "…") {
		t.Fatalf("row parsed as %+v", r)
	}
	if w, want := ansi.StringWidth(r.name), panelWidth-frameNameStart-1-ctxBadgeWidth; w != want {
		t.Errorf("the name was cut to %d columns, want the %d the badges and the count leave", w, want)
	}

	conf := newConfFixture(t)
	conf.write(t, `row = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx}"`+"\n", fleetNow)
	p.followConfig()
	fl = parseFrame([]string{"title", strings.TrimRight(ansi.Strip(p.rowHead(s, "orbit-demo")), " ")})
	if r := fl[1]; r.kind != lineRow || r.ctx != "417k ▰▰▱▱▱" {
		t.Fatalf("with the bar opted into, the row parsed as %+v", r)
	} else if w, want := ansi.StringWidth(r.name), panelWidth-frameNameStart-1-ctxWidth; w != want {
		t.Errorf("with the bar, the name was cut to %d columns, want %d", w, want)
	}
}
