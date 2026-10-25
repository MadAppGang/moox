// Package layout parses and encodes tmux window layouts in both formats tmux
// writes (architecture §6.3, A9): the classic v1 "csum,WxH,X,Y{…}|[…]" form
// and the 3.8 JSON v2 form {"V":2,"L":cell}. It re-encodes either (v1 with a
// computed checksum, for a cross-version restore, RR13) and removes one leaf
// exactly, which is how the tt observer pane is dropped before select-layout
// (B45, RR10).
//
// The grammar and the checks follow tmux's layout-custom.c (3.8): v1 cells
// are WxH,X,Y[,ID] optionally followed by {children} (side by side) or
// [children] (stacked); v2 cells carry t/w/h/x/y, nodes c, leaves a/l/i/z/I.
package layout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Format is the layout string's format, spelled as the snapshot spells it.
type Format string

// The two formats.
const (
	V1 Format = "v1"
	V2 Format = "v2"
)

// Cell kinds.
const (
	KindH    byte = 'h' // children side by side (v1 "{…}", tmux LAYOUT_LEFTRIGHT)
	KindV    byte = 'v' // children stacked (v1 "[…]", tmux LAYOUT_TOPBOTTOM)
	KindPane byte = 'p'
)

// tmux's bounds (tmux.h PANE_MINIMUM/PANE_MAXIMUM/WINDOW_MAXIMUM).
const (
	paneMin   = 1
	paneMax   = 10000
	windowMax = 10000
	v1Depth   = 1000
)

var (
	// ErrSyntax is a layout string tmux would refuse to parse.
	ErrSyntax = errors.New("layout: malformed")
	// ErrChecksum is a v1 layout whose checksum does not match its body.
	ErrChecksum = errors.New("layout: invalid checksum")
	// ErrGeometry is a layout whose sizes do not add up (layout_check).
	ErrGeometry = errors.New("layout: size mismatch")
	// ErrFloatingLeaf: the leaf to remove is a 3.8 floating pane, not a tiled
	// cell; the caller keeps the original layout (§6.3 step 1).
	ErrFloatingLeaf = errors.New("layout: leaf is a floating pane")
	// ErrNoLeaf: no leaf carries the pane id.
	ErrNoLeaf = errors.New("layout: no such pane")
	// ErrOnlyLeaf: removing the window's only pane leaves no layout.
	ErrOnlyLeaf = errors.New("layout: cannot remove the only pane")
)

// Node is one layout cell.
type Node struct {
	Kind       byte // KindH, KindV or KindPane
	W, H, X, Y int
	// Leaf fields. Index is v2 "i" (for a v1 layout: the DFS position);
	// Active is v2 "a"; Last is v2 "l" (-1: not in the last-panes list);
	// Z is v2 "z" (-1: tiled, >= 0: a floating pane).
	Index  int
	Active bool
	Last   int
	Z      int
	// SavedPane is the pane id as "%N" (v2 "I", v1 the trailing ID), or "".
	SavedPane string
	Children  []*Node
}

func newLeaf() *Node { return &Node{Kind: KindPane, Last: -1, Z: -1} }

// Parse reads either format. Leading blanks are skipped, as tmux does.
func Parse(s string) (*Node, Format, error) {
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	if strings.HasPrefix(s, "{") {
		n, err := parseV2(s)
		return n, V2, err
	}
	n, err := parseV1(s)
	return n, V1, err
}

// ---------------------------------------------------------------------------
// v1
// ---------------------------------------------------------------------------

// Checksum is tmux's layout_checksum over the body (after "csum,").
func Checksum(body string) uint16 {
	var c uint16
	for i := 0; i < len(body); i++ {
		c = (c >> 1) + ((c & 1) << 15)
		c += uint16(int16(int8(body[i]))) // tmux adds a (signed) char
	}
	return c
}

func parseV1(s string) (*Node, error) {
	if len(s) < 5 || s[4] != ',' {
		return nil, fmt.Errorf("%w: malformed layout header", ErrSyntax)
	}
	sum, err := strconv.ParseUint(s[:4], 16, 16)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed layout header", ErrSyntax)
	}
	body := s[5:]
	if uint16(sum) != Checksum(body) {
		return nil, ErrChecksum
	}
	p := &v1parser{s: body}
	n, err := p.cell(0)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("%w: trailing data", ErrSyntax)
	}
	for i, l := range n.Leaves() {
		l.Index = i
	}
	return n, nil
}

type v1parser struct {
	s string
	i int
}

