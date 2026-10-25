package layout

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Real layouts, printed by tmux 3.8 on an isolated server (160x40):
//
//	fixNested: split -v, then split -h of the top pane → v[ h[%0,%2], %1 ]
//	fixColumn: split -h, split -h, then split -v of the middle → h[ %3, v[%4,%6], %5 ]
const (
	fixNested = `{"V":2,"L":{"t":"v","w":160,"h":40,"x":0,"y":0,"c":[{"t":"h","w":160,"h":20,"x":0,"y":0,"c":[{"t":"p","w":80,"h":20,"x":0,"y":0,"l":1,"i":0,"I":"%0"},{"t":"p","w":79,"h":20,"x":81,"y":0,"a":true,"i":1,"I":"%2"}]},{"t":"p","w":160,"h":19,"x":0,"y":21,"l":0,"i":2,"I":"%1"}]}}`
	fixColumn = `{"V":2,"L":{"t":"h","w":160,"h":40,"x":0,"y":0,"c":[{"t":"p","w":80,"h":40,"x":0,"y":0,"l":2,"i":0,"I":"%3"},{"t":"v","w":39,"h":40,"x":81,"y":0,"c":[{"t":"p","w":39,"h":20,"x":81,"y":0,"l":1,"i":1,"I":"%4"},{"t":"p","w":39,"h":19,"x":81,"y":21,"a":true,"i":2,"I":"%6"}]},{"t":"p","w":39,"h":40,"x":121,"y":0,"l":0,"i":3,"I":"%5"}]}}`
	// three side-by-side panes, the observer (%8) in the middle (built by hand,
	// widths 53+1+52+1+53 = 160).
	fixBetween = `{"V":2,"L":{"t":"h","w":160,"h":40,"x":0,"y":0,"c":[{"t":"p","w":53,"h":40,"x":0,"y":0,"a":true,"i":1,"I":"%7"},{"t":"p","w":52,"h":40,"x":54,"y":0,"l":0,"i":2,"I":"%8"},{"t":"p","w":53,"h":40,"x":107,"y":0,"i":3,"I":"%9"}]}}`
	fixSingle  = `{"V":2,"L":{"t":"p","w":167,"h":51,"x":0,"y":0,"a":true,"i":1,"I":"%1"}}`
)

// Fixtures is every layout string the tests and the fuzz seed use.
var Fixtures = []string{fixNested, fixColumn, fixBetween, fixSingle}

func mustParse(t *testing.T, s string) (*Node, Format) {
	t.Helper()
	n, f, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%.60s…): %v", s, err)
	}
	return n, f
}

// geom renders the leaves in assignment order as "id WxH,X,Y [a|lN]".
func geom(n *Node) string {
	var parts []string
	for _, l := range n.Leaves() {
		g := fmt.Sprintf("%s %dx%d,%d,%d i%d", l.SavedPane, l.W, l.H, l.X, l.Y, l.Index)
		if l.Active {
			g += " a"
		}
		if l.Last >= 0 {
			g += fmt.Sprintf(" l%d", l.Last)
		}
		parts = append(parts, g)
	}
	return strings.Join(parts, " | ")
}

func TestParse_V2ReencodesByteIdentical(t *testing.T) {
	for _, s := range Fixtures {
		n, f := mustParse(t, s)
		if f != V2 {
			t.Fatalf("format %s", f)
		}
		if got := n.EncodeV2(); got != s {
			t.Errorf("re-encode differs:\n got %s\nwant %s", got, s)
		}
		if err := n.Validate(); err != nil {
			t.Errorf("tmux's own layout fails Validate: %v", err)
		}
	}
}

