package main

// panel_icons_test.go covers the icon row of feedback 2, which feedback 3
// made an opt-in ({status:icon}; the built-in row is now the status badge,
// panel_badges_test.go): the status is an emoji icon in a fixed two-column
// slot, and the age beside it is plain text that carries the attention cue.
// The frames here are drawn with iconPanelConf, feedback 2's templates.

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/panelfmt"
)

// iconPanelConf is the feedback 2 row, which a user can opt back into.
const iconPanelConf = `row       = "{gutter}{status:icon} {age:short}  {name:colour} · {group}{>}{ctx:nobar}"
attn_row  = "{gutter}{status:icon} {age:short}  {name:colour}{>}{ctx:nobar}"
`

// iconNameStart is the display column an icon row's name starts in: the
// gutter, the two-column icon, a space, the 3-column age and two spaces.
const iconNameStart = 1 + iconCols + 1 + chipAgeCols + 2

// iconRow reads an icon row's first line: its icon (pad trimmed), age and
// name. ok is false for any other line.
func iconRow(l string) (icon, age, name string, ok bool) {
	c := displayCells(l)
	if len(c) <= iconNameStart || (c[0] != "▌" && c[0] != " ") || c[1] == " " ||
		c[1+iconCols] != " " || c[iconNameStart-1] != " " || c[iconNameStart] == " " {
		return "", "", "", false
	}
	icon = strings.TrimSpace(strings.Join(c[1:1+iconCols], ""))
	age = strings.TrimSpace(strings.Join(c[2+iconCols:2+iconCols+chipAgeCols], ""))
	name = strings.Join(c[iconNameStart:], "")
	if i := strings.Index(name, "  "); i >= 0 {
		name = name[:i]
	}
	name, _, _ = strings.Cut(name, " · ")
	return icon, age, name, icon != "" && age != ""
}

// iconFrameRow is the icon row naming name in a plain frame.
func iconFrameRow(lines []string, name string) (icon, age string, ok bool) {
	for _, l := range lines {
		if ic, ag, n, isRow := iconRow(l); isRow && n == name {
			return ic, ag, true
		}
	}
	return "", "", false
}

// The built-in glyphs are exactly the keys panel.conf accepts, and each is
// one Emoji_Presentation codepoint two columns wide, with no variation
// selector and no joiner: the rules a user's icon is held to, and then some.
func TestTheBuiltInIconsAreSingleTwoColumnEmoji(t *testing.T) {
	keys := make([]string, 0, len(defaultStatusIcons))
	for k := range defaultStatusIcons {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := panelfmt.IconNames()
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("defaultStatusIcons has %v, panel.conf accepts %v", keys, want)
	}
	var conf strings.Builder
	for _, k := range want {
		g := defaultStatusIcons[k]
		if utf8.RuneCountInString(g) != 1 || ansi.StringWidth(g) != iconCols {
			t.Errorf("icon %s = %q (%U): %d runes, %d columns; want one codepoint of %d", k, g, []rune(g), utf8.RuneCountInString(g), ansi.StringWidth(g), iconCols)
		}
		if strings.ContainsAny(g, "️‍") {
			t.Errorf("icon %s = %U carries a selector or joiner", k, []rune(g))
		}
		conf.WriteString("icon_" + k + ` = "` + g + "\"\n")
	}
	if set := parsePanelConfForTest(t, conf.String()); len(set.Icons) != len(want) {
		t.Errorf("the built-in glyphs written as panel.conf keys parse to %v", set.Icons)
	}
}

// Every status, including one this build has never seen, draws an icon; the
// two ended statuses share one, and an unseen status is unclear.
func TestEveryStatusDrawsAnIcon(t *testing.T) {
	want := map[observer.Status]string{
		observer.StatusBusy: iconBusy, observer.StatusWorking: iconWork,
		observer.StatusWaiting: iconWaiting, observer.StatusBlocked: iconBlocked,
		observer.StatusIdle: iconIdle, observer.StatusShell: iconShell,
		observer.StatusUnknown: iconUnclear, observer.StatusDone: iconEnded,
		observer.StatusFailed: iconEnded, "compacting": iconUnclear,
	}
	for _, st := range observer.AllStatuses {
		if _, ok := want[st]; !ok {
			t.Errorf("status %q has no expected icon in this test", st)
		}
	}
	for st, key := range want {
		s := observer.Session{Status: st}
		if got := iconStatusOf(s); got != key {
			t.Errorf("%s draws the %s icon, want %s", st, got, key)
		}
		if g := statusIcon(s, nil); g != defaultStatusIcons[key] || g == "" {
			t.Errorf("%s draws %q with no panel.conf, want the built-in %q", st, g, defaultStatusIcons[key])
		}
		if g := statusIcon(s, map[string]string{key: "*"}); g != "*" {
			t.Errorf("%s draws %q, want panel.conf's *", st, g)
		}
	}
}

