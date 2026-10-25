package transcriptfs

import (
	"io/fs"
	"path"
	"strings"
)

// This file is §8.3 item 6 and measured fact 10: the subagent count comes from
// FILES on disk, not from parsing the transcript.
//
// Derived from agentbar's internal/transcript/agentmeta.go — the PATH KNOWLEDGE
// only (see THIRD_PARTY.md). Upstream reads one named meta file to answer "what
// is this one agent"; we count them to answer "how many were there", which is a
// different question and a different function.
//
// # Why counting files beats parsing the transcript, measured twice
//
// A nested agent leaves NO record in its parent's transcript. Verified on a
// real machine, on one session of a project directory with nested agents:
//
//	17 meta files.  16 have spawnDepth 1, and each one's toolUseId appears
//	                3 to 6 times in the parent transcript.
//	 1 has spawnDepth 2, and its toolUseId appears ZERO times — its parent is
//	                another AGENT, whose transcript lives under subagents/.
//
// So a transcript-derived count answers 16 where the truth is 17, and it does
// so with a number that looks entirely reasonable. Machine-wide the undercount
// is ~7.2 %. Counting files cannot undercount: a meta file is written per agent
// regardless of depth.
//
// Re-measured while writing this: 193 subagent directories, 1 647 meta files,
// spawnDepth distribution 23/346/31 over the first 400 sampled. Fact 10
// recorded 196/1 556, round 2 reported 193/1 503 and §8.3 item 6 recorded
// 193/1 757 — the count moves weekly. THE METHOD IS THE FIXED THING, and no
// test asserts a machine-wide total.

// The layout, named so the two places that have to agree cite one string.
const (
	// SubagentsDirName is the directory beside the transcript, named for the
	// SESSION: <slug>/<sessionId>/subagents/.
	SubagentsDirName = "subagents"
	// MetaSuffix is what makes a file an agent rather than a transcript. The
	// directory holds BOTH `agent-<id>.meta.json` and `agent-<id>.jsonl`, so a
	// count of directory entries would roughly double the answer.
	MetaSuffix = ".meta.json"
)

// SubagentsDirFor returns the subagents directory for a transcript, as a
// slash-separated name relative to the projects root.
//
// `<slug>/<sessionId>.jsonl` -> `<slug>/<sessionId>/subagents`. It is a
// function so a test can assert the derivation directly; getting it wrong
// yields an absent directory, which reads as "we cannot say" and would
// therefore be invisible.
func SubagentsDirFor(transcriptRel string) string {
	return path.Join(strings.TrimSuffix(transcriptRel, ".jsonl"), SubagentsDirName)
}

// countSubagents counts the meta files beside a transcript.
//
// Three outcomes, and the distinction between the first two is the whole of
// AC4:
//
//	directory absent     -> (nil, false).  "We cannot say."  NOT zero.
//	directory empty      -> (&0,  true).   "None." A real measurement.
//	directory has files  -> (&n,  true).
//
// It COUNTS AND PARSES NOTHING. Not one meta file is opened: they carry a
// `description` field holding the instruction the agent was given, which is
// user text and has no business crossing this boundary (§5.4). The count is
// the entire answer, so reading the contents could only add exposure.
//
// A read error is treated as absence rather than as a failure. The enrichment
// pass must degrade to "no opinion" on unknown data, and a subagents directory
// we cannot list is exactly that.
func (s *Source) countSubagents(transcriptRel string) (*int, bool) {
	entries, err := fs.ReadDir(s.fsys, SubagentsDirFor(transcriptRel))
	if err != nil {
		return nil, false
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			// Not observed in the wild, and a directory is not an agent.
			continue
		}
		if strings.HasSuffix(e.Name(), MetaSuffix) {
			n++
		}
	}
	return &n, true
}
