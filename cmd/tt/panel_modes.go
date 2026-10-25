package main

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/projectkey"
)

// panelMode is how the panel groups its sessions.
//
// The research behind this (ai-docs/sessions/dev-research-session-grouping-*)
// surveyed some 70 tools: status and project are the only groupings shipped
// widely, and project is the more common default (Claude Desktop, Codex's
// agent command center). A tmux session is a different axis on this machine —
// 7 of 13 tmux sessions hold more than one repository — and colour is a
// grouping no surveyed tool offers; it is here because the user asked for it,
// and it gets more useful the more /color is used.
type panelMode int

const (
	modeProject panelMode = iota
	modeStatus
	modeTmux
	modeColour
)

// panelModes is the cycle order of the mode key.
var panelModes = []panelMode{modeProject, modeStatus, modeTmux, modeColour}

// defaultPanelMode is the first run's mode. Project: the more common default,
// and the one that moves rows least as statuses change (2.5x fewer than status
// in the research's busy-hours replay) — with NEEDS YOU pinned on top.
const defaultPanelMode = modeProject

func (m panelMode) String() string {
	switch m {
	case modeStatus:
		return "status"
	case modeTmux:
		return "tmux"
	case modeColour:
		return "colour"
	default:
		return "project"
	}
}

// next is the mode the mode key moves to.
func (m panelMode) next() panelMode {
	for i, mode := range panelModes {
		if mode == m {
			return panelModes[(i+1)%len(panelModes)]
		}
	}
	return defaultPanelMode
}

// parsePanelMode reads a saved mode. "color" is accepted too.
func parsePanelMode(s string) (panelMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "project":
		return modeProject, true
	case "status":
		return modeStatus, true
	case "tmux":
		return modeTmux, true
	case "colour", "color":
		return modeColour, true
	}
	return defaultPanelMode, false
}

// statusSections is status mode: the five status groups, as the panel has
// always drawn them. NEEDS YOU is longest wait first, every other group by name
// with the session id as the tiebreak, so two renderings of one state agree.
func statusSections(sessions []observer.Session, now time.Time) []panelSection {
	byGroup := map[panelGroup][]panelRow{}
	for _, s := range sessions {
		g := panelGroupOf(s)
		byGroup[g] = append(byGroup[g], panelRow{s: s, class: g, band: g == groupNeedsYou})
	}
	var secs []panelSection
	for _, g := range panelGroups {
		if g == groupEnded {
			continue
		}
		rows := byGroup[g]
		sortRows(rows, now, g == groupNeedsYou)
		secs = append(secs, panelSection{
			key: g.title(), title: g.title(), band: g == groupNeedsYou, rows: rows,
		})
	}
	return secs
}

// sortRows orders rows longest wait first (byAge) or by name, the session id
// breaking ties either way.
func sortRows(rows []panelRow, now time.Time, byAge bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].s, rows[j].s
		if byAge {
			if ageA, ageB := panelAge(a, now), panelAge(b, now); ageA != ageB {
				return ageA > ageB
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.Kind < b.Kind
	})
}

// panelHome is a session's group in a grouped mode.
type panelHome struct {
	key   string
	label string
	// rank orders groups: 0 for ordinary groups, sorted by label; higher
	// ranks are the catch-alls, which always come last.
	rank int
	// palette is the colour mode's order, -1 elsewhere.
	palette int
	// hue is colour mode's group colour: its title is drawn as the colour's
	// name badge. The zero hue (every other mode) draws it neutral.
	hue panelHue
	// foldable groups of one session go into "other".
	foldable bool
}

// The catch-all groups, in the order they come last in.
const (
	rankOrdinary = iota
	rankOther
	rankNoTmux
	rankBackground
	rankNoColour
)

// panelColourOrder is colour mode's group order: the spectrum, as /color names
// its colours.
var panelColourOrder = []string{"red", "orange", "yellow", "green", "cyan", "blue", "purple", "pink"}

// home decides which group a session belongs to in mode. A label is drawn
// as a heading and on collapsed lines, so it is sanitised here, at the
// boundary (§2.7, §10): a tmux session or folder name is session text.
func (p panel) home(mode panelMode, s observer.Session, projects map[string]projectkey.Project, labels map[string]string) panelHome {
	switch mode {
	case modeTmux:
		if s.Tmux != nil && s.Tmux.Session != "" {
			return panelHome{key: "t:" + s.Tmux.Session, label: sanitizeText(s.Tmux.Session), palette: -1, foldable: true}
		}
		if s.Kind == observer.KindBackground {
			// A background session never has a pane. It gets a group of its
			// own, last and never folded: 7 of 8 no-pane rows measured were
			// background sessions, and "no tmux" would say the wrong thing.
			return panelHome{key: "background", label: "background", rank: rankBackground, palette: -1}
		}
		return panelHome{key: "no tmux", label: "no tmux", rank: rankNoTmux, palette: -1, foldable: true}
	case modeColour:
		c := strings.ToLower(panelAgentColor(s))
		if h := sessionHue(c); h.known() {
			pal := len(panelColourOrder)
			for i, name := range panelColourOrder {
				if name == c {
					pal = i
				}
			}
			return panelHome{key: "c:" + c, label: c, palette: pal, hue: h}
		}
		return panelHome{key: "no colour", label: "no colour", rank: rankNoColour, palette: -1}
	default:
		pr := projects[s.CWD]
		if pr.Key == "" {
			return panelHome{key: "no folder", label: "no folder", rank: rankNoTmux, palette: -1, foldable: true}
		}
		return panelHome{key: "p:" + pr.Key, label: sanitizeText(labels[pr.Key]), palette: -1, foldable: true}
	}
}

