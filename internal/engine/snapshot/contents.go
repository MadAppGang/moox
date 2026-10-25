package snapshot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The pane-contents file (§6.1): a gzip of
//
//	"#moox-contents 1 " server_pid " " server_start "\n"
//	then per pane: "#moox-pane " pane_id " " len "\n" <len bytes>
//
// Entries are keyed by the PANE ID of the server life the header names, never
// by S:W.P (R2-C7: kill pane 1 of 2 and pane 2 becomes S:W.1, so a positional
// key would hand it the dead pane's scrollback under a valid hash). Pane ids
// are never reused within one server life, so (server, pane id) is an
// identity.

// ErrContents is a contents file that does not parse.
var ErrContents = errors.New("snapshot: malformed contents file")

const (
	contentsMagic = "#moox-contents"
	contentsVer   = "1"
	paneMagic     = "#moox-pane"
	// maxEntry bounds one entry; a full-history capture of a busy pane is a
	// few MiB, so this is headroom against a corrupt length, not a budget.
	maxEntry = 256 << 20
)

// EncodeContents builds the file. Entries are written in pane-id order and
// the gzip header carries no name and no time, so equal input gives equal
// bytes, hence an equal sha256 and an unchanged fingerprint (S8).
func EncodeContents(server Server, entries map[string][]byte) ([]byte, error) {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return paneLess(ids[i], ids[j]) })

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(zw, "%s %s %d %d\n", contentsMagic, contentsVer, server.Pid, server.StartTime)
	for _, id := range ids {
		if strings.ContainsAny(id, " \n") || id == "" {
			return nil, fmt.Errorf("snapshot: contents: bad pane id %q", id)
		}
		fmt.Fprintf(zw, "%s %s %d\n", paneMagic, id, len(entries[id]))
		if _, err := zw.Write(entries[id]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeContents reads a contents file.
func DecodeContents(data []byte) (Server, map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return Server{}, nil, fmt.Errorf("%w: %v", ErrContents, err)
	}
	defer func() { _ = zr.Close() }()
	r := bufio.NewReader(zr)
	head, err := r.ReadString('\n')
	if err != nil {
		return Server{}, nil, fmt.Errorf("%w: header: %v", ErrContents, err)
	}
	f := strings.Fields(head)
	if len(f) != 4 || f[0] != contentsMagic || f[1] != contentsVer {
		return Server{}, nil, fmt.Errorf("%w: header %q", ErrContents, strings.TrimSpace(head))
	}
	pid, err1 := strconv.Atoi(f[2])
	start, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil {
		return Server{}, nil, fmt.Errorf("%w: header %q", ErrContents, strings.TrimSpace(head))
	}
	out := map[string][]byte{}
	for {
		line, err := r.ReadString('\n')
		if errors.Is(err, io.EOF) && line == "" {
			return Server{Pid: pid, StartTime: start}, out, nil
		}
		if err != nil {
			return Server{}, nil, fmt.Errorf("%w: record: %v", ErrContents, err)
		}
		rf := strings.Fields(line)
		if len(rf) != 3 || rf[0] != paneMagic {
			return Server{}, nil, fmt.Errorf("%w: record %q", ErrContents, strings.TrimSpace(line))
		}
		n, err := strconv.Atoi(rf[2])
		if err != nil || n < 0 || n > maxEntry {
			return Server{}, nil, fmt.Errorf("%w: length %q", ErrContents, rf[2])
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return Server{}, nil, fmt.Errorf("%w: entry %s: %v", ErrContents, rf[1], err)
		}
		out[rf[1]] = b
	}
}

// SHA256 is the hex sha256 the snapshot records for a contents file.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// paneLess orders "%12" after "%9", and anything else lexically.
func paneLess(a, b string) bool {
	na, ea := strconv.Atoi(strings.TrimPrefix(a, "%"))
	nb, eb := strconv.Atoi(strings.TrimPrefix(b, "%"))
	if ea == nil && eb == nil && na != nb {
		return na < nb
	}
	return a < b
}
