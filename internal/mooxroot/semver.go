package mooxroot

import (
	"strconv"
	"strings"
)

// CompareVersions orders two version strings for tt serve's --ensure handshake
// (moox §3.3, PR-L2): -1 if a < b, 0 if equal, +1 if a > b.
//
// The rule is semver's, with the one property the handshake depends on stated
// first: an unstamped dev build (DevVersion, "0.0.0-dev", or tt's former
// "devel"/"(devel)") and anything that does not parse as a version at all sort
// BELOW every release, so a brew build and a dev build never ping-pong — a
// release replaces a dev daemon, a dev build never replaces a release one
// unless asked (--replace). A leading "v" is ignored; a pre-release sorts below
// its release; build metadata ("+…") is ignored.
func CompareVersions(a, b string) int {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa.core[i] != pb.core[i] {
			if pa.core[i] < pb.core[i] {
				return -1
			}
			return 1
		}
	}
	return comparePre(pa.pre, pb.pre)
}

type semver struct {
	core [3]int
	pre  []string
}

// parseSemver accepts MAJOR.MINOR.PATCH[-pre][+build]. Dev spellings are
// reported as not-a-version, which CompareVersions sorts lowest.
func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" || s == DevVersion || s == "devel" || s == "(devel)" {
		return semver{}, false
	}
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return semver{}, false
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

// comparePre is semver §11: no pre-release > any pre-release; identifiers
// compare numerically when both are numeric, else lexically; numeric < text;
// a shorter list that is a prefix sorts first.
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, ea := strconv.Atoi(a[i])
		nb, eb := strconv.Atoi(b[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
