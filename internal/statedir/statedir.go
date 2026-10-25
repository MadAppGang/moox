// Package statedir is the ONE package in tt that writes to the filesystem.
//
// # Why it exists
//
// Code review round 2 (A1, A2) found that the two packages §9.4a allowed to
// write — sockserve (socket + pid file) and agentscli (cache + lock) — checked
// containment by comparing path SPELLINGS. Three things walked straight past
// that check:
//
//	a state directory, or any ancestor of it, that is a symlink into the
//	  Claude tree: the lexical check approved it and every write landed under
//	  Claude's real directory;
//	an observer.pid that is a symlink: os.WriteFile followed it and truncated
//	  whatever it pointed at;
//	`tt observe --stop`, which unlinked whatever path the pid file's "socket"
//	  key named, with no containment at all.
//
// And the static audit could not see any of it, because it exempted both
// packages wholesale. So containment is now a property of HOW writes are made
// rather than of a path comparison made beforehand:
//
//  1. The directory is resolved to a CANONICAL (symlink-free) absolute path,
//     and that path — not the spelling it was configured as — is what is
//     checked against the canonical Claude root.
//  2. The canonical path is then opened component by component from "/", each
//     step with O_NOFOLLOW|O_DIRECTORY. A component that is (or has become) a
//     symlink fails the walk instead of redirecting it, so the descriptor this
//     package holds names exactly the directory that was checked.
//  3. Every operation is RELATIVE to that descriptor (openat, unlinkat,
//     renameat, fstatat, fchmodat) and never follows a symlink at the final
//     component. A write cannot leave the verified directory because it never
//     names a path outside it.
//  4. Removal checks the entry's TYPE (and, where the caller has one, its
//     identity) first. A socket name holding a regular file is somebody's data.
//
// The audit now allowlists only this package, and holds THIS package to rule 3
// syntactically: a path-taking mutator anywhere in it is a finding.
package statedir

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	// ErrNoStateDir means there is nowhere to write: an empty path.
	ErrNoStateDir = errors.New("statedir: no state directory")
	// ErrInsideClaudeTree is hard constraint 1: the directory resolves to a
	// location inside the Claude tree, however it was spelled.
	ErrInsideClaudeTree = errors.New("statedir: refusing to write inside the Claude tree")
	// ErrSymlink means a symlink was found where this package will not follow
	// one: a path component during the walk, or a file's final component.
	ErrSymlink = errors.New("statedir: refusing to follow a symlink")
	// ErrNotRegular means a file name holds something other than a regular
	// file, and this package will not write through or over it.
	ErrNotRegular = errors.New("statedir: not a regular file")
	// ErrWrongType means a removal was refused because the entry is not the
	// kind of thing (or not the same file) the caller said it was removing.
	ErrWrongType = errors.New("statedir: refusing to remove an entry of the wrong type")
	// ErrMoved means the configured path stopped resolving to the verified
	// directory while a socket was being bound through it.
	ErrMoved = errors.New("statedir: the state directory moved while it was in use")
	// ErrBadName means a name was not a single path component.
	ErrBadName = errors.New("statedir: not a single path component")
	// ErrNoClaudeRoot means Open or OpenExisting was given no Claude tree to
	// refuse. It used to mean "skip the check" (moox architecture §2.4,
	// PR-H17), which made the one refusal that matters bypassable by
	// omission. The single entry that legitimately skips it is
	// OpenUserConfig, and the audit lets only internal/setup reference that.
	ErrNoClaudeRoot = errors.New("statedir: no Claude tree named to refuse")
)

// Type is what a directory entry is.
type Type int

const (
	TypeOther Type = iota
	TypeRegular
	TypeDir
	TypeSymlink
	TypeSocket
)

func (t Type) String() string {
	switch t {
	case TypeRegular:
		return "regular file"
	case TypeDir:
		return "directory"
	case TypeSymlink:
		return "symlink"
	case TypeSocket:
		return "socket"
	default:
		return "special file"
	}
}

// Info is what fstatat said about one entry, without following it.
type Info struct {
	Type    Type
	Dev     uint64
	Ino     uint64
	Size    int64
	ModTime time.Time
}

// SameFile reports whether two Infos describe the same inode.
func (i Info) SameFile(o Info) bool { return i.Dev == o.Dev && i.Ino == o.Ino }

// maxReadBytes bounds ReadFile. Everything this package reads is a small
// document tt itself wrote (a pid file, the agents cache); 8 MiB is headroom,
// not a budget.
const maxReadBytes = 8 << 20

// Dir is one verified state directory, held open by descriptor.
type Dir struct {
	path      string // absolute, as configured (the spelling messages use)
	canonical string // symlink-free absolute path the descriptor was opened by
	fd        int
	id        Info

	mu     sync.Mutex
	closed bool
}

