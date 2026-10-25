package statedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The operations moox adds to tt's statedir (§2.1, §2.4). Every one keeps the
// package's rule: relative to the verified directory descriptor, never
// following a symlink at the final component, owner-only modes.

// ErrExists means a no-replace rename found its destination taken.
var ErrExists = errors.New("statedir: destination exists")

// OpenSub opens the child directory name, creating it 0700 if missing. The
// child is opened O_NOFOLLOW|O_DIRECTORY relative to d, so a symlink at name
// is refused rather than followed; the result is a Dir in its own right.
func (d *Dir) OpenSub(name string) (*Dir, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := openChildDir(d.fd, name)
	if errors.Is(err, unix.ENOENT) {
		if merr := unix.Mkdirat(d.fd, name, 0o700); merr != nil && !errors.Is(merr, unix.EEXIST) {
			return nil, &fs.PathError{Op: "mkdir", Path: d.Join(name), Err: merr}
		}
		fd, err = openChildDir(d.fd, name)
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			if info, lerr := d.Lstat(name); lerr == nil && info.Type == TypeSymlink {
				return nil, fmt.Errorf("%w: %s", ErrSymlink, d.Join(name))
			}
		}
		return nil, &fs.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "fstat", Path: d.Join(name), Err: err}
	}
	return &Dir{
		path:      filepath.Join(d.path, name),
		canonical: filepath.Join(d.canonical, name),
		fd:        fd,
		id:        infoOf(&st),
	}, nil
}

// ReadDirNames lists the entries of the directory (no "." or ".."), from a
// fresh descriptor so repeated calls each see the whole directory.
func (d *Dir) ReadDirNames() ([]string, error) {
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: d.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), d.path)
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: d.path, Err: err}
	}
	return names, nil
}

// OpenAppend opens name for appending, creating it 0600 if missing. A symlink
// or non-regular file at name is refused. The caller closes the file.
func (d *Dir) OpenAppend(name string) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := refuseLocked(d, name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, d.Join(name))
		}
		return nil, &fs.PathError{Op: "open", Path: d.Join(name), Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "fstat", Path: d.Join(name), Err: err}
	}
	if info := infoOf(&st); info.Type != TypeRegular {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: %s is a %s", ErrNotRegular, d.Join(name), info.Type)
	}
	return os.NewFile(uintptr(fd), d.Join(name)), nil
}

// Rename moves oldName to newName inside this directory, replacing a regular
// file at newName (rename(2) never follows either name). A non-regular
// destination is refused, as WriteFile refuses one.
func (d *Dir) Rename(oldName, newName string) error {
	if err := validName(oldName); err != nil {
		return err
	}
	if err := validName(newName); err != nil {
		return err
	}
	for _, n := range []string{oldName, newName} {
		if err := refuseLocked(d, n); err != nil {
			return err
		}
	}
	switch info, err := d.Lstat(newName); {
	case err == nil && info.Type != TypeRegular:
		return fmt.Errorf("%w: %s is a %s; refusing to rename over it", ErrNotRegular, d.Join(newName), info.Type)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := unix.Renameat(d.fd, oldName, d.fd, newName); err != nil {
		return &fs.PathError{Op: "rename", Path: d.Join(oldName), Err: err}
	}
	return nil
}

// RenameAt moves the ENTRY oldName in src to newName in dst — across two
// directory handles, renameat(2) with two descriptors — and never replaces:
// an existing newName is ErrExists. It moves whatever the entry is, a symlink
// included (the link itself moves, its target is untouched), which is what
// `moox setup` needs to back up a stowed ~/.tmux.conf link (§8.3, PR-H17).
func RenameAt(src *Dir, oldName string, dst *Dir, newName string) error {
	if src == nil || dst == nil {
		return ErrNoStateDir
	}
	if err := validName(oldName); err != nil {
		return err
	}
	if err := validName(newName); err != nil {
		return err
	}
	if err := refuseLocked(src, oldName); err != nil {
		return err
	}
	if err := renameNoReplace(src.fd, oldName, dst.fd, newName); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrExists, dst.Join(newName))
		}
		return &fs.PathError{Op: "rename", Path: src.Join(oldName), Err: err}
	}
	return nil
}

// RemoveTree removes the entry name and, if it is a directory, everything
// below it. Every step is descriptor-relative and no-follow: a symlink
// anywhere in the tree is unlinked as a link, and its target is never
// visited. A missing name is not an error.
func (d *Dir) RemoveTree(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := refuseLocked(d, name); err != nil {
		return err
	}
	return removeTreeAt(d.fd, name, d.Join(name), 0)
}

