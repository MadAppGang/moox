// Package mooxroot finds the share root (the directory holding moox.conf) and
// the bin directory for an installed or dev-tree moox, and carries the one
// linker-stamped version every moox binary reports (§3.2, §9.1, R2-H18).
//
// Three layouts exist, and the share root moox writes into ~/.tmux.conf must
// be the STABLE one, never a versioned directory that the next upgrade
// deletes (§3.2):
//
//	brew   <prefix>/Cellar/moox/<v>/bin/moox     -> <prefix>/opt/moox/share/moox
//	curl   ~/.local/share/moox/<v>/bin/moox      -> ~/.local/share/moox/current/share/moox
//	dev    <repo>/moox/bin/moox (task build)     -> <repo>/moox/share/moox
//
// plus any `<d>/bin/<exe>` whose `<d>/share/moox/moox.conf` exists (brew's
// opt/ link, the curl `current` link), and a `go run` binary, which lives in a
// throwaway build dir and is located through the module source tree instead.
//
// Leaf package: stdlib only, reads only (§2.3).
package mooxroot

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Version is set at link time by every release build:
//
//	-X github.com/MadAppGang/moox/internal/mooxroot.Version=<semver>
//
// A plain `go build` leaves it empty, which VersionString reports as DevVersion.
var Version string

// DevVersion is what an unstamped build reports. It compares below every
// release in the --ensure handshake (§3.3).
const DevVersion = "0.0.0-dev"

// VersionString is the stamped version, or DevVersion.
func VersionString() string {
	v := strings.TrimPrefix(strings.TrimSpace(Version), "v")
	if v == "" {
		return DevVersion
	}
	return v
}

// Stamped reports whether the linker set Version.
func Stamped() bool { return strings.TrimSpace(Version) != "" }

// ModulePath is the module a dev tree's go.mod declares.
const ModulePath = "github.com/MadAppGang/moox"

// confName is the entry file every share root holds.
const confName = "moox.conf"

// ErrNotFound means no share root could be located for an executable.
var ErrNotFound = errors.New("mooxroot: no share/moox found for this executable")

// Root is a resolved installation.
type Root struct {
	Share   string // stable share root (holds moox.conf)
	Bin     string // directory holding moox and tt
	Libexec string // directory holding moox's private helpers (rpaster)
}

// Resolve locates the installation an executable belongs to. exe should be
// os.Executable(); it is symlink-resolved first, so a brew `bin/moox` link
// into the Cellar maps to opt/. cwd is used only for a `go run` binary.
func Resolve(exe, cwd string) (Root, error) {
	share, err := Stable(exe, cwd)
	if err != nil {
		return Root{}, err
	}
	return Root{Share: share, Bin: filepath.Clean(filepath.Join(share, "..", "..", "bin")), Libexec: LibexecDir(share)}, nil
}

// LibexecDir is where moox keeps helpers it must not put on PATH, relative to
// a share root: brew's <keg>/libexec (formula `libexec.install`, reached
// through opt/moox), install.sh's <version>/libexec (through `current`), a
// dev tree's moox/libexec. rpaster lives there and not in bin/: the separate
// madappgang/tap/rpaster formula (or a hand-installed ~/.local/bin/rpaster)
// owns that name, and a second bin/rpaster made brew refuse to link ANY of
// moox's binaries (V10).
func LibexecDir(share string) string {
	return filepath.Clean(filepath.Join(share, "..", "..", "libexec"))
}

// RpasterName is the copy-image helper's executable name.
const RpasterName = "rpaster"

