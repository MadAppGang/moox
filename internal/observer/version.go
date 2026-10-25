package observer

import (
	"runtime/debug"

	"github.com/MadAppGang/moox/internal/mooxroot"
)

// DevelVersion is what an unstamped build reports: mooxroot.DevVersion, which
// compares below every release in tt serve's version handshake (moox §3.3). A
// plain `go build` has neither a linker stamp nor module version information.
const DevelVersion = mooxroot.DevVersion

// readBuildInfo is the toolchain's build info. A variable so a test can show
// the linker stamp outranks it.
var readBuildInfo = debug.ReadBuildInfo

// ResolveVersion returns tt's own version.
//
// There is exactly one source for this string at a time, in this order:
//
//  1. mooxroot.Version, set by the release with -X (moox §9.1, R2-H18). Every
//     binary of a release carries the same value, so tt and moox can never
//     disagree about which release they are.
//  2. The module version the toolchain stamps into a `go install pkg@v` build.
//  3. DevelVersion.
//
// It is never hardcoded in an encoder, and it is orthogonal to the wire schema
// number: the schema says what shape the JSON is, this says which build
// produced it, and conflating them makes a cosmetic release bump look like a
// breaking wire change.
//
// The composition root resolves it at startup and passes it into wire.Options,
// so no encoder ever reaches for it.
func ResolveVersion() string {
	if mooxroot.Stamped() {
		return mooxroot.VersionString()
	}
	bi, ok := readBuildInfo()
	if !ok {
		return DevelVersion
	}
	v := bi.Main.Version
	// The toolchain writes "(devel)" for an unstamped main module; normalise it
	// to our own spelling so consumers see one value, not two.
	if v == "" || v == "(devel)" {
		return DevelVersion
	}
	return v
}
