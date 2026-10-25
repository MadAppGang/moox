package statedir

import "golang.org/x/sys/unix"

// renameNoReplace is renameatx_np(RENAME_EXCL): atomic, fails with EEXIST.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return unix.RenameatxNp(fromfd, from, tofd, to, unix.RENAME_EXCL)
}