// The width contract of the icon slot: every head line the icon row and
// attn_row draw — for every status, with and without Attention, with the
// built-in emoji, with one-column glyphs from panel.conf, and with a mix — is
// at most its width in display columns, and wherever the row has room its
// name starts in column 9 whatever the icon.
func TestIconRowsFitTheirWidthAndStartTheirNamesInOneColumn(t *testing.T) {
	sets := map[string]*panelfmt.Set{
		"built-in emoji": parsePanelConfForTest(t, iconPanelConf),
		"one-column": parsePanelConfForTest(t, iconPanelConf+`icon_busy = "*"
icon_work = "+"
icon_waiting = "~"
icon_blocked = "!"
icon_idle = "."
icon_shell = "$"
icon_unclear = "?"
icon_ended = "x"
`),
		"mixed": parsePanelConfForTest(t, iconPanelConf+"icon_idle = \"·\"\nicon_shell = \"🐚\"\n"),
	}
	statuses := append(slices.Clone(observer.AllStatuses), "compacting")
	since := fleetNow.Add(-13 * time.Minute)
	for name, set := range sets {
		rc := &rowContext{now: fleetNow, ctx: set.Ctx, icons: set.Icons, project: func(string) string { return "" }}
		for _, st := range statuses {
			for _, attn := range []bool{false, true} {
				s := observer.Session{SessionID: "id-" + string(st), Name: "row-" + string(st), Status: st, Attention: attn, StatusSince: &since}
				for _, band := range []bool{false, true} {
					r := panelRow{s: s, class: panelGroupOf(s), band: band, label: "grp"}
					tpl := set.Row
					if band {
						tpl = set.AttnRow
					}
					for w := 1; w <= 80; w++ {
						line, ok := tpl.Line(w, rowResolver(&r, rc))
						if !ok {
							t.Fatalf("%s %s: no line at %d", name, st, w)
						}
						if got := ansi.StringWidth(line); got > w {
							t.Errorf("%s %s attention=%v band=%v: %d columns at width %d: %q", name, st, attn, band, got, w, ansi.Strip(line))
						}
						if w < 30 {
							continue
						}
						p := ansi.Strip(line)
						i := strings.Index(p, "row-") // the name may give way, never its start
						if i < 0 {
							t.Errorf("%s %s at %d: the name is cut: %q", name, st, w, p)
							continue
						}
						if col := ansi.StringWidth(p[:i]); col != iconNameStart {
							t.Errorf("%s %s attention=%v at %d: the name starts in column %d, want %d: %q", name, st, attn, w, col, iconNameStart, p)
						}
					}
				}
			}
		}
	}
}

// The same contract on whole painted frames: the golden fleet drawn with the
// built-in emoji and with one-column glyphs, in every mode and both themes,
// fits the sidebar line by line, and every row it draws parses with its name
// in column 9 (iconRow reads rows by that geometry and nothing else).
func TestIconFramesFitTheSidebarAndAlignTheirNames(t *testing.T) {
	f := goldenFleet(t)
	for _, conf := range []string{iconPanelConf, iconPanelConf + "icon_idle = \"·\"\nicon_busy = \"*\"\nicon_ended = \"x\"\n"} {
		c := newConfFixture(t)
		c.write(t, conf, f.now)
		for _, gc := range goldenCases() {
			frame := strings.TrimSuffix(goldenFrame(t, f, gc), "\n")
			rows := 0
			for i, l := range strings.Split(frame, "\n") {
				if w := ansi.StringWidth(l); w > panelWidth {
					t.Errorf("%s (conf %q) line %d is %d columns: %q", gc.name, conf, i, w, ansi.Strip(l))
				}
			}
			icons := map[string]bool{}
			for _, l := range plain(frame) {
				if icon, _, _, ok := iconRow(l); ok {
					rows++
					icons[icon] = true
				}
			}
			if rows == 0 || len(icons) < 3 {
				t.Errorf("%s (conf %q): %d rows with icons %v; the check read too little", gc.name, conf, rows, icons)
			}
		}
	}
}