// maxTreeDepth bounds RemoveTree's recursion: moox's own trees are a few
// levels deep, and a deeper one is not something it made.
const maxTreeDepth = 64

func removeTreeAt(dirfd int, name, label string, depth int) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &fs.PathError{Op: "lstat", Path: label, Err: err}
	}
	if infoOf(&st).Type != TypeDir {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &fs.PathError{Op: "unlink", Path: label, Err: err}
		}
		return nil
	}
	if depth >= maxTreeDepth {
		return fmt.Errorf("statedir: %s is deeper than %d levels; refusing", label, maxTreeDepth)
	}
	fd, err := openChildDir(dirfd, name)
	if err != nil {
		return &fs.PathError{Op: "open", Path: label, Err: err}
	}
	f := os.NewFile(uintptr(fd), label)
	names, rerr := f.Readdirnames(-1)
	if rerr != nil {
		_ = f.Close()
		return &fs.PathError{Op: "readdir", Path: label, Err: rerr}
	}
	for _, n := range names {
		if err := removeTreeAt(int(f.Fd()), n, filepath.Join(label, n), depth+1); err != nil {
			_ = f.Close()
			return err
		}
	}
	_ = f.Close()
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "rmdir", Path: label, Err: err}
	}
	return nil
}

// Mode is the directory's permission bits.
func (d *Dir) Mode() (uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return 0, &fs.PathError{Op: "fstat", Path: d.path, Err: err}
	}
	return uint32(st.Mode) & 0o7777, nil
}

// TightenToOwner makes the directory 0700 if it is more permissive, and
// reports whether it changed anything (the gate logs MODE-TIGHTENED, §4.2).
// It acts on the descriptor, so it changes exactly the directory verified.
func (d *Dir) TightenToOwner() (bool, error) {
	mode, err := d.Mode()
	if err != nil {
		return false, err
	}
	if mode&0o077 == 0 {
		return false, nil
	}
	if err := unix.Fchmod(d.fd, mode&^0o077); err != nil {
		return false, &fs.PathError{Op: "chmod", Path: d.path, Err: err}
	}
	return true, nil
}

// ReplaceKeepMode publishes data at name atomically, like WriteFile, but the
// new file keeps the permission bits of the regular file it replaces (0600 if
// there is none). It exists for one case: `moox uninstall` taking its own line
// back out of a user's shell rc file (§8.3), where narrowing a 0644 ~/.zshrc
// to 0600 would be a change moox did not make on the way in. A symlink or
// non-regular file at name is refused, as WriteFile refuses one.
func (d *Dir) ReplaceKeepMode(name string, data []byte) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := refuseLocked(d, name); err != nil {
		return err
	}
	perm := uint32(0o600)
	var st unix.Stat_t
	switch err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		if info := infoOf(&st); info.Type != TypeRegular {
			return fmt.Errorf("%w: %s is a %s; refusing to write over it", ErrNotRegular, d.Join(name), info.Type)
		}
		perm = uint32(st.Mode) & 0o777
	case !errors.Is(err, unix.ENOENT):
		return &fs.PathError{Op: "lstat", Path: d.Join(name), Err: err}
	}
	tmp := name + ".tmp-" + randomSuffix()
	fd, err := unix.Openat(d.fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &fs.PathError{Op: "create", Path: d.Join(tmp), Err: err}
	}
	cleanup := func() { _ = unix.Unlinkat(d.fd, tmp, 0) }
	if err := writeAll(fd, data); err != nil {
		_ = unix.Close(fd)
		cleanup()
		return &fs.PathError{Op: "write", Path: d.Join(tmp), Err: err}
	}
	if err := unix.Fchmod(fd, perm); err != nil {
		_ = unix.Close(fd)
		cleanup()
		return &fs.PathError{Op: "chmod", Path: d.Join(tmp), Err: err}
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		cleanup()
		return &fs.PathError{Op: "fsync", Path: d.Join(tmp), Err: err}
	}
	if err := unix.Close(fd); err != nil {
		cleanup()
		return &fs.PathError{Op: "close", Path: d.Join(tmp), Err: err}
	}
	if err := unix.Renameat(d.fd, tmp, d.fd, name); err != nil {
		cleanup()
		return &fs.PathError{Op: "rename", Path: d.Join(name), Err: err}
	}
	return nil
}