// Open verifies the state directory, creating any missing components 0700.
// claudeRoot must name the Claude tree; an empty one is ErrNoClaudeRoot.
func Open(path, claudeRoot string) (*Dir, error) { return openChecked(path, claudeRoot, true) }

// OpenExisting verifies the state directory without creating anything. A
// missing directory is an error satisfying errors.Is(err, fs.ErrNotExist).
func OpenExisting(path, claudeRoot string) (*Dir, error) {
	return openChecked(path, claudeRoot, false)
}

// OpenUserConfig opens (creating 0700 if missing) a directory WITHOUT the
// Claude-tree refusal. It exists for one caller: `moox setup` writing its
// hooks into ~/.claude/settings.json with the user's explicit consent (F5).
// audit.RestrictedCalls lets only internal/setup reference it (§2.4).
func OpenUserConfig(path string) (*Dir, error) { return open(path, "", true) }

func openChecked(path, claudeRoot string, create bool) (*Dir, error) {
	if strings.TrimSpace(claudeRoot) == "" {
		return nil, ErrNoClaudeRoot
	}
	return open(path, claudeRoot, create)
}

func open(path, claudeRoot string, create bool) (*Dir, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrNoStateDir
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("statedir: %s: %w", path, err)
	}
	canon, err := Canonical(abs)
	if err != nil {
		return nil, fmt.Errorf("statedir: resolving %s: %w", abs, err)
	}

	if strings.TrimSpace(claudeRoot) != "" {
		rootAbs, err := filepath.Abs(claudeRoot)
		if err != nil {
			rootAbs = filepath.Clean(claudeRoot)
		}
		rootCanon, err := Canonical(rootAbs)
		if err != nil {
			// A root that cannot be resolved is compared by spelling only.
			// That is no weaker than before, and failing here would let an
			// unreadable Claude tree switch off tt's own cache.
			rootCanon = rootAbs
		}
		for _, p := range []string{abs, canon} {
			for _, r := range []string{rootAbs, rootCanon} {
				if Within(p, r) {
					return nil, fmt.Errorf("%w: %s resolves to %s, which is inside %s",
						ErrInsideClaudeTree, path, canon, rootCanon)
				}
			}
		}
	}

	fd, err := walk(canon, create)
	if err != nil {
		return nil, fmt.Errorf("statedir: opening %s: %w", canon, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("statedir: fstat %s: %w", canon, err)
	}
	id := infoOf(&st)
	if id.Type != TypeDir {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("statedir: %s is not a directory", canon)
	}
	return &Dir{path: abs, canonical: canon, fd: fd, id: id}, nil
}

// walk opens canon one component at a time, refusing every symlink.
func walk(canon string, create bool) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, c := range strings.Split(strings.TrimPrefix(canon, "/"), "/") {
		if c == "" {
			continue
		}
		next, err := openChildDir(fd, c)
		if errors.Is(err, unix.ENOENT) && create {
			if merr := unix.Mkdirat(fd, c, 0o700); merr != nil && !errors.Is(merr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, merr
			}
			next, err = openChildDir(fd, c)
		}
		_ = unix.Close(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return -1, fmt.Errorf("%w: %s", ErrSymlink, c)
			}
			if errors.Is(err, unix.ENOENT) {
				return -1, &fs.PathError{Op: "open", Path: canon, Err: fs.ErrNotExist}
			}
			return -1, fmt.Errorf("%s: %w", c, err)
		}
		fd = next
	}
	return fd, nil
}

func openChildDir(dirfd int, name string) (int, error) {
	return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// Canonical resolves every symlink in the longest EXISTING prefix of an
// absolute path and appends the missing remainder verbatim. The remainder
// cannot contain a symlink — it does not exist yet — and Open creates it with
// no-follow semantics, so the result is the path the directory will really
// have.
func Canonical(abs string) (string, error) {
	p := filepath.Clean(abs)
	var rest []string
	cur := p
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{real}, rest...)...), nil
}