// panel.conf's icons reach the row; an unusable one is named in the footer
// note, by file and line, and the row keeps the built-in glyph.
func TestAPanelConfIconIsDrawnAndAnUnusableOneKeepsTheBuiltIn(t *testing.T) {
	c := newConfFixture(t)
	c.write(t, iconPanelConf+"icon_idle = \"·\"\n", fleetNow)
	p, _, _ := openTestPanel(t, fleetNow)
	p.mode = modeStatus
	deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
	if icon, age, ok := iconFrameRow(plain(p.view()), "alpha"); !ok || icon != "·" || age != "3m" {
		t.Fatalf("the file's idle icon is not drawn: %q %q\n%s", icon, age, p.view())
	}
	if n := p.configNote(); n != "" {
		t.Errorf("a usable icon left a note: %q", n)
	}

	for i, tc := range []struct{ body, note string }{
		{"icon_idle = \"💤💤\"\n", "panel.conf:3:13: icon_idle must be 1 or 2 columns wide, not 4; the built-in icon is used"},
		{"\nicon_idle = \"✔️\"\n", "panel.conf:4:13: icon_idle contains U+FE0F (a variation selector)"},
		{"icon_idle = \"👩‍💻\"\n", "panel.conf:3:13: icon_idle contains U+200D (a zero-width joiner)"},
	} {
		c.write(t, iconPanelConf+tc.body, fleetNow.Add(time.Duration(i+1)*time.Second))
		deliver(t, p, snapshotEvent(confSnapshot(fleetNow)))
		if n := p.configNote(); !strings.HasPrefix(n, tc.note) || strings.Contains(n, panelConfFallback) {
			t.Errorf("%q: note %q, want it to start %q and keep the file", tc.body, n, tc.note)
		}
		if icon, _, ok := iconFrameRow(plain(p.view()), "alpha"); !ok || icon != defaultStatusIcons[iconIdle] {
			t.Errorf("%q: the row does not keep the built-in idle icon: %q", tc.body, icon)
		}
	}
}

// The icon is painted with no style at all — an emoji brings its own colours,
// and a configured glyph takes the terminal's — so it adds no fill and no
// colour to the calm budget. The age is plain text: slate on a calm row, and
// on an Attention row bold in the loud hue (honey, or the alert coral for
// blocked); {age:quiet} keeps it slate. No age paints a background.
func TestTheAgeCarriesTheAttentionCueAndTheIconIsUnstyled(t *testing.T) {
	for _, half := range panelHalves {
		t.Run(half.name, func(t *testing.T) {
			withPanelTheme(t, half.name == "dark")
			for _, g := range defaultStatusIcons {
				if got := paintSegment(statusIconCell(g)); got != g {
					t.Errorf("icon %q painted as %q, want the bare glyph", g, got)
				}
			}
			if got := paintSegment(statusIconCell("·")); got != "· " {
				t.Errorf("a one-column icon painted as %q, want it padded to two columns", got)
			}

			since := fleetNow.Add(-13 * time.Minute)
			for _, tc := range []struct {
				status   observer.Status
				attn     bool
				tpl      string
				fg       string
				bold     bool
				whatTone string
			}{
				{observer.StatusWaiting, true, "{age:short}", half.pick(panelPal.ask), true, "ask"},
				{observer.StatusIdle, true, "{age:short}", half.pick(panelPal.ask), true, "ask"},
				{observer.StatusBlocked, true, "{age:short}", half.pick(panelPal.alert), true, "alert"},
				{observer.StatusBlocked, true, "{age:short:quiet}", half.pick(panelPal.slate), false, "slate"},
				{observer.StatusBlocked, false, "{age:short}", half.pick(panelPal.slate), false, "slate"},
				{observer.StatusBusy, false, "{age}", half.pick(panelPal.slate), false, "slate"},
			} {
				r := panelRow{s: observer.Session{Name: "x", Status: tc.status, Attention: tc.attn, StatusSince: &since}}
				set := parsePanelConfForTest(t, "row = \""+tc.tpl+"\"\n")
				rc := &rowContext{now: fleetNow, ctx: set.Ctx, project: func(string) string { return "" }}
				line, _ := set.Row.Line(panelWidth, rowResolver(&r, rc))
				if !strings.Contains(line, sgrRGB(t, 38, tc.fg)) {
					t.Errorf("%s attention=%v %s: %q is not %s", tc.status, tc.attn, tc.tpl, line, tc.whatTone)
				}
				if bold := strings.Contains(line, "\x1b[1;"); bold != tc.bold {
					t.Errorf("%s attention=%v %s: bold=%v, want %v: %q", tc.status, tc.attn, tc.tpl, bold, tc.bold, line)
				}
				if strings.Contains(line, "48;2;") {
					t.Errorf("%s %s: the age paints a fill: %q", tc.status, tc.tpl, line)
				}
			}
		})
	}
}
