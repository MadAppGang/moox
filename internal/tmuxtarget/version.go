package tmuxtarget

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed `tmux -V`.
//
// Shapes seen in the wild: "tmux 3.6a", "tmux 3.8", "tmux next-3.9",
// "tmux master", and the bare forms without the "tmux " prefix. A "master"
// build compares above every release; "next-X.Y" is X.Y.
type Version struct {
	Raw    string // as printed, trimmed
	Major  int
	Minor  int
	Patch  string // the letter suffix, "a" in 3.6a
	Master bool
	Next   bool
}

// ParseVersion parses the output of `tmux -V`.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	v := Version{Raw: raw}
	body := strings.TrimSpace(strings.TrimPrefix(raw, "tmux"))
	if body == "master" {
		v.Master = true
		return v, nil
	}
	if after, ok := strings.CutPrefix(body, "next-"); ok {
		v.Next = true
		body = after
	}
	major, rest, ok := strings.Cut(body, ".")
	if !ok {
		return Version{}, fmt.Errorf("tmuxtarget: unrecognised tmux version %q", raw)
	}
	maj, err := strconv.Atoi(major)
	if err != nil {
		return Version{}, fmt.Errorf("tmuxtarget: unrecognised tmux version %q", raw)
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return Version{}, fmt.Errorf("tmuxtarget: unrecognised tmux version %q", raw)
	}
	minor, _ := strconv.Atoi(rest[:i])
	v.Major, v.Minor, v.Patch = maj, minor, rest[i:]
	return v, nil
}

// AtLeast reports whether v >= major.minor (the patch letter is ignored: D1's
// floor is "3.6", and 3.6a satisfies it).
func (v Version) AtLeast(major, minor int) bool {
	if v.Master {
		return true
	}
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

func (v Version) String() string { return v.Raw }

// Version runs `tmux -V` with this Target's binary. -V needs no server, so the
// socket is not passed.
func (t Target) Version(ctx context.Context) (Version, error) {
	cmd := t.Command(ctx)
	cmd.Args = []string{t.Bin, "-V"}
	out, err := cmd.Output()
	if err != nil {
		return Version{}, fmt.Errorf("tmuxtarget: %s -V: %w", t.Bin, err)
	}
	return ParseVersion(string(out))
}
