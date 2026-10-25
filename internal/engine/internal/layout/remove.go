package layout

import "fmt"

// RemoveLeaf removes the leaf whose SavedPane is savedPane, exactly as §6.3
// (PR-H13) specifies, so the remaining panes keep their saved geometry:
//
//  1. a floating leaf (v2 "z") is not a tiled cell: ErrFloatingLeaf;
//  2. the recipient R is the leaf's previous tiled sibling, or the next one
//     if it is first; the freed extent along the parent's axis is the leaf's
//     size there plus one border;
//  3. R grows by the freed extent, and moves back by it if R is the next
//     sibling;
//  4. the change is propagated into R's subtree: along a container's own axis
//     the delta goes to its last tiled child (recursively) and every child's
//     offset shifts with the container's; across the axis every child takes
//     the new cross size and offset;
//  5. a parent left with one child is replaced by that child, which inherits
//     the parent's geometry;
//  6. v2 fields: leaf "i" values are renumbered contiguously from 0 in their
//     original order, the removed pane's entry leaves the "l" stack and the
//     stack closes up, and "a" moves to R's first leaf if the leaf was active;
//  7. Validate runs on the result.
//
// On any error the receiver is left exactly as it was (the caller keeps the
// original layout string and logs LAYOUT-REMOVE-FAILED or
// LAYOUT-FLOATING-OBSERVER).
func (n *Node) RemoveLeaf(savedPane string) error {
	work := n.Clone()
	root, err := removeLeaf(work, savedPane)
	if err != nil {
		return err
	}
	if err := root.Validate(); err != nil {
		return fmt.Errorf("layout: after removing %s: %w", savedPane, err)
	}
	*n = *root
	return nil
}

func removeLeaf(root *Node, savedPane string) (*Node, error) {
	leaf, parent := find(root, nil, savedPane)
	if leaf == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoLeaf, savedPane)
	}
	if leaf.Z >= 0 {
		return nil, fmt.Errorf("%w: %s", ErrFloatingLeaf, savedPane)
	}
	if parent == nil {
		return nil, ErrOnlyLeaf
	}

	// 2. the recipient: previous tiled sibling, else the next one.
	pos := -1
	for i, c := range parent.Children {
		if c == leaf {
			pos = i
		}
	}
	var recip *Node
	recipIsNext := false
	for i := pos - 1; i >= 0; i-- {
		if parent.Children[i].tiled() {
			recip = parent.Children[i]
			break
		}
	}
	if recip == nil {
		for i := pos + 1; i < len(parent.Children); i++ {
			if parent.Children[i].tiled() {
				recip, recipIsNext = parent.Children[i], true
				break
			}
		}
	}
	if recip == nil {
		return nil, fmt.Errorf("%w: %s has no tiled sibling", ErrGeometry, savedPane)
	}

	// 3+4. grow the recipient along the parent's axis and propagate.
	w, h, x, y := recip.W, recip.H, recip.X, recip.Y
	if parent.Kind == KindH {
		freed := leaf.W + 1
		w += freed
		if recipIsNext {
			x -= freed
		}
	} else {
		freed := leaf.H + 1
		h += freed
		if recipIsNext {
			y -= freed
		}
	}
	resize(recip, w, h, x, y)

	// 6 (before the leaf disappears): the v2 bookkeeping.
	if leaf.Active {
		if first := firstLeaf(recip); first != nil {
			if first.Last >= 0 {
				closeLast(root, first.Last)
				first.Last = -1
			}
			first.Active = true
		}
	} else if leaf.Last >= 0 {
		closeLast(root, leaf.Last)
	}

	parent.Children = append(parent.Children[:pos:pos], parent.Children[pos+1:]...)

	// 5. a parent with one child left is replaced by that child.
	if len(parent.Children) == 1 {
		only := parent.Children[0]
		resize(only, parent.W, parent.H, parent.X, parent.Y)
		*parent = *only
	}

	// 6. renumber "i" contiguously from 0 in the original order.
	for i, l := range root.Leaves() {
		l.Index = i
	}
	return root, nil
}

func find(n, parent *Node, savedPane string) (*Node, *Node) {
	if n.Kind == KindPane {
		if n.SavedPane == savedPane {
			return n, parent
		}
		return nil, nil
	}
	for _, c := range n.Children {
		if l, p := find(c, n, savedPane); l != nil {
			return l, p
		}
	}
	return nil, nil
}

// closeLast removes position pos from the last-panes stack: every later
// entry moves up one.
func closeLast(root *Node, pos int) {
	for _, l := range root.allLeaves() {
		if l.Last > pos {
			l.Last--
		}
	}
}

func firstLeaf(n *Node) *Node {
	if n.Kind == KindPane {
		if n.Z < 0 {
			return n
		}
		return nil
	}
	for _, c := range n.Children {
		if l := firstLeaf(c); l != nil {
			return l
		}
	}
	return nil
}

// resize sets n's geometry and propagates the change into its subtree (§6.3
// step 4): along n's own axis the size delta goes to its LAST tiled child and
// every child shifts with n's offset; across the axis every tiled child takes
// n's new cross size and offset. Floating children keep their geometry.
func resize(n *Node, w, h, x, y int) {
	dw, dh, dx, dy := w-n.W, h-n.H, x-n.X, y-n.Y
	n.W, n.H, n.X, n.Y = w, h, x, y
	if n.Kind == KindPane {
		return
	}
	last := -1
	for i, c := range n.Children {
		if c.tiled() {
			last = i
		}
	}
	for i, c := range n.Children {
		if !c.tiled() {
			continue
		}
		if n.Kind == KindH {
			cw := c.W
			if i == last {
				cw += dw
			}
			resize(c, cw, h, c.X+dx, y)
		} else {
			ch := c.H
			if i == last {
				ch += dh
			}
			resize(c, w, ch, x, c.Y+dy)
		}
	}
}
