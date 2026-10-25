package blackbox_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"path/filepath"
	"strings"
	"testing"
)

// Valid directory-wide resurrect archive, authored from the documented tar
// entry names. It is deliberately sparse: missing entries must not leak another
// pane's content. No archive or recordings from the real HOME are accessed.
func authorResurrectContents(e *sandbox, fixture resurrectFixture) (string, map[string]string) {
	e.t.Helper()
	payloads := map[string]string{
		"magus:1.1":  "IMPORTED_NORMAL_" + e.nonce + "\n",
		"mnemex:1.1": "IMPORTED_REALIGNED_" + e.nonce + "\n",
	}
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	archive := tar.NewWriter(gz)
	for _, key := range []string{"magus:1.1", "mnemex:1.1"} {
		payload := payloads[key]
		if err := archive.WriteHeader(&tar.Header{Name: "./pane_contents/pane-" + key, Mode: 0600, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
			e.t.Fatal(err)
		}
		if _, err := archive.Write([]byte(payload)); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		e.t.Fatal(err)
	}
	path := filepath.Join(fixture.dir, "pane_contents.tar.gz")
	writeFile(e.t, path, data.String(), 0600)
	return path, payloads
}

// TEST-45, REQ-23/REQ-20/REQ-21: explicit --contents imports the legal archive
// into versioned pane-id frames, while the original last/TSV/archive stay intact.
func TestImport_ExplicitContentsArchivePreservesPaneMappingAndSourceBytes(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
	fixture := authorResurrectFixture(e)
	archive, payloads := authorResurrectContents(e, fixture)
	before := image(t, fixture.dir)
	target := filepath.Join(e.base, "control", "with-contents.json")
	requireCode(t, e.mooxRun("import", "--from", fixture.file, "--contents", archive, "--out", target), 0)
	snapshot := decodeWrittenSnapshot(t, readFile(t, target), "import-resurrect")
	assertImportedWorkspace(t, snapshot, fixture)
	for _, pane := range snapshot.Panes {
		t.Logf("imported pane %s saved_id=%q contents=%v", pane.Key, pane.SavedID, pane.Contents)
	}
	frames := snapshotFrames(e, target, snapshot)
	if len(frames) != len(payloads) {
		t.Errorf("imported contents entries=%d, want %d", len(frames), len(payloads))
	}
	for _, pane := range snapshot.Panes {
		expected, present := payloads[pane.Key]
		if pane.Contents != present {
			t.Errorf("pane %s contents=%v, want %v", pane.Key, pane.Contents, present)
		}
		if present {
			if frames[pane.SavedID] != expected {
				t.Errorf("pane %s imported frame=%q, want %q", pane.Key, frames[pane.SavedID], expected)
			}
			for other, payload := range payloads {
				if other != pane.Key && strings.Contains(frames[pane.SavedID], strings.TrimSpace(payload)) {
					t.Errorf("pane %s received %s archive text", pane.Key, other)
				}
			}
		}
	}
	equalImage(t, image(t, fixture.dir), before, "resurrect TSV/last/contents archive")
}
