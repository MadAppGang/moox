package registryfs

import (
	"io/fs"
	"sync"
	"testing/fstest"
)

// fstestFile opens a one-file in-memory FS, which is the shortest way to get
// an fs.File over a byte slice without writing a File implementation.
func fstestFile(name string, data []byte) (fs.File, error) {
	return fstest.MapFS{name: &fstest.MapFile{Data: data, Mode: 0o600}}.Open(name)
}

// countingFS counts opens per path. Several assertions in this package are
// about which files were TOUCHED rather than about the answer — "the .key
// sibling is never opened" cannot be tested by looking at the result, because
// a reader that opened and discarded it would produce the same result.
type countingFS struct {
	inner fs.FS
	mu    sync.Mutex
	opens map[string]int
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.mu.Lock()
	c.opens[name]++
	c.mu.Unlock()
	return c.inner.Open(name)
}

// mutatingFS returns different bytes on the second open of a path. That is the
// mechanism §6.5 is about: the registry is rewritten IN PLACE, so a read
// landing mid-write sees a truncated prefix and the same read moments later
// sees the finished record. A static fixture cannot express that.
type mutatingFS struct {
	inner fs.FS
	mu    sync.Mutex
	// after maps a path to the bytes its SECOND and subsequent opens return.
	after map[string][]byte
	opens map[string]int
}

func (m *mutatingFS) Open(name string) (fs.File, error) {
	m.mu.Lock()
	if m.opens == nil {
		m.opens = map[string]int{}
	}
	m.opens[name]++
	n := m.opens[name]
	replacement, has := m.after[name]
	m.mu.Unlock()

	if has && n > 1 {
		return fstestFile(name, replacement)
	}
	return m.inner.Open(name)
}