func (p *v1parser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *v1parser) num() (int, bool) {
	j := p.i
	for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
		j++
	}
	if j == p.i || j-p.i > 6 {
		return 0, false
	}
	v, _ := strconv.Atoi(p.s[p.i:j])
	p.i = j
	return v, true
}

func (p *v1parser) expect(c byte) bool {
	if p.peek() != c {
		return false
	}
	p.i++
	return true
}

func (p *v1parser) cell(depth int) (*Node, error) {
	if depth > v1Depth {
		return nil, fmt.Errorf("%w: too deep", ErrSyntax)
	}
	bad := fmt.Errorf("%w: invalid layout", ErrSyntax)
	n := newLeaf()
	var ok bool
	if n.W, ok = p.num(); !ok || !p.expect('x') {
		return nil, bad
	}
	if n.H, ok = p.num(); !ok || !p.expect(',') {
		return nil, bad
	}
	if n.X, ok = p.num(); !ok || !p.expect(',') {
		return nil, bad
	}
	if n.Y, ok = p.num(); !ok {
		return nil, bad
	}
	if n.W < paneMin || n.W > paneMax || n.H < paneMin || n.H > paneMax || n.X > windowMax || n.Y > windowMax {
		return nil, bad
	}
	// ",ID" — unless the digits after the comma are the next cell's "WxH".
	if p.peek() == ',' {
		save := p.i
		p.i++
		if id, ok := p.num(); ok && p.peek() != 'x' {
			n.SavedPane = "%" + strconv.Itoa(id)
		} else {
			p.i = save
		}
	}
	switch p.peek() {
	case ',', '}', ']', 0:
		return n, nil
	case '{':
		n.Kind = KindH
	case '[':
		n.Kind = KindV
	default:
		return nil, bad
	}
	n.SavedPane = ""
	closer := byte('}')
	if n.Kind == KindV {
		closer = ']'
	}
	for {
		p.i++ // '{', '[' or ','
		c, err := p.cell(depth + 1)
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, c)
		if p.peek() != ',' {
			break
		}
	}
	if !p.expect(closer) {
		return nil, bad
	}
	return n, nil
}

// EncodeV1 is "%04x,<body>" over the TILED part of the layout, as tmux's
// compat dump does: floating leaves are dropped, a container left with one
// child collapses to it, and the root offset becomes 0,0.
func (n *Node) EncodeV1() string {
	c := tiledCopy(n)
	if c == nil {
		return "0000,"
	}
	if c.Kind == KindPane {
		c.X, c.Y = 0, 0
	}
	var b strings.Builder
	c.writeV1(&b)
	body := b.String()
	return fmt.Sprintf("%04x,%s", Checksum(body), body)
}

func tiledCopy(n *Node) *Node {
	if n.Kind == KindPane {
		if n.Z >= 0 {
			return nil
		}
		c := *n
		c.Children = nil
		return &c
	}
	c := *n
	c.Children = nil
	for _, ch := range n.Children {
		if cc := tiledCopy(ch); cc != nil {
			c.Children = append(c.Children, cc)
		}
	}
	switch len(c.Children) {
	case 0:
		return nil
	case 1:
		return c.Children[0]
	}
	return &c
}

func (n *Node) writeV1(b *strings.Builder) {
	fmt.Fprintf(b, "%dx%d,%d,%d", n.W, n.H, n.X, n.Y)
	if n.Kind == KindPane {
		if id, ok := paneNum(n.SavedPane); ok {
			fmt.Fprintf(b, ",%d", id)
		}
		return
	}
	open, closer := byte('{'), byte('}')
	if n.Kind == KindV {
		open, closer = '[', ']'
	}
	b.WriteByte(open)
	for i, c := range n.Children {
		if i > 0 {
			b.WriteByte(',')
		}
		c.writeV1(b)
	}
	b.WriteByte(closer)
}

func paneNum(id string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimPrefix(id, "%"))
	if err != nil || !strings.HasPrefix(id, "%") || v < 0 {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// v2
// ---------------------------------------------------------------------------

func parseV2(s string) (*Node, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("%w: invalid layout json: %v", ErrSyntax, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrSyntax)
	}
	v, err := jsonInt(top, "V")
	if err != nil {
		return nil, err
	}
	if v != 2 {
		return nil, fmt.Errorf("%w: version mismatch", ErrSyntax)
	}
	raw, ok := top["L"]
	if !ok {
		return nil, fmt.Errorf("%w: missing \"L\"", ErrSyntax)
	}
	root, err := parseV2Cell(raw)
	if err != nil {
		return nil, err
	}
	if err := checkIndexes(root); err != nil {
		return nil, err
	}
	return root, nil
}