// Rpaster resolves the copy-image helper: moox's own copy in the share root's
// libexec first, then any rpaster on PATH (look). bundled reports which one
// was found. share may be "" (no root found): then only PATH is tried.
func Rpaster(share string, look func(string) (string, error)) (path string, bundled bool, err error) {
	if share != "" {
		p := filepath.Join(LibexecDir(share), RpasterName)
		if isExec(p) {
			return p, true, nil
		}
	}
	if look != nil {
		if p, err := look(RpasterName); err == nil {
			return p, false, nil
		}
	}
	where := "PATH"
	if share != "" {
		where = LibexecDir(share) + " or on PATH"
	}
	return "", false, fmt.Errorf("mooxroot: no %s in %s", RpasterName, where)
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// Stable maps an executable to its stable share root (§3.2).
func Stable(exe, cwd string) (string, error) {
	if exe == "" {
		return "", ErrNotFound
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = filepath.Clean(exe)
	}
	if s, ok := brewStable(real); ok {
		return s, nil
	}
	if s, ok := curlStable(real); ok {
		return s, nil
	}
	// Generic <d>/bin/<exe> with <d>/share/moox/moox.conf: the opt/ link, the
	// curl `current` link, and a dev tree's moox/bin.
	for _, p := range []string{filepath.Clean(exe), real} {
		dir := filepath.Dir(p)
		if filepath.Base(dir) == "bin" {
			cand := filepath.Join(filepath.Dir(dir), "share", "moox")
			if isFile(filepath.Join(cand, confName)) {
				return cand, nil
			}
		}
	}
	// `go run`: the binary is in a go-build temp dir. Find the module tree
	// from the working directory instead.
	if cwd != "" {
		if s, ok := devTreeFrom(cwd); ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotFound, exe)
}

// brewStable: <prefix>/Cellar/moox/<v>/bin/<exe> -> <prefix>/opt/moox/share/moox.
func brewStable(p string) (string, bool) {
	parts := strings.Split(p, string(filepath.Separator))
	for i := len(parts) - 5; i >= 0; i-- {
		if parts[i] == "Cellar" && parts[i+1] == "moox" && parts[i+3] == "bin" && i+4 == len(parts)-1 {
			prefix := string(filepath.Separator) + filepath.Join(parts[:i]...)
			return filepath.Join(prefix, "opt", "moox", "share", "moox"), true
		}
	}
	return "", false
}

// curlStable: <prefix>/share/moox/<v>/bin/<exe> -> .../moox/current/share/moox.
// The default prefix is ~/.local; under any other `install.sh --prefix` the
// layout is recognised by what the installer leaves beside the versions —
// the `current` link or install.json — so a custom prefix is not pinned to
// a versioned directory the next upgrade prunes (G1-H7).
func curlStable(p string) (string, bool) {
	bin := filepath.Dir(p)
	ver := filepath.Dir(bin)
	mooxDir := filepath.Dir(ver)
	if filepath.Base(bin) != "bin" || filepath.Base(mooxDir) != "moox" {
		return "", false
	}
	share := filepath.Dir(mooxDir)
	if filepath.Base(share) != "share" {
		return "", false
	}
	stable := filepath.Join(mooxDir, "current", "share", "moox")
	if filepath.Base(filepath.Dir(share)) == ".local" {
		return stable, true
	}
	if fi, err := os.Lstat(filepath.Join(mooxDir, "current")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return stable, true
	}
	if isFile(filepath.Join(mooxDir, "install.json")) {
		return stable, true
	}
	return "", false
}

// devTreeFrom walks up from dir to the module root whose go.mod declares
// ModulePath and returns its share/moox when moox.conf exists there.
func devTreeFrom(dir string) (string, bool) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		if declaresModule(filepath.Join(d, "go.mod")) {
			cand := filepath.Join(d, "share", "moox")
			return cand, isFile(filepath.Join(cand, confName))
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// DevShare returns <module root>/share/moox for a directory inside the moox
// source tree, whether or not moox.conf exists yet. Tests use it to find the
// tree they run in.
func DevShare(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if declaresModule(filepath.Join(d, "go.mod")) {
			return filepath.Join(d, "share", "moox"), nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("%w: no go.mod for %s above %s", ErrNotFound, ModulePath, dir)
		}
		d = parent
	}
}

func declaresModule(gomod string) bool {
	f, err := os.Open(gomod)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`) == ModulePath
		}
	}
	return false
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
