#!/bin/bash
# §9.7's AC14 spot-check: a BRACKETED comparison of what tt reports against
# ground truth collected independently of tt.
#
# WHY A BRACKET. Revision 1's protocol captured `tt sessions` first and then
# read the registry, the pids, the panes and the job records. Those operations
# are sequential, so the two sides describe different instants on a machine
# whose live set moves constantly — which produces both false failures (real
# movement read as a defect) and false passes (fields collected at different
# instants forming a plausible but nonexistent composite state). Here the truth
# is captured BEFORE and AFTER tt's own read, and only sessions whose truth is
# IDENTICAL in both captures are compared. A field that was the same before and
# after was the same during, so comparing it is sound; a field that moved is
# excluded and reported as movement.
#
# READ-ONLY, and specifically: it reads ~/.claude and the DEFAULT tmux server
# (list-panes only, which starts no server and changes nothing), and it writes
# ONLY inside its output directory. It starts no tmux server of its own.
#
# Usage:  validation/spot-check.sh [OUTPUT_DIR] [PATH_TO_TT]
#   OUTPUT_DIR defaults to ./validation
#   PATH_TO_TT defaults to /tmp/tt-dev, i.e. the build under test rather than
#              whatever is installed — the point is to validate THIS binary.

set -uo pipefail

OUT=${1:-validation}
TT=${2:-/tmp/tt-dev}
TS=$(date -u +%Y%m%dT%H%M%SZ)
CLAUDE=${CLAUDE_DIR:-$HOME/.claude}

mkdir -p "$OUT"

if [ ! -x "$TT" ]; then
  echo "spot-check: $TT is not executable. Build it first: go build -o /tmp/tt-dev ." >&2
  exit 2
fi

# jq and awk are required; their absence is a setup error, not a finding.
for tool in jq awk; do
  command -v "$tool" >/dev/null || { echo "spot-check: $tool is required" >&2; exit 2; }
done

# ---------------------------------------------------------------------------
# truth, captured with NO tt code
# ---------------------------------------------------------------------------