func jsonInt(m map[string]json.RawMessage, k string) (int, error) {
	raw, ok := m[k]
	if !ok {
		return 0, fmt.Errorf("%w: missing %q", ErrSyntax, k)
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&num); err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", ErrSyntax, k)
	}
	v, err := num.Int64()
	if err != nil || v < -1<<31 || v > 1<<31-1 {
		return 0, fmt.Errorf("%w: %q is not an integer", ErrSyntax, k)
	}
	return int(v), nil
}

func parseV2Cell(raw json.RawMessage) (*Node, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: cell is not an object", ErrSyntax)
	}
	var t string
	if err := json.Unmarshal(m["t"], &t); err != nil {
		return nil, fmt.Errorf("%w: cell type", ErrSyntax)
	}
	n := newLeaf()
	switch t {
	case "p":
	case "h":
		n.Kind = KindH
	case "v":
		n.Kind = KindV
	default:
		return nil, fmt.Errorf("%w: unknown cell type %q", ErrSyntax, t)
	}
	var err error
	if n.W, err = jsonInt(m, "w"); err != nil {
		return nil, err
	}
	if n.H, err = jsonInt(m, "h"); err != nil {
		return nil, err
	}
	if n.X, err = jsonInt(m, "x"); err != nil {
		return nil, err
	}
	if n.Y, err = jsonInt(m, "y"); err != nil {
		return nil, err
	}
	if n.W < paneMin || n.W > paneMax || n.H < paneMin || n.H > paneMax ||
		n.X < -windowMax || n.X > windowMax || n.Y < -windowMax || n.Y > windowMax {
		return nil, fmt.Errorf("%w: cell geometry out of range", ErrSyntax)
	}
	if n.Kind == KindPane {
		if _, ok := m["c"]; ok {
			return nil, fmt.Errorf("%w: panes cannot have children", ErrSyntax)
		}
		if n.Index, err = jsonInt(m, "i"); err != nil {
			return nil, err
		}
		if n.Index < 0 {
			return nil, fmt.Errorf("%w: invalid index", ErrSyntax)
		}
		if raw, ok := m["a"]; ok {
			if err := json.Unmarshal(raw, &n.Active); err != nil {
				return nil, fmt.Errorf("%w: \"a\" is not a boolean", ErrSyntax)
			}
		} else if _, ok := m["l"]; ok {
			if n.Last, err = jsonInt(m, "l"); err != nil {
				return nil, err
			}
			if n.Last < 0 {
				return nil, fmt.Errorf("%w: invalid last", ErrSyntax)
			}
		}
		if _, ok := m["z"]; ok {
			if n.Z, err = jsonInt(m, "z"); err != nil {
				return nil, err
			}
			if n.Z < 0 {
				return nil, fmt.Errorf("%w: invalid floating zindex", ErrSyntax)
			}
		}
		if raw, ok := m["I"]; ok {
			_ = json.Unmarshal(raw, &n.SavedPane) // ignored by tmux on parse
		}
		return n, nil
	}
	var kids []json.RawMessage
	if err := json.Unmarshal(m["c"], &kids); err != nil {
		return nil, fmt.Errorf("%w: node children", ErrSyntax)
	}
	if len(kids) < 2 {
		return nil, fmt.Errorf("%w: nodes must have more than one child", ErrSyntax)
	}
	for _, k := range kids {
		c, err := parseV2Cell(k)
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, c)
	}
	return n, nil
}

// checkIndexes is layout_parse_ctx_check_indexes plus construct's own checks:
// at most one active pane, at least one pane, and no duplicate index, z-index
// or last index.
func checkIndexes(root *Node) error {
	leaves := root.allLeaves()
	if len(leaves) == 0 {
		return fmt.Errorf("%w: no panes", ErrSyntax)
	}
	idx, z, last := map[int]bool{}, map[int]bool{}, map[int]bool{}
	active := 0
	for _, l := range leaves {
		if l.Active {
			active++
		}
		if idx[l.Index] {
			return fmt.Errorf("%w: duplicate pane index", ErrSyntax)
		}
		idx[l.Index] = true
		if l.Z >= 0 {
			if z[l.Z] {
				return fmt.Errorf("%w: duplicate pane z-index", ErrSyntax)
			}
			z[l.Z] = true
		}
		if l.Last >= 0 && !l.Active {
			if last[l.Last] {
				return fmt.Errorf("%w: duplicate last pane index", ErrSyntax)
			}
			last[l.Last] = true
		}
	}
	if active > 1 {
		return fmt.Errorf("%w: more than one active pane", ErrSyntax)
	}
	return nil
}