// groupedSections builds a grouped mode's sections: the NEEDS YOU band, then
// one group per project, tmux session or colour, in a STABLE order.
//
// Stable, not "worst group first": about ten of the surveyed tools keep a
// stable order and every one that says why says stability; users file bugs
// against lists that reshuffle; and in the replay worst-first moved 3.3-3.9x
// the lines for no gain once the band exists. Inside a group rows are by name
// for the same reason — ordering them by status moved rows in 7 of 58
// snapshot pairs against 1.
func (p panel) groupedSections(mode panelMode, sessions []observer.Session) []panelSection {
	projects := map[string]projectkey.Project{}
	var labels map[string]string
	if mode == modeProject {
		resolver := p.resolver()
		list := make([]projectkey.Project, 0, len(sessions))
		for _, s := range sessions {
			if _, done := projects[s.CWD]; !done {
				projects[s.CWD] = resolver.Resolve(s.CWD)
				list = append(list, projects[s.CWD])
			}
		}
		labels = projectkey.Labels(list)
	}

	type member struct {
		s    observer.Session
		home panelHome
	}
	type group struct {
		home    panelHome
		members []member
	}
	groups := map[string]*group{}
	var order []string
	var band []panelRow
	for _, s := range sessions {
		h := p.home(mode, s, projects, labels)
		class := panelGroupOf(s)
		if class == groupEnded {
			continue // not drawn: frame leaves them out, and this stays total
		}
		g, ok := groups[h.key]
		if !ok {
			g = &group{home: h}
			groups[h.key] = g
			order = append(order, h.key)
		}
		g.members = append(g.members, member{s: s, home: h})
	}

	// Fold groups of one session into "other" — but only when there are at
	// least two of them, since a single "other" row is a group of one again.
	var singles []string
	for _, k := range order {
		if g := groups[k]; len(g.members) == 1 && g.home.foldable {
			singles = append(singles, k)
		}
	}
	if len(singles) >= 2 && mode != modeColour {
		other := &group{home: panelHome{key: "other", label: "other", rank: rankOther, palette: -1}}
		for _, k := range singles {
			other.members = append(other.members, groups[k].members...)
			delete(groups, k)
		}
		groups["other"] = other
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := groups[keys[i]].home, groups[keys[j]].home
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.palette != b.palette {
			return a.palette < b.palette
		}
		if la, lb := strings.ToLower(a.label), strings.ToLower(b.label); la != lb {
			return la < lb
		}
		return a.key < b.key
	})

	var secs []panelSection
	for _, k := range keys {
		g := groups[k]
		sec := panelSection{key: k, title: g.home.label, hue: g.home.hue}
		var rows []panelRow
		for _, m := range g.members {
			s, h := m.s, m.home
			class := panelGroupOf(s)
			if class == groupNeedsYou {
				// Moved to the band, which names this group on the row.
				label := h.label
				if mode == modeColour {
					label = "" // the row's own colour already says it
				}
				band = append(band, panelRow{s: s, class: class, band: true, label: label})
				continue
			}
			label := ""
			if k == "other" {
				label = h.label
			}
			rows = append(rows, panelRow{s: s, class: class, label: label})
		}
		if len(rows) == 0 {
			continue // every session it holds is in the band, which names it
		}
		sortRows(rows, p.now, false)
		sec.rows = rows
		secs = append(secs, sec)
	}

	sortRows(band, p.now, true)
	if len(band) > 0 {
		secs = append([]panelSection{{key: "band", title: groupNeedsYou.title(), band: true, rows: band}}, secs...)
	}
	return secs
}

// resolver is the panel's project resolver, remembered across frames.
func (p panel) resolver() *projectkey.Resolver {
	if p.memo != nil && p.memo.resolver != nil {
		return p.memo.resolver
	}
	return projectkey.New("")
}

// nameSays reports whether a row's name already starts with its group's label
// at a word boundary — `madbench-workspace-5e` in madbench — so the label
// would only repeat it. The {group} token is Empty then. How the name and the
// label share a row's width is the template engine's (panelfmt fit): the name
// gives way first down to 12 columns, then the label down to 6, then both —
// the split labelled() used to hand-code.
func nameSays(name, label string) bool {
	n, l := strings.ToLower(name), strings.ToLower(label)
	if !strings.HasPrefix(n, l) {
		return false
	}
	rest := []rune(n[len(l):])
	return len(rest) == 0 || !(unicode.IsLetter(rest[0]) || unicode.IsDigit(rest[0]))
}
