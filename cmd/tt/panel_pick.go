package main

// panel_pick.go is the sidebar's "show only" filters (the user, 2026-10-01):
// one SECTION and one STATE, which combine. `f` opens the picker: every
// section title leads with its key in brackets ([1]-[9], [0], then letters)
// and every count-strip word with its state's letter ([n] NEED, [r] RUN,
// [i] IDL, [u] ???). In the picker a section's key shows only that section
// and a state's letter only the rows in that state, in whatever sections hold
// them. A pick closes the picker, so `f 1` is the filtered list at once (the
// user, 2026-10-06: "when I'm done filtering with f and 1 it still shows
// select your filter; I need to press f again"). To combine, `f` again:
// `f 1 f r` is the first section's running sessions. The same key again turns
// its filter off; `f`, esc or enter close the picker without picking; `esc`
// outside it clears every filter, the text one too.
//
// Like the text filter (panel_scroll.go) these are view state: the header's
// counts stay the fleet's, and the collapse-free layout is decided exactly as
// before over what the filters keep.

import (
	"strings"
)

// The picker's keys: section labels in drawing order, and the state letters.
// A section label is never a state letter, the picker's own `f`, `w`, or
// the cursor's `j` and `k`, which move the cursor while the picker is open.
const (
	pickKey           = "f"
	pickSectionLabels = "1234567890abcdeghlmopqstvxyz"
)

// stripKeys are the state letters, by strip state.
var stripKeys = [stripStates]string{"n", "r", "i", "u"}

// pickLabel is the key badge of the i-th section, "" past the last label.
func pickLabel(i int) string {
	if i < 0 || i >= len(pickSectionLabels) {
		return ""
	}
	return pickSectionLabels[i : i+1]
}

// pickFilters applies the section and state filters to a frame's sections,
// after numbering them in drawing order so a section keeps its key whatever
// the filters hide.
func (p panel) pickFilters(secs []panelSection) []panelSection {
	for i := range secs {
		secs[i].pick = pickLabel(i)
	}
	var out []panelSection
	for _, sec := range secs {
		if p.pickSection != "" && sec.key != p.pickSection {
			continue
		}
		if p.pickStateOn {
			var rows []panelRow
			for _, r := range sec.rows {
				if st, ok := stripStateOf(r.class); ok && st == p.pickState {
					rows = append(rows, r)
				}
			}
			if len(rows) == 0 {
				continue
			}
			sec.rows = rows
		}
		out = append(out, sec)
	}
	return out
}

// pickWord is a strip word with its picker key in brackets: the word's own
// first letter when it is the key ("[N]EED"), else the key before the word
// ("[u]???").
func pickWord(word, key string) string {
	if word != "" && strings.EqualFold(word[:1], key) {
		return "[" + word[:1] + "]" + word[1:]
	}
	return "[" + key + "]" + word
}

// anyPick reports whether a section or state filter is on.
func (p panel) anyPick() bool { return p.pickSection != "" || p.pickStateOn }

// pressPick is `f` with the picker closed: it opens the picker, whether or
// not a filter is on, so a second filter can be added to the first.
func (p *panel) pressPick() { p.picking = true }

// pickByKey applies a key pressed in the picker and closes it: a state letter
// toggles that state, a section's key toggles that section, and the picker's
// own key, esc or enter close it without picking. A key that means nothing
// leaves it open. It reports whether the key meant anything.
func (p *panel) pickByKey(key string) bool {
	switch key {
	case pickKey, "esc", "enter":
		p.picking = false
		return true
	}
	for st := stripState(0); st < stripStates; st++ {
		if key == stripKeys[st] {
			if p.pickStateOn && p.pickState == st {
				p.pickStateOn = false
			} else {
				p.pickState, p.pickStateOn = st, true
			}
			p.scroll, p.picking = 0, false
			return true
		}
	}
	// No snapshot yet (the sidebar is still waiting, or its observer went
	// away): there are no sections to key. frame() needs one.
	if p.snap == nil {
		return false
	}
	// Sections are keyed as the frame numbers them before any pick filter,
	// so a key still finds a section the filters hide.
	unpicked := *p
	unpicked.pickSection, unpicked.pickStateOn = "", false
	for _, sec := range unpicked.frame().secs {
		if sec.pick != "" && key == sec.pick {
			if p.pickSection == sec.key {
				p.pickSection, p.pickTitle = "", ""
			} else {
				p.pickSection, p.pickTitle = sec.key, sec.title
			}
			p.scroll, p.picking = 0, false
			return true
		}
	}
	return false
}

// clearFilters drops every filter: the text, the section and the state.
func (p *panel) clearFilters() {
	p.clearFilter()
	p.pickSection, p.pickTitle, p.pickStateOn, p.picking = "", "", false, false
}

// pickSummary is what the section and state filters show, for the footer:
// "dotfiles · RUN", "" when neither is on.
func (p panel) pickSummary() string {
	var parts []string
	if p.pickSection != "" {
		parts = append(parts, sanitizeText(p.pickTitle))
	}
	if p.pickStateOn {
		parts = append(parts, stripLabels[p.pickState])
	}
	return strings.Join(parts, " · ")
}
