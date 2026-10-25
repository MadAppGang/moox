//go:build tmuxe2e

package layout

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

// TestE2E_Layout_RemoveLeafAcceptedByTmux (RR10, RR13): on an isolated
// server, build each fixture shape, remove one leaf with RemoveLeaf, kill
// that pane, apply the result with select-layout — in v2 and in v1 (our
// computed checksum) — and compare tmux's resulting pane geometry with ours.
func TestE2E_Layout_RemoveLeafAcceptedByTmux(t *testing.T) {
	s := tmuxtest.Start(t, tmuxtest.WithSize(160, 40))
	shapes := []struct {
		name   string
		build  [][]string // run in the new window, in order; %W is the window id
		remove int        // index into the window's panes (pane order)
	}{
		{"sidebar column beside v", [][]string{{"split-window", "-h", "-t", "%W"}, {"split-window", "-h", "-t", "%W"},
			{"select-pane", "-t", "%W.1"}, {"split-window", "-v", "-t", "%W.1"}}, 3},
		{"observer between", [][]string{{"split-window", "-h", "-t", "%W"}, {"split-window", "-h", "-t", "%W"},
			{"select-layout", "-t", "%W", "even-horizontal"}}, 1},
		{"nested h in v (first)", [][]string{{"split-window", "-v", "-t", "%W"}, {"split-window", "-h", "-t", "%W.0"}}, 0},
		{"nested h in v (last of v)", [][]string{{"split-window", "-v", "-t", "%W"}, {"split-window", "-h", "-t", "%W.0"}}, 2},
		{"tiled 5", [][]string{{"split-window", "-t", "%W"}, {"split-window", "-t", "%W"}, {"split-window", "-t", "%W"},
			{"split-window", "-t", "%W"}, {"select-layout", "-t", "%W", "tiled"}}, 2},
	}
	for _, enc := range []Format{V2, V1} {
		for _, sh := range shapes {
			t.Run(fmt.Sprintf("%s/%s", enc, sh.name), func(t *testing.T) {
				wid := strings.TrimSpace(s.Run(t, "new-window", "-d", "-P", "-F", "#{window_id}", "/bin/sh"))
				for _, c := range sh.build {
					args := make([]string, len(c))
					for i, a := range c {
						args[i] = strings.ReplaceAll(a, "%W", wid)
					}
					if args[0] == "split-window" {
						args = append(args, "/bin/sh")
					}
					s.Run(t, args...)
				}
				ids := strings.Fields(s.Run(t, "list-panes", "-t", wid, "-F", "#{pane_id}"))
				n, _, err := Parse(strings.TrimSpace(s.Run(t, "display", "-p", "-t", wid, "#{window_layout}")))
				if err != nil {
					t.Fatal(err)
				}
				victim := ids[sh.remove]
				if err := n.RemoveLeaf(victim); err != nil {
					t.Fatal(err)
				}
				s.Run(t, "kill-pane", "-t", victim)
				s.Run(t, "select-layout", "-t", wid, n.Encode(enc))

				var want []string
				for _, l := range n.allLeaves() {
					want = append(want, fmt.Sprintf("%s %dx%d,%d,%d", l.SavedPane, l.W, l.H, l.X, l.Y))
				}
				got := strings.Fields(strings.ReplaceAll(s.Run(t, "list-panes", "-t", wid, "-F",
					"#{pane_id}_#{pane_width}x#{pane_height},#{pane_left},#{pane_top}"), "_", " "))
				if strings.Join(pairs(got), " | ") != strings.Join(want, " | ") {
					t.Fatalf("tmux laid out\n %s\nwe computed\n %s", strings.Join(pairs(got), " | "), strings.Join(want, " | "))
				}
			})
		}
	}
}

func pairs(f []string) []string {
	var out []string
	for i := 0; i+1 < len(f); i += 2 {
		out = append(out, f[i]+" "+f[i+1])
	}
	return out
}
