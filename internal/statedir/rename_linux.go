package statedir

import "golang.org/x/sys/unix"

// renameNoReplace is renameat2(RENAME_NOREPLACE): atomic, fails with EEXIST.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return unix.Renameat2(fromfd, from, tofd, to, unix.RENAME_NOREPLACE)
}
