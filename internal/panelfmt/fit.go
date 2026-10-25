package panelfmt

import (
	"slices"
	"strings"
)

// springMin is the spring's width when the line is tight (§2.4).
const springMin = 1

// fit lays the resolved slots out in width display columns (§2.4) and paints
// the line.
//
//  1. Natural widths fit: done. The spring absorbs the slack, so a line with
//     a spring is exactly width wide.
//  2. Shrink shrinkable segments (Shrink > 0) toward their Min, rank by rank
//     ascending, right to left within a rank.
//  3. Still over: drop the next droppable segment (Drop > 0, lowest rank,
//     rightmost first) and re-collapse, so its separator goes with it by the
//     §2.3 rules; then retry from natural widths, so a drop that frees room
//     un-shrinks what it no longer needs to.
//  4. Still over with nothing left to drop: shrink below Min, down to one
//     column ("…"), in the same order.
//  5. Hard-truncate the painted line at width. Nothing ever exceeds width.
func (t *Template) fit(width int, slots []slot) string {
	if width <= 0 {
		return ""
	}
	dropped := make([]bool, len(slots))
	gone := func(i int) bool { return slots[i].empty || dropped[i] }
	sMin := 0
	if t.spring {
		sMin = springMin
	}

	var (
		pieces [][]piece
		widths []int
	)
	for {
		pieces = t.collapse(gone)
		widths = naturalWidths(slots)
		over := t.total(pieces, widths) + sMin - width
		if over <= 0 {
			return t.paint(width, pieces, slots, widths)
		}
		order := shrinkOrder(pieces, slots)
		if over = shrink(order, slots, widths, over, minFloor); over <= 0 {
			return t.paint(width, pieces, slots, widths)
		}
		next := nextDrop(pieces, slots)
		if next < 0 {
			shrink(order, slots, widths, over, oneFloor)
			return t.paint(width, pieces, slots, widths)
		}
		dropped[next] = true
	}
}

// paint renders the parts with the spring filling what is left of width.
func (t *Template) paint(width int, pieces [][]piece, slots []slot, widths []int) string {
	left, lw := t.paintPieces(pieces[0], slots, widths)
	if !t.spring {
		return clip(left, width)
	}
	right, rw := t.paintPieces(pieces[1], slots, widths)
	pad := max(width-lw-rw, springMin)
	return clip(left+strings.Repeat(" ", pad)+right, width)
}

// total is the width of every part at the allotted widths.
func (t *Template) total(pieces [][]piece, widths []int) int {
	n := 0
	for _, ps := range pieces {
		n += t.measure(ps, widths)
	}
	return n
}

func naturalWidths(slots []slot) []int {
	w := make([]int, len(slots))
	for i, s := range slots {
		w[i] = s.w
	}
	return w
}

// drawn lists the slots the collapsed parts draw, in template order.
func drawn(pieces [][]piece) []int {
	var out []int
	for _, ps := range pieces {
		for _, pc := range ps {
			if pc.slot >= 0 {
				out = append(out, pc.slot)
			}
		}
	}
	return out
}

// shrinkOrder is the drawn shrinkable slots: rank ascending, right to left
// within a rank.
func shrinkOrder(pieces [][]piece, slots []slot) []int {
	var order []int
	for _, i := range drawn(pieces) {
		if slots[i].seg.Shrink > 0 {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if ra, rb := slots[a].seg.Shrink, slots[b].seg.Shrink; ra != rb {
			return ra - rb
		}
		return b - a
	})
	return order
}

// nextDrop is the drawn slot to drop next: lowest Drop rank, rightmost
// within a rank; -1 when none is droppable.
func nextDrop(pieces [][]piece, slots []slot) int {
	best := -1
	for _, i := range drawn(pieces) {
		d := slots[i].seg.Drop
		if d <= 0 {
			continue
		}
		if best < 0 || d < slots[best].seg.Drop || (d == slots[best].seg.Drop && i > best) {
			best = i
		}
	}
	return best
}

// shrink takes up to over columns from the slots in order, each down to its
// floor, and returns what is still over.
func shrink(order []int, slots []slot, widths []int, over int, floor func(slot) int) int {
	for _, i := range order {
		if over <= 0 {
			break
		}
		f := min(floor(slots[i]), slots[i].w)
		if widths[i] <= f {
			continue
		}
		d := min(over, widths[i]-f)
		widths[i] -= d
		over -= d
	}
	return over
}

// minFloor is a segment's Min, at least one column ("…").
func minFloor(s slot) int { return max(s.seg.Min, 1) }

// oneFloor is the last resort: one column.
func oneFloor(slot) int { return 1 }
