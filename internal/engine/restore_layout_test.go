package engine

import (
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// TestRestore_LayoutCrossVersionFallback (RR13): a saved layout is applied in
// the restoring server's encoding whatever the snapshot holds (v2 JSON on
// tmux 3.8+, v1 below); a 3.8 server that refuses it gets the other encoding
// once; a layout neither encoding fits ends tiled with LAYOUT-FALLBACK, and a
// pre-3.8 server is never sent v2, which it cannot parse.
func TestRestore_LayoutCrossVersionFallback(t *testing.T) {
	cases := []struct {
		name, version, saved string
		refuse               func(layout.Format) bool
		want                 []layout.Format // select-layout tries per window, in order
		fallback             bool
	}{
		{"3.7 restores a v2 snapshot as v1", "3.7", snapshot.LayoutV2, nil, []layout.Format{layout.V1}, false},
		{"3.6a restores a v1 snapshot as v1", "3.6a", snapshot.LayoutV1, nil, []layout.Format{layout.V1}, false},
		{"3.8 restores a v1 snapshot as v2", "3.8", snapshot.LayoutV1, nil, []layout.Format{layout.V2}, false},
		{"3.8 retries v1 when v2 is refused", "3.8", snapshot.LayoutV2,
			func(f layout.Format) bool { return f == layout.V2 }, []layout.Format{layout.V2, layout.V1}, false},
		{"3.8 refusing both ends tiled", "3.8", snapshot.LayoutV1,
			func(layout.Format) bool { return true }, []layout.Format{layout.V2, layout.V1}, true},
		{"3.7 refusing v1 ends tiled, never v2", "3.7", snapshot.LayoutV2,
			func(layout.Format) bool { return true }, []layout.Format{layout.V1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRFake()
			f.version = tc.version
			f.addSession("bootstrap")
			e, _, log := newRestoreEngine(t, f)
			snap := snapOf("a:0:2", "a:1:3")
			for i := range snap.Windows {
				w := &snap.Windows[i]
				root, _, err := layout.Parse(w.Layout)
				if err != nil {
					t.Fatal(err)
				}
				if tc.saved == snapshot.LayoutV2 {
					w.Layout, w.LayoutFormat = root.Encode(layout.V2), snapshot.LayoutV2
				}
			}
			if err := snapshot.Validate(snap); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			publish(t, e, snap)
			if tc.refuse != nil {
				f.fail = func(c []string) bool {
					if c[0] != "select-layout" || c[len(c)-1] == "tiled" {
						return false
					}
					_, format, err := layout.Parse(c[len(c)-1])
					return err == nil && tc.refuse(format)
				}
			}
			if _, _, err := restoreFake(t, e, RestoreRequest{}); err != nil {
				t.Fatal(err)
			}
			tries := map[string][]layout.Format{}
			tiled := map[string]bool{}
			for _, c := range f.log {
				if c[0] != "select-layout" {
					continue
				}
				wid := rflag(c, "-t")
				if c[len(c)-1] == "tiled" {
					// pane creation tiles to make room for splits; only a
					// tiled after a saved-layout try is the fallback
					tiled[wid] = len(tries[wid]) != 0
					continue
				}
				_, format, err := layout.Parse(c[len(c)-1])
				if err != nil {
					t.Fatalf("select-layout %q does not parse: %v", c[len(c)-1], err)
				}
				tries[wid] = append(tries[wid], format)
			}
			if len(tries) != 2 {
				t.Fatalf("select-layout reached %d windows, want 2: %v", len(tries), tries)
			}
			for wid, got := range tries {
				if len(got) != len(tc.want) || strings.Join(fmts(got), ",") != strings.Join(fmts(tc.want), ",") {
					t.Errorf("%s: tries %v, want %v", wid, got, tc.want)
				}
				if tiled[wid] != tc.fallback {
					t.Errorf("%s: tiled=%v, want %v", wid, tiled[wid], tc.fallback)
				}
			}
			if n := strings.Count(log.String(), "LAYOUT-FALLBACK"); (n == 2) != tc.fallback || (!tc.fallback && n != 0) {
				t.Errorf("%d LAYOUT-FALLBACK lines, fallback=%v:\n%s", n, tc.fallback, log.String())
			}
		})
	}
}

func fmts(fs []layout.Format) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f)
	}
	return out
}
