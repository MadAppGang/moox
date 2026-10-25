package agentscli_test

// Code review round 2, A2: the cache's containment was LEXICAL, so a state
// directory reached through a symlink into the Claude tree passed it and the
// cache and lock were written under Claude's real directory. The assertion is
// on the stand-in tree's contents, not on an outcome label alone: the property
// is that nothing lands there.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/agentscli"
)

func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestTheCacheRefusesAStateDirectoryReachedThroughASymlinkIntoTheClaudeTree(t *testing.T) {
	claudeRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "harmless-looking")
	if err := os.Symlink(claudeRoot, alias); err != nil {
		t.Fatal(err)
	}
	r := &runner{payload: fixture(t, "real-39.json")}
	src := agentscli.New(agentscli.Config{
		StateDir:     filepath.Join(alias, "state", "tt"),
		ClaudeRoot:   claudeRoot,
		SpawnTimeout: time.Second,
		StaleAfterMs: 30_000,
		Run:          r.run,
	})

	res, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !res.OK || len(res.Entries) != 39 {
		t.Fatalf("ok = %v entries = %d: a refused cache must still answer", res.OK, len(res.Entries))
	}
	if got := treeEntries(t, claudeRoot); len(got) != 0 {
		t.Fatalf("the Claude tree gained %v through a symlinked state directory", got)
	}
	if res.Cache != observer.AgentsCacheUnwritable {
		t.Errorf("cache = %q, want unwritable", res.Cache)
	}
}

// A symlink sitting at the cache's own name is refused rather than replaced:
// the writer does not decide what somebody else's link was for, and it never
// writes through one.
func TestTheCacheRefusesASymlinkedCacheFile(t *testing.T) {
	state := t.TempDir()
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "someone-elses.json")
	content := []byte(`{"not":"ours"}`)
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(state, agentscli.CacheFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	r := &runner{payload: fixture(t, "real-39.json")}
	src := newSource(t, state, r, nil)
	res, _ := src.List(context.Background())
	if !res.OK {
		t.Fatalf("the listing failed: %+v", res)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(content) {
		t.Fatalf("the link's target changed: %q (err %v)", got, err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlinked cache file was replaced (info %v, err %v)", info, err)
	}
	if res.Cache != observer.AgentsCacheUnwritable {
		t.Errorf("cache = %q, want unwritable: publishing over a symlink is refused", res.Cache)
	}
}