// Within reports whether path is root or lies inside it, comparing cleaned
// spellings. It is only ever given paths that have already been made absolute
// (and, by Open, canonical).
func Within(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	p, r := filepath.Clean(path), filepath.Clean(root)
	if p == r {
		return true
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Path is the directory as configured (absolute), for messages.
func (d *Dir) Path() string { return d.path }

// Canonical is the symlink-free path the descriptor was opened through.
func (d *Dir) Canonical() string { return d.canonical }

// Join names an entry for a message. It is never used to open anything.
func (d *Dir) Join(name string) string { return filepath.Join(d.path, name) }

// Close releases the directory descriptor. Idempotent.
func (d *Dir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return unix.Close(d.fd)
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// Lstat describes one entry without following it.
func (d *Dir) Lstat(name string) (Info, error) {
	if err := validName(name); err != nil {
		return Info{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Info{}, &fs.PathError{Op: "lstat", Path: d.Join(name), Err: err}
	}
	return infoOf(&st), nil
}

// ReadFile reads a regular file without following a symlink at its name.
func (d *Dir) ReadFile(name string) ([]byte, Info, error) { return d.ReadFileLimit(name, maxReadBytes) }

// ReadFileLimit is ReadFile with a caller-chosen bound, for the one document
// that may legitimately be larger than 8 MiB: a pane-contents file the engine
// restores from (full scrollback of every pane, §6.1).
func (d *Dir) ReadFileLimit(name string, maxBytes int64) ([]byte, Info, error) {
	if err := validName(name); err != nil {
		return nil, Info{}, err
	}
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, Info{}, fmt.Errorf("%w: %s", ErrSymlink, d.Join(name))
		}
		return nil, Info{}, &fs.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	defer func() { _ = unix.Close(fd) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, Info{}, &fs.PathError{Op: "fstat", Path: d.Join(name), Err: err}
	}
	info := infoOf(&st)
	if info.Type != TypeRegular {
		return nil, info, fmt.Errorf("%w: %s is a %s", ErrNotRegular, d.Join(name), info.Type)
	}
	if info.Size > maxBytes {
		return nil, info, fmt.Errorf("statedir: %s is %d bytes, over the %d-byte bound",
			d.Join(name), info.Size, maxBytes)
	}

	buf := make([]byte, 0, info.Size+1)
	chunk := make([]byte, 32<<10)
	for {
		n, err := unix.Read(fd, chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if int64(len(buf)) > maxBytes {
				return nil, info, fmt.Errorf("statedir: %s grew past the read bound", d.Join(name))
			}
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, info, &fs.PathError{Op: "read", Path: d.Join(name), Err: err}
		}
		if n == 0 {
			return buf, info, nil
		}
	}
}

// WriteFile publishes data at name atomically: a fresh temp file created
// O_EXCL|O_NOFOLLOW in this directory, then renameat over name.
//
// An existing name that is not a regular file — a symlink above all — is
// REFUSED rather than replaced. rename(2) would not follow it, so replacing it
// would be safe for the link's target; it is refused anyway because a link at
// one of tt's own file names is not something tt put there, and quietly
// discarding it is a decision this package does not get to make.
func (d *Dir) WriteFile(name string, data []byte, perm uint32) error {
	return d.writeFile(name, data, perm, false)
}

// WriteFileSync is WriteFile made durable: temp + fsync + renameat +
// fsync(dir), so once it returns the new content survives a crash, and a
// reader never sees a partial file (§6.2 publish).
func (d *Dir) WriteFileSync(name string, data []byte, perm uint32) error {
	return d.writeFile(name, data, perm, true)
}

// ownerOnly keeps only the owner bits: nothing moox writes is group- or
// world-accessible (§4.2 file modes, PR-M10).
func ownerOnly(perm uint32) uint32 { return perm & 0o700 }

func (d *Dir) writeFile(name string, data []byte, perm uint32, durable bool) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := refuseLocked(d, name); err != nil {
		return err
	}
	perm = ownerOnly(perm)
	switch info, err := d.Lstat(name); {
	case err == nil && info.Type != TypeRegular:
		return fmt.Errorf("%w: %s is a %s; refusing to write over it", ErrNotRegular, d.Join(name), info.Type)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}

	tmp := name + ".tmp-" + randomSuffix()
	fd, err := unix.Openat(d.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return &fs.PathError{Op: "create", Path: d.Join(tmp), Err: err}
	}
	cleanup := func() { _ = unix.Unlinkat(d.fd, tmp, 0) }

	if err := writeAll(fd, data); err != nil {
		_ = unix.Close(fd)
		cleanup()
		return &fs.PathError{Op: "write", Path: d.Join(tmp), Err: err}
	}
	// The umask applied to openat's mode; the file's mode is the caller's.
	if err := unix.Fchmod(fd, perm); err != nil {
		_ = unix.Close(fd)
		cleanup()
		return &fs.PathError{Op: "chmod", Path: d.Join(tmp), Err: err}
	}
	if durable {
		if err := unix.Fsync(fd); err != nil {
			_ = unix.Close(fd)
			cleanup()
			return &fs.PathError{Op: "fsync", Path: d.Join(tmp), Err: err}
		}
	}
	if err := unix.Close(fd); err != nil {
		cleanup()
		return &fs.PathError{Op: "close", Path: d.Join(tmp), Err: err}
	}
	if err := unix.Renameat(d.fd, tmp, d.fd, name); err != nil {
		cleanup()
		return &fs.PathError{Op: "rename", Path: d.Join(name), Err: err}
	}
	if durable {
		if err := unix.Fsync(d.fd); err != nil {
			return &fs.PathError{Op: "fsync", Path: d.path, Err: err}
		}
	}
	return nil
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// CreateExclusive creates name O_CREAT|O_EXCL|O_NOFOLLOW and returns the open
// descriptor and the identity of the file it created. The caller closes the
// descriptor with unix.Close and removes the file with Remove, passing that
// identity so it can never remove a file some other process created since.
func (d *Dir) CreateExclusive(name string, perm uint32) (int, Info, error) {
	if err := validName(name); err != nil {
		return -1, Info{}, err
	}
	perm = ownerOnly(perm)
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
	if err != nil {
		return -1, Info{}, &fs.PathError{Op: "create", Path: d.Join(name), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(d.fd, name, 0)
		return -1, Info{}, &fs.PathError{Op: "fstat", Path: d.Join(name), Err: err}
	}
	return fd, infoOf(&st), nil
}

// Remove unlinks name only if it is of type want and, when same is non-nil,
// only if it is still that very file.
//
// unlinkat removes a directory ENTRY in this directory; it never follows a
// symlink and never reaches outside. The type check is what stops "the socket
// is stale, remove it" from removing a regular file that merely sits at the
// socket's name.
func (d *Dir) Remove(name string, want Type, same *Info) error {
	if err := refuseLocked(d, name); err != nil {
		return err
	}
	info, err := d.Lstat(name)
	if err != nil {
		return err
	}
	if info.Type != want {
		return fmt.Errorf("%w: %s is a %s, not a %s", ErrWrongType, d.Join(name), info.Type, want)
	}
	if same != nil && !info.SameFile(*same) {
		return fmt.Errorf("%w: %s was replaced by another file", ErrWrongType, d.Join(name))
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return &fs.PathError{Op: "unlink", Path: d.Join(name), Err: err}
	}
	return nil
}

// Chmod changes a regular file's or a socket's mode. Symlinks are refused.
func (d *Dir) Chmod(name string, mode uint32) error {
	info, err := d.Lstat(name)
	if err != nil {
		return err
	}
	if info.Type == TypeSymlink {
		return fmt.Errorf("%w: %s", ErrSymlink, d.Join(name))
	}
	if err := unix.Fchmodat(d.fd, name, mode, 0); err != nil {
		return &fs.PathError{Op: "chmod", Path: d.Join(name), Err: err}
	}
	return nil
}

// ListenUnix binds a unix stream socket at name inside this directory.
//
// bind(2) has no *at form on darwin, so the socket is bound through the
// configured path, bracketed by two identity checks: before binding, that path
// must still resolve to the verified directory; after, the new entry must be a
// socket IN the verified directory (found by fstatat on the descriptor) and the
// path must still resolve there. Anything else unwinds.
//
// Go's own unlink-on-close is switched OFF: it unlinks by path, and removal
// belongs to Remove, which checks what it is removing.
func (d *Dir) ListenUnix(name string) (*net.UnixListener, Info, error) {
	if err := validName(name); err != nil {
		return nil, Info{}, err
	}
	if err := d.stillResolves(); err != nil {
		return nil, Info{}, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: d.Join(name), Net: "unix"})
	if err != nil {
		return nil, Info{}, err
	}
	ln.SetUnlinkOnClose(false)

	info, lerr := d.Lstat(name)
	rerr := d.stillResolves()
	if lerr != nil || info.Type != TypeSocket || rerr != nil {
		_ = ln.Close()
		if lerr == nil && info.Type == TypeSocket {
			_ = d.Remove(name, TypeSocket, &info)
		}
		return nil, Info{}, fmt.Errorf("%w: %s (bound entry: %v %v)", ErrMoved, d.Join(name), lerr, rerr)
	}
	return ln, info, nil
}

// stillResolves checks that the configured path still names the directory the
// descriptor holds.
func (d *Dir) stillResolves() error {
	var st unix.Stat_t
	if err := unix.Stat(d.path, &st); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrMoved, d.path, err)
	}
	if !infoOf(&st).SameFile(d.id) {
		return fmt.Errorf("%w: %s no longer resolves to %s", ErrMoved, d.path, d.canonical)
	}
	return nil
}

func infoOf(st *unix.Stat_t) Info {
	info := Info{
		Dev:     uint64(st.Dev),
		Ino:     uint64(st.Ino),
		Size:    st.Size,
		ModTime: time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)),
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
		info.Type = TypeRegular
	case unix.S_IFDIR:
		info.Type = TypeDir
	case unix.S_IFLNK:
		info.Type = TypeSymlink
	case unix.S_IFSOCK:
		info.Type = TypeSocket
	default:
		info.Type = TypeOther
	}
	return info
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on darwin or linux; a fixed suffix still
		// works because the temp file is O_EXCL and a collision is an error,
		// not an overwrite.
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}