func TestParse_V1Grammar(t *testing.T) {
	body := "160x40,0,0{80x40,0,0,3,79x40,81,0[79x20,81,0,4,79x19,81,21,6]}"
	s := fmt.Sprintf("%04x,%s", Checksum(body), body)
	n, f := mustParse(t, s)
	if f != V1 || n.Kind != KindH || len(n.Children) != 2 || n.Children[1].Kind != KindV {
		t.Fatalf("parsed %s %+v", f, n)
	}
	if got := geom(n); got != "%3 80x40,0,0 i0 | %4 79x20,81,0 i1 | %6 79x19,81,21 i2" {
		t.Fatalf("leaves: %s", got)
	}
	if n.EncodeV1() != s {
		t.Fatalf("v1 re-encode %q != %q", n.EncodeV1(), s)
	}
	// a leaf without an id is legal ("WxH,X,Y" followed by the next cell)
	body2 := "160x40,0,0{80x40,0,0,79x40,81,0}"
	if _, _, err := Parse(fmt.Sprintf("%04x,%s", Checksum(body2), body2)); err != nil {
		t.Fatalf("id-less leaves: %v", err)
	}
	for _, bad := range []string{"", "zzzz,1x1,0,0", fmt.Sprintf("%04x,%s", Checksum(body)+1, body),
		fmt.Sprintf("%04x,%s", Checksum(body+"x"), body+"x"), "0000,"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if _, _, err := Parse(fmt.Sprintf("%04x,%s", Checksum(body)+1, body)); !errors.Is(err, ErrChecksum) {
		t.Errorf("bad checksum: %v", err)
	}
}

func TestParse_V2Refusals(t *testing.T) {
	bad := map[string]string{
		"version":     `{"V":3,"L":{"t":"p","w":1,"h":1,"x":0,"y":0,"i":0}}`,
		"one child":   `{"V":2,"L":{"t":"h","w":1,"h":1,"x":0,"y":0,"c":[{"t":"p","w":1,"h":1,"x":0,"y":0,"i":0}]}}`,
		"two active":  `{"V":2,"L":{"t":"h","w":3,"h":1,"x":0,"y":0,"c":[{"t":"p","w":1,"h":1,"x":0,"y":0,"a":true,"i":0},{"t":"p","w":1,"h":1,"x":2,"y":0,"a":true,"i":1}]}}`,
		"dup index":   `{"V":2,"L":{"t":"h","w":3,"h":1,"x":0,"y":0,"c":[{"t":"p","w":1,"h":1,"x":0,"y":0,"i":0},{"t":"p","w":1,"h":1,"x":2,"y":0,"i":0}]}}`,
		"pane kids":   `{"V":2,"L":{"t":"p","w":1,"h":1,"x":0,"y":0,"i":0,"c":[]}}`,
		"zero width":  `{"V":2,"L":{"t":"p","w":0,"h":1,"x":0,"y":0,"i":0}}`,
		"type":        `{"V":2,"L":{"t":"q","w":1,"h":1,"x":0,"y":0,"i":0}}`,
		"no index":    `{"V":2,"L":{"t":"p","w":1,"h":1,"x":0,"y":0}}`,
		"trailing":    `{"V":2,"L":{"t":"p","w":1,"h":1,"x":0,"y":0,"i":0}}{}`,
		"not integer": `{"V":2,"L":{"t":"p","w":1.5,"h":1,"x":0,"y":0,"i":0}}`,
	}
	for name, s := range bad {
		if _, _, err := Parse(s); err == nil {
			t.Errorf("%s: accepted %s", name, s)
		}
	}
}

func TestChecksum_MatchesTmux(t *testing.T) {
	// A 2-pane v1 body; real tmux accepting our checksum is proven by
	// TestE2E_Layout_RemoveLeafAcceptedByTmux (select-layout of EncodeV1).
	body := "167x51,0,0{83x51,0,0,1,83x51,84,0,2}"
	n, _ := mustParse(t, fmt.Sprintf("%04x,%s", Checksum(body), body))
	if n.LeafCount() != 2 {
		t.Fatal(n.LeafCount())
	}
	// the published algorithm, spelled out once more by hand for 3 bytes
	var c uint16
	for _, b := range []byte("1x1") {
		c = (c >> 1) + ((c & 1) << 15)
		c += uint16(b)
	}
	if Checksum("1x1") != c {
		t.Fatalf("Checksum(1x1) = %04x, want %04x", Checksum("1x1"), c)
	}
}

// TestLayout_V2toV1Checksum (RR13): a v2 layout re-encoded as v1 carries a
// checksum tmux's parser accepts and the same tiled geometry.
func TestLayout_V2toV1Checksum(t *testing.T) {
	for _, s := range Fixtures {
		n, _ := mustParse(t, s)
		v1 := n.EncodeV1()
		m, f, err := Parse(v1)
		if err != nil || f != V1 {
			t.Fatalf("EncodeV1 → %q does not parse: %v", v1, err)
		}
		strip := func(n *Node) string {
			var p []string
			for _, l := range n.allLeaves() {
				p = append(p, fmt.Sprintf("%s %dx%d,%d,%d", l.SavedPane, l.W, l.H, l.X, l.Y))
			}
			return strings.Join(p, " | ")
		}
		if strip(m) != strip(n) {
			t.Fatalf("geometry changed:\n v2 %s\n v1 %s", strip(n), strip(m))
		}
		// and back to v2: still a valid layout
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLayout_RemoveObserverLeaf (B45, RR10, §6.3): the exact rule on each
// fixture shape the architecture names.
func TestLayout_RemoveObserverLeaf(t *testing.T) {
	cases := []struct {
		name, layout, remove, want string
	}{
		{"sidebar column beside a v subtree", fixColumn, "%5",
			"%3 80x40,0,0 i0 l1 | %4 79x20,81,0 i1 l0 | %6 79x19,81,21 i2 a"},
		{"observer between two panes", fixBetween, "%8",
			"%7 106x40,0,0 i0 a | %9 53x40,107,0 i1"},
		{"first leaf of a nested h in v", fixNested, "%0",
			"%2 160x20,0,0 i0 a | %1 160x19,0,21 i1 l0"},
		{"last leaf of v: recipient is an h subtree", fixNested, "%1",
			"%0 80x40,0,0 i0 l0 | %2 79x40,81,0 i1 a"},
		{"v2 active leaf: a moves to the recipient, l closes up", fixColumn, "%6",
			"%3 80x40,0,0 i0 l1 | %4 39x40,81,0 i1 a | %5 39x40,121,0 i2 l0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, _ := mustParse(t, c.layout)
			if err := n.RemoveLeaf(c.remove); err != nil {
				t.Fatal(err)
			}
			if got := geom(n); got != c.want {
				t.Fatalf("after removing %s:\n got %s\nwant %s", c.remove, got, c.want)
			}
			if err := n.Validate(); err != nil {
				t.Fatal(err)
			}
			if n.LeafCount() != strings.Count(c.want, "|")+1 {
				t.Fatal("leaf count")
			}
			// the result is a layout tmux accepts in both encodings
			for _, enc := range []string{n.EncodeV2(), n.EncodeV1()} {
				if _, _, err := Parse(enc); err != nil {
					t.Fatalf("%s: %v", enc, err)
				}
			}
		})
	}
}

func TestLayout_RemoveLeafRefusals(t *testing.T) {
	n, _ := mustParse(t, fixSingle)
	if err := n.RemoveLeaf("%1"); !errors.Is(err, ErrOnlyLeaf) {
		t.Fatalf("only leaf: %v", err)
	}
	n, _ = mustParse(t, fixColumn)
	before := n.EncodeV2()
	if err := n.RemoveLeaf("%42"); !errors.Is(err, ErrNoLeaf) {
		t.Fatalf("missing leaf: %v", err)
	}
	floating := `{"V":2,"L":{"t":"h","w":160,"h":40,"x":0,"y":0,"c":[{"t":"p","w":80,"h":40,"x":0,"y":0,"a":true,"i":0,"I":"%1"},{"t":"p","w":79,"h":40,"x":81,"y":0,"i":1,"I":"%2"},{"t":"p","w":30,"h":10,"x":5,"y":5,"i":2,"z":0,"I":"%3"}]}}`
	f, _ := mustParse(t, floating)
	fb := f.EncodeV2()
	if err := f.RemoveLeaf("%3"); !errors.Is(err, ErrFloatingLeaf) {
		t.Fatalf("floating: %v", err)
	}
	if f.EncodeV2() != fb || n.EncodeV2() != before {
		t.Fatal("a refused RemoveLeaf modified the layout")
	}
	// a tiled leaf beside a floating one: the floating cell keeps its place
	if err := f.RemoveLeaf("%2"); err != nil {
		t.Fatal(err)
	}
	if got := geom(f); got != "%1 160x40,0,0 i0 a | %3 30x10,5,5 i1" {
		t.Fatalf("got %s", got)
	}
	// and v1 drops the floating pane, as tmux's compat dump does
	if v1 := f.EncodeV1(); !strings.HasSuffix(v1, ",160x40,0,0,1") {
		t.Fatalf("v1 of tiled+floating: %s", v1)
	}
}

func TestValidate_Geometry(t *testing.T) {
	n, _ := mustParse(t, fixColumn)
	n.Children[0].W = 81
	if err := n.Validate(); !errors.Is(err, ErrGeometry) {
		t.Fatalf("width sum: %v", err)
	}
	n, _ = mustParse(t, fixColumn)
	n.Children[1].Children[1].Y = 22
	if err := n.Validate(); !errors.Is(err, ErrGeometry) {
		t.Fatalf("offset: %v", err)
	}
	n, _ = mustParse(t, fixColumn)
	n.Children[2].H = 39
	if err := n.Validate(); !errors.Is(err, ErrGeometry) {
		t.Fatalf("cross size: %v", err)
	}
}

// FuzzLayoutRoundTrip (RR13): parse → encode → parse is stable in both
// formats, and a v1 re-encoding of a valid layout always carries a checksum
// the parser (tmux's algorithm) accepts. TestE2E_Layout_RemoveLeafAcceptedByTmux
// feeds the same encodings to a real server.
func FuzzLayoutRoundTrip(f *testing.F) {
	for _, s := range Fixtures {
		f.Add(s)
		n, _, _ := Parse(s)
		f.Add(n.EncodeV1())
	}
	f.Add("bb62,159x48,0,0{79x48,0,0,79x48,80,0}")
	f.Fuzz(func(t *testing.T, s string) {
		n, format, err := Parse(s)
		if err != nil {
			return
		}
		e := n.Encode(format)
		m, f2, err := Parse(e)
		if err != nil {
			t.Fatalf("re-encoded %q does not parse: %v (from %q)", e, err, s)
		}
		if f2 != format || m.Encode(f2) != e {
			t.Fatalf("not stable: %q → %q", e, m.Encode(f2))
		}
		if n.Validate() != nil || !nonNegative(n) {
			return
		}
		if v1 := n.EncodeV1(); v1 != "0000," {
			if _, _, err := Parse(v1); err != nil {
				t.Fatalf("EncodeV1 %q: %v", v1, err)
			}
		}
		if _, _, err := Parse(n.EncodeV2()); err != nil {
			t.Fatalf("EncodeV2: %v", err)
		}
		// removing any tiled leaf of a valid layout either succeeds with a
		// valid layout or leaves it untouched
		for _, l := range n.allLeaves() {
			c := n.Clone()
			before := c.EncodeV2()
			if err := c.RemoveLeaf(l.SavedPane); err != nil {
				if c.EncodeV2() != before {
					t.Fatal("failed RemoveLeaf mutated the layout")
				}
				continue
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("RemoveLeaf(%s) of %q gave an invalid layout: %v", l.SavedPane, s, err)
			}
		}
	})
}

func nonNegative(n *Node) bool {
	if n.X < 0 || n.Y < 0 {
		return false
	}
	for _, c := range n.Children {
		if !nonNegative(c) {
			return false
		}
	}
	return true
}