# capture_truth writes one ground-truth document. Every source is read here and
# nowhere else, so "before" and "after" are byte-for-byte the same procedure —
# which is what makes their difference meaningful.
capture_truth() {
  local dest=$1
  local started ended
  started=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

  # 1. the interactive registry: id, pid, status, statusUpdatedAt.
  #    `.key` siblings are never read.
  local registry
  registry=$(jq -s -c '[.[] | {
      sessionId: .sessionId,
      pid: .pid,
      status: .status,
      waitingFor: (.waitingFor // null),
      statusUpdatedAt: .statusUpdatedAt,
      tmux: (.tmux // "")
    }] | sort_by(.sessionId)' "$CLAUDE"/sessions/*.json 2>/dev/null || echo '[]')

  # 2. pid liveness, by kill -0, one per registry record.
  local pids alive='[]'
  pids=$(echo "$registry" | jq -r '.[].pid | select(. != null)')
  for pid in $pids; do
    if kill -0 "$pid" 2>/dev/null; then
      alive=$(echo "$alive" | jq -c --argjson p "$pid" '. + [{pid: $p, alive: true}]')
    else
      alive=$(echo "$alive" | jq -c --argjson p "$pid" '. + [{pid: $p, alive: false}]')
    fi
  done

  # 3. panes, from the DEFAULT server, read-only. Sigils included — they are
  #    part of the identifiers (measured fact 7) and a stripped id silently
  #    resolves to the wrong pane or to nothing.
  local panes
  panes=$(tmux list-panes -a -F '#{session_name}:#{window_id}.#{pane_id}' 2>/dev/null |
    jq -R -s -c 'split("\n") | map(select(length > 0)) | sort')

  # 4. background job records, depth 1 only (a decoy state.json lives at depth
  #    5 and 8 under one job's tmp/).
  local jobs='[]'
  if [ -d "$CLAUDE/jobs" ]; then
    jobs=$(for d in "$CLAUDE"/jobs/*/; do
      [ -f "$d/state.json" ] || continue
      jq -c --arg dir "$(basename "$d")" '{
        dir: $dir,
        sessionId: .sessionId,
        state: .state,
        tempo: .tempo,
        updatedAt: .updatedAt,
        linkScanPath: (.linkScanPath // ""),
        reapedMidWorkAt: (.reapedMidWorkAt // null)
      }' "$d/state.json" 2>/dev/null
    done | jq -s -c 'sort_by(.dir)')
  fi

  # 5. the newest TIMESTAMPED record in each job transcript's tail. Eight record
  #    types carry no timestamp and on most job transcripts the final line is one
  #    of them, so this walks backwards rather than reading the last line.
  local activity='[]'
  while IFS=$'\t' read -r sid path; do
    [ -n "$path" ] && [ -f "$path" ] || continue
    local newest
    newest=$(tail -c 65536 "$path" | awk '
      /"timestamp"/ {
        line = $0
        if (match(line, /"timestamp":"[^"]*"/)) {
          t = substr(line, RSTART + 14, RLENGTH - 15)
          last = t
        }
      }
      END { print last }')
    activity=$(echo "$activity" | jq -c --arg s "$sid" --arg t "$newest" '. + [{sessionId: $s, lastActivityAt: $t}]')
  done < <(echo "$jobs" | jq -r '.[] | [.sessionId, .linkScanPath] | @tsv')

  # 6. the vendor's own listing. Bounded: a wedged CLI must not hang the check,
  #    and this machine has had one (three of three probes exiting 124 at 12 s).
  local agents='[]' agents_ok=true
  if ! agents=$(timeout 10 claude agents --json 2>/dev/null | jq -c 'sort_by(.sessionId)'); then
    agents='[]'
    agents_ok=false
  fi
  [ -z "$agents" ] && { agents='[]'; agents_ok=false; }

  ended=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

  jq -n -c \
    --arg started "$started" --arg ended "$ended" \
    --argjson registry "$registry" --argjson alive "$alive" \
    --argjson panes "$panes" --argjson jobs "$jobs" \
    --argjson activity "$activity" --argjson agents "$agents" \
    --argjson agentsOK "$agents_ok" \
    '{captureStarted: $started, captureEnded: $ended, registry: $registry,
      pidLiveness: $alive, panes: $panes, jobs: $jobs, transcriptActivity: $activity,
      agents: $agents, agentsOK: $agentsOK}' > "$dest"
}

T0=$(date +%s)

echo "spot-check: truth BEFORE"
capture_truth "$OUT/truth-before-$TS.json"

echo "spot-check: tt sessions --json"
"$TT" sessions --json --include-dead --refresh > "$OUT/observed-$TS.json"
TT_EXIT=$?

echo "spot-check: truth AFTER"
capture_truth "$OUT/truth-after-$TS.json"

T1=$(date +%s)
BRACKET=$((T1 - T0))

# ---------------------------------------------------------------------------
# the stable set, and the comparison over it
# ---------------------------------------------------------------------------

REPORT="$OUT/spot-check-$TS.md"

jq -n \
  --slurpfile before "$OUT/truth-before-$TS.json" \
  --slurpfile after "$OUT/truth-after-$TS.json" \
  --slurpfile observed "$OUT/observed-$TS.json" \
  --argjson bracket "$BRACKET" \
  --arg ts "$TS" '
  # Timestamps arrive in two shapes and BOTH are live: the registry writes
  # statusUpdatedAt as epoch millis, a job record writes updatedAt as RFC3339.
  # Normalising to whole seconds is what lets one comparison read both, and a
  # second is well inside the bracket this check reports.
  def norm(t):
    if t == null then null
    elif (t | type) == "number" then (t / 1000 | floor | todate)
    else (t | tostring | sub("\\.[0-9]+Z$"; "Z"))
    end;

  def truthset(t):
    ([t.registry[] | . as $r | {key: .sessionId, value: {
        kind: "interactive",
        status: $r.status,
        at: norm($r.statusUpdatedAt),
        # Ground-truth liveness, derived the way §3.5 derives it and with no tt
        # code: a live pid AND a pane that still exists. The pane reference is
        # compared VERBATIM, sigils included — stripping them yields ids tmux
        # silently resolves to the wrong pane (AC8).
        live: (
          ([t.pidLiveness[] | select(.pid == $r.pid) | .alive] | first // false)
          and (($r.tmux != "") and (([t.panes[] | select(. == $r.tmux)] | length) > 0))
        )
      }}] +
     [t.jobs[] | {key: .sessionId, value: {
        kind: "background",
        status: .state,
        at: norm(.updatedAt),
        # A job record carries no pid and nothing to kill -0, so ground truth
        # has no independent liveness for it. null, not false: "we cannot say"
        # and "it is dead" are different claims and only one is true here.
        live: null
      }}]) | from_entries;

  ($before[0]) as $b | ($after[0]) as $a | ($observed[0]) as $o |
  (truthset($b)) as $tb | (truthset($a)) as $ta |
  (($tb | keys) + ($ta | keys) | unique) as $union |
  ([$union[] | select(($tb[.] != null) and ($tb[.] == $ta[.]))]) as $stable |
  ([$o.sessions[] | {key: .sessionId, value: .}] | from_entries) as $obs |

  {
    takenAt: $ts,
    bracketSeconds: $bracket,
    counts: {
      truthBeforeRegistry: ($b.registry | length),
      truthAfterRegistry: ($a.registry | length),
      truthBeforeJobs: ($b.jobs | length),
      truthAfterJobs: ($a.jobs | length),
      panesBefore: ($b.panes | length),
      panesAfter: ($a.panes | length),
      agentsListed: ($b.agents | length),
      agentsOK: $b.agentsOK,
      observedSessions: ($o.sessions | length),
      union: ($union | length),
      stable: ($stable | length),
      movement: (($union | length) - ($stable | length))
    },
    membership: {
      inTruthNotInTT: [$stable[] | select($obs[.] == null)],
      inTTNotInTruth: [$o.sessions[].sessionId | select(($union | index(.)) == null)]
    },
    statusDisagreements: [
      $stable[] as $id | select($obs[$id] != null) |
      select($obs[$id].rawStatus != $tb[$id].status) |
      {sessionId: $id, kind: $tb[$id].kind, truth: $tb[$id].status, tt: $obs[$id].rawStatus}
    ],
    kindDisagreements: [
      $stable[] as $id | select($obs[$id] != null) |
      select($obs[$id].kind != $tb[$id].kind) |
      {sessionId: $id, truth: $tb[$id].kind, tt: $obs[$id].kind}
    ],
    # M15: statusSince is one of the three fields revision 1 changed and never
    # validated, so it is re-derived here from the record and compared.
    statusSinceDisagreements: [
      $stable[] as $id | select($obs[$id] != null) |
      select($obs[$id].statusSince != null and $tb[$id].at != null) |
      select(norm($obs[$id].statusSince) != $tb[$id].at) |
      {sessionId: $id, truth: $tb[$id].at, tt: $obs[$id].statusSince}
    ],
    livenessDisagreements: [
      $stable[] as $id | select($obs[$id] != null) |
      select($tb[$id].live != null and $obs[$id].live != null) |
      select($obs[$id].live != $tb[$id].live) |
      {sessionId: $id, truth: $tb[$id].live, tt: $obs[$id].live, reason: $obs[$id].liveReason}
    ],
    # The standing falsification test for §3.5 rule 1. Skipped, not failed, when
    # the vendor CLI could not be reached — which is the state this machine was
    # measured in.
    terminalStateGate: (
      if ($b.agentsOK | not) then {skipped: "no corroboration listing"}
      else {
        nonTerminalUnlisted: [
          $b.jobs[] | select(.state != "done" and .state != "failed") | . as $j |
          select(([$b.agents[] | select(.sessionId == $j.sessionId)] | length) == 0) |
          {dir: $j.dir, state: $j.state}
        ],
        terminalListed: [
          $b.jobs[] | select(.state == "done" or .state == "failed") | . as $j |
          select(([$b.agents[] | select(.sessionId == $j.sessionId)] | length) > 0) |
          {dir: $j.dir, state: $j.state}
        ]
      }
      end
    ),
    liveness: {
      live: ([$o.sessions[] | select(.live == true)] | length),
      dead: ([$o.sessions[] | select(.live == false)] | length),
      unknown: ([$o.sessions[] | select(.live == null)] | length),
      byReason: ([$o.sessions[] | .liveReason] | group_by(.) | map({(.[0]): length}) | add)
    },
    sources: ($o.sources | with_entries({key: .key, value: {ok: .value.ok, ageMs: .value.ageMs, lastError: .value.lastError}})),
    warnings: [$o.warnings[] | .code]
  } |
  . + {
    outcome: (
      if (.counts.stable * 2) < .counts.union then "inconclusive"
      elif (.statusDisagreements | length) > 0
        or (.kindDisagreements | length) > 0
        or (.statusSinceDisagreements | length) > 0
        or (.livenessDisagreements | length) > 0
        or (.membership.inTruthNotInTT | length) > 0 then "fail"
      else "pass" end
    )
  }
  ' > "$OUT/comparison-$TS.json"

OUTCOME=$(jq -r '.outcome' "$OUT/comparison-$TS.json")

{
  echo "# AC14 spot-check — $TS"
  echo
  echo "Outcome: **$OUTCOME**. Bracket width: ${BRACKET}s. \`tt sessions\` exit: $TT_EXIT."
  echo
  echo '## Counts'
  echo
  echo '```json'
  jq '.counts' "$OUT/comparison-$TS.json"
  echo '```'
  echo
  echo '## Liveness partition, as tt reports it'
  echo
  echo '```json'
  jq '.liveness' "$OUT/comparison-$TS.json"
  echo '```'
  echo
  echo '## Disagreements over the stable set'
  echo
  echo '```json'
  jq '{membership, statusDisagreements, kindDisagreements, statusSinceDisagreements, livenessDisagreements}' "$OUT/comparison-$TS.json"
  echo '```'
  echo
  echo '## The terminal-state gate, and what tt said about its own sources'
  echo
  echo '```json'
  jq '{terminalStateGate, sources, warnings}' "$OUT/comparison-$TS.json"
  echo '```'
  echo
  echo '## Artifacts'
  echo
  echo "- \`truth-before-$TS.json\` — ground truth, no tt code"
  echo "- \`observed-$TS.json\` — \`tt sessions --json --include-dead --refresh\`"
  echo "- \`truth-after-$TS.json\` — the same captures, after"
  echo "- \`comparison-$TS.json\` — the stable set and every compared field"
} > "$REPORT"

echo "spot-check: $OUTCOME — $REPORT"

# ---------------------------------------------------------------------------
# §9.4(c) the rootless write check, with its no-spawn control
# ---------------------------------------------------------------------------
#
# The instrument matters here: `find` on this machine is a shell function
# shimming bfs, which REJECTS -newermt '-3 seconds' and writes to stderr while
# printing nothing on stdout — a failure indistinguishable from a clean result.
# /usr/bin/find is used explicitly, a non-empty stderr fails the check, and
# nothing is excluded from the search.

WRITECHECK="$OUT/write-check-$TS.txt"
{
  echo "# §9.4(c) — did anything write under ~/.claude while tt ran?"
  echo
  echo "## control window: no spawn, 3 seconds"
  sleep 3
  /usr/bin/find "$CLAUDE" -maxdepth 4 -newermt '-3 seconds' 2>controlerr.tmp
  echo "control stderr: $(cat controlerr.tmp)"
  rm -f controlerr.tmp
  echo
  echo "## spawn window: 5 corroboration spawns, then the same search"
  for _ in 1 2 3 4 5; do "$TT" sessions --json --refresh > /dev/null 2>&1; done
  /usr/bin/find "$CLAUDE" -maxdepth 4 -newermt '-3 seconds' 2>spawnerr.tmp
  echo "spawn stderr: $(cat spawnerr.tmp)"
  rm -f spawnerr.tmp
} > "$WRITECHECK" 2>&1

echo "spot-check: write check -> $WRITECHECK"
exit 0
