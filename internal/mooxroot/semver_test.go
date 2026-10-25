package mooxroot

import "testing"

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"1.0.0", "0.9.9", 1},
		{"0.10.0", "0.9.0", 1},
		{"0.1.0-rc.1", "0.1.0", -1},
		{"0.1.0-rc.2", "0.1.0-rc.10", -1},
		{"0.1.0-alpha", "0.1.0-alpha.1", -1},
		{"0.1.0-1", "0.1.0-alpha", -1},
		{"0.1.0+build.5", "0.1.0", 0},
		// The handshake's load-bearing rows: dev and garbage sort below every release.
		{DevVersion, "0.0.1", -1},
		{"devel", "0.0.0", -1},
		{"(devel)", "0.1.0-rc.1", -1},
		{"0.1.0", DevVersion, 1},
		{DevVersion, "devel", 0},
		{"not-a-version", "0.0.1", -1},
		{"1.2", "1.2.0", -1},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := CompareVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d (antisymmetry)", tc.b, tc.a, got, -tc.want)
		}
	}
}
