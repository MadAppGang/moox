package observer

import (
	"runtime/debug"
	"testing"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

// TestResolveVersionNeverReturnsEmpty. The version string reaches every
// machine-readable surface's envelope, so an empty one would emit `"tt": ""` on
// four surfaces at once. Under `go test` the main module is unstamped, so this
// exercises the fallback path — which is also the path a `go build -o /tmp/...`
// binary takes, i.e. the one used for every verification build in this project.
func TestResolveVersionNeverReturnsEmpty(t *testing.T) {
	got := ResolveVersion()
	if got == "" {
		t.Fatal("ResolveVersion returned an empty string")
	}
	if got == "(devel)" {
		t.Errorf("ResolveVersion leaked the toolchain's spelling %q; "+
			"it must normalise to %q so consumers see one value", got, DevelVersion)
	}
}

// TestResolveVersionIsStable: it is resolved once from build info and must not
// vary between calls, because the composition root reads it once and passes it
// down.
func TestResolveVersionIsStable(t *testing.T) {
	if a, b := ResolveVersion(), ResolveVersion(); a != b {
		t.Errorf("ResolveVersion is not stable: %q then %q", a, b)
	}
}

// TestResolveVersion_LinkerStampWins (moox §4.5, R2-H18): a release stamps
// mooxroot.Version, and that value outranks whatever the toolchain's build info
// says — including a module version of its own.
func TestResolveVersion_LinkerStampWins(t *testing.T) {
	prevStamp, prevInfo := mooxroot.Version, readBuildInfo
	t.Cleanup(func() { mooxroot.Version, readBuildInfo = prevStamp, prevInfo })

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, true
	}
	mooxroot.Version = ""
	if got := ResolveVersion(); got != "v9.9.9" {
		t.Fatalf("unstamped: ResolveVersion = %q, want the module version v9.9.9", got)
	}

	mooxroot.Version = "v1.2.3"
	if got := ResolveVersion(); got != "1.2.3" {
		t.Fatalf("stamped: ResolveVersion = %q, want the linker stamp 1.2.3", got)
	}

	mooxroot.Version = ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}
	if got := ResolveVersion(); got != mooxroot.DevVersion {
		t.Fatalf("dev build: ResolveVersion = %q, want %q", got, mooxroot.DevVersion)
	}
}