// EncodeV2 writes the JSON form with tmux's key order, so a layout tmux
// printed re-encodes byte-identically.
func (n *Node) EncodeV2() string {
	var b strings.Builder
	b.WriteString(`{"V":2,"L":`)
	n.writeV2(&b)
	b.WriteString("}")
	return b.String()
}

func (n *Node) writeV2(b *strings.Builder) {
	fmt.Fprintf(b, `{"t":"%c","w":%d,"h":%d,"x":%d,"y":%d`, n.Kind, n.W, n.H, n.X, n.Y)
	if n.Kind != KindPane {
		b.WriteString(`,"c":[`)
		for i, c := range n.Children {
			if i > 0 {
				b.WriteByte(',')
			}
			c.writeV2(b)
		}
		b.WriteString("]}")
		return
	}
	if n.Active {
		b.WriteString(`,"a":true`)
	} else if n.Last >= 0 {
		fmt.Fprintf(b, `,"l":%d`, n.Last)
	}
	fmt.Fprintf(b, `,"i":%d`, n.Index)
	if n.Z >= 0 {
		fmt.Fprintf(b, `,"z":%d`, n.Z)
	}
	if n.SavedPane != "" {
		id, _ := json.Marshal(n.SavedPane)
		fmt.Fprintf(b, `,"I":%s`, id)
	}
	b.WriteString("}")
}

// Encode writes the layout in format f.
func (n *Node) Encode(f Format) string {
	if f == V2 {
		return n.EncodeV2()
	}
	return n.EncodeV1()
}

// ---------------------------------------------------------------------------
// queries
// ---------------------------------------------------------------------------

func (n *Node) allLeaves() []*Node {
	if n.Kind == KindPane {
		return []*Node{n}
	}
	var out []*Node
	for _, c := range n.Children {
		out = append(out, c.allLeaves()...)
	}
	return out
}

// Leaves returns the leaves in assignment order: tmux assigns panes to cells
// in ascending "i" (v2); for a v1 layout Index is the DFS position, so this is
// DFS order there.
func (n *Node) Leaves() []*Node {
	out := n.allLeaves()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// LeafCount is the number of panes the layout describes, floating included.
func (n *Node) LeafCount() int { return len(n.allLeaves()) }

// tiled is layout_cell_is_tiled || layout_cell_has_tiled_child.
func (n *Node) tiled() bool {
	if n.Kind == KindPane {
		return n.Z < 0
	}
	for _, c := range n.Children {
		if c.tiled() {
			return true
		}
	}
	return false
}

// Validate checks the geometry invariants of layout_check (layout-custom.c):
// in an h container the tiled children's widths plus one border each add up
// to its width, each one's height equals its own, and their x offsets are
// cumulative from the container's; the same transposed for v. Floating cells
// are skipped, as tmux skips them.
func (n *Node) Validate() error {
	if n.Kind == KindPane {
		return nil
	}
	var kids []*Node
	for _, c := range n.Children {
		if c.tiled() {
			kids = append(kids, c)
		}
	}
	if len(kids) == 0 {
		return nil
	}
	sum := 0
	off := n.X
	if n.Kind == KindV {
		off = n.Y
	}
	for _, c := range kids {
		if n.Kind == KindH {
			if c.H != n.H || c.Y != n.Y {
				return fmt.Errorf("%w: child %dx%d,%d,%d across %dx%d,%d,%d", ErrGeometry, c.W, c.H, c.X, c.Y, n.W, n.H, n.X, n.Y)
			}
			if c.X != off {
				return fmt.Errorf("%w: child x %d, want %d", ErrGeometry, c.X, off)
			}
			off += c.W + 1
			sum += c.W + 1
		} else {
			if c.W != n.W || c.X != n.X {
				return fmt.Errorf("%w: child %dx%d,%d,%d across %dx%d,%d,%d", ErrGeometry, c.W, c.H, c.X, c.Y, n.W, n.H, n.X, n.Y)
			}
			if c.Y != off {
				return fmt.Errorf("%w: child y %d, want %d", ErrGeometry, c.Y, off)
			}
			off += c.H + 1
			sum += c.H + 1
		}
		if err := c.Validate(); err != nil {
			return err
		}
	}
	want := n.W
	if n.Kind == KindV {
		want = n.H
	}
	if sum-1 != want {
		return fmt.Errorf("%w: children add up to %d, container is %d", ErrGeometry, sum-1, want)
	}
	return nil
}

// Clone is a deep copy.
func (n *Node) Clone() *Node {
	c := *n
	c.Children = nil
	for _, ch := range n.Children {
		c.Children = append(c.Children, ch.Clone())
	}
	return &c
}
