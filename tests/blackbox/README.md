# moox black-box tests

Standalone module `moox-blackbox`; Go standard library only. No product-module imports,
implementation reads, fixture copies from the live HOME, network access, or package-manager
operations are needed.

## Run later (not run during authoring)

The orchestrator supplies the built artifacts:

```sh
export MOOX_BIN=/absolute/path/to/moox
export TT_BIN=/absolute/path/to/tt
export MOOX_SHARE=/absolute/path/to/share/moox
export TMUX_BIN=/absolute/path/to/tmux  # optional; otherwise tmux on PATH
cd tests
go test ./...
```

Go 1.26 (the main module's, what CI installs), macOS/Linux, stock tmux >=3.6, /bin/sh and ps are required.
A test skips when an artifact variable it needs is unset; a configured but missing or
non-executable artifact is an error, not a silent skip. The shipped share must correspond
to the supplied binaries. PATH is supplemented with canonical moox/tt names, and
MOOX_ROOT is set to the supplied share for nested shipped scripts.

There are **45 scenario families / 93 expanded cases** (46 top-level Go tests). Each has a TEST-n and REQ-n
mapping in TEST-PLAN.md. No suite, compiler check or negative control was run under
STOP_AT: implement. Only gofmt syntax/format processing was performed; this is **not**
evidence that tests compile or pass.

## Isolation and cleanup

- Every case/subcase owns short fresh /tmp directories for HOME, each XDG root,
  TMPDIR, TMUX_TMPDIR and all sockets. TMUX and user shell-startup injection are
  not inherited. Only /bin/sh panes are started, with /bin/sh as the default.
- A temporary config wrapper sources **MOOX_SHARE/moox.conf** and sets only
  safety, socket-policy, geometry/indexing and save-test controls.
- No default server, live HOME/state, broad pkill, kill-server, or tool-client
  run-shell is used. Legacy-default **key** tests perform offline imports/unpin
  against an inert tmux dependency (version output, otherwise no server); that
  fixture cannot execute tmux or contact any socket. A shell guard rejects those tmux operations and foreign
  sockets and fails cleanup if any rejected attempt occurred.
- Servers end by their own session IDs; tt is stopped on its explicitly selected
  socket. Owned server/pane/daemon PID identities and descendants are inventoried,
  and cleanup fails on a survivor. After reporting a leak, only birth-checked
  owned descendants may receive termination signals; never a tmux server.
- The CLI's normally nonexistent selected socket is registered for cleanup too,
  in case an incorrect metadata/setup command unexpectedly starts a server.
- Event tests use condition polling and public publish sequences. The 45s command
  bound and eventual-condition deadlines detect a stuck harness, not performance
  compliance.

## Scope limits — not a full V1–V11 certification

The new §8.1, §4.2, §6.1 and resurrect-format contracts close the earlier
syntax/path/schema/import authoring blockers. The original suite is retained;
only calls now contradicted by the exact CLI syntax were corrected.

Added coverage:
- `--mode replace|separate`, `--no-claude-hooks`, `--accessible`, uninstall
  `--yes|--dry-run`, version alias and 12 concrete invalid-syntax cases.
- Exact nondefault path formulas independent of exported path strings; offline
  legacy snapshots in XD/tmux/resurrect versus runtime/pins under XS. Whole fake
  HOME/XDG scans reject stray copies in fallback or other-key directories.
- Nonempty pins, selective FILE/all clearing, warning count/clear, JSON show,
  and preservation of pointer/snapshot bytes.
- Strict v1 writer field/type/presence checks, explicit null/empty values,
  one-space formatting/newline, live pane metadata, and 22 additive/invalid/newer
  reader-validation cases.
- Authored normal/collapsed/realigned resurrect rows, continuation, SID/base64
  launch/replay tags, escaped cwd, classic/JSON V2 layout and two-field state.
  FILE and both last forms, --out, --report, --publish -L/-S and --contents are
  exercised without reading real data or changing source last/TSV/archive.
- Stale >14-day AND missing-socket cleanup; recent-child/live/default/legacy
  preservation; bootstrap pristine/--force; configured max-wait and clean idle.
- Real /bin/sh contents capture with exact gzip framing/hash/server identity,
  pane-ID reuse after index shifts and no reuse across server lives.

Still not claimed:
- Live default-server continuity/tt/freeze behavior (forbidden by the sandbox),
  shell-hook zsh/bash panes, interactive attachment to separate-mode -L moox,
  manifest fields not supplied in §8.3, actual launch/ack/retention transitions,
  the full 30s/15min scheduling matrix, nvim/frozen/observer strategy parity,
  real-data imports/benchmarks, release/publishing/live cutover and the complete
  historical-bug matrix.
- The exported nondefault state key is used because its algorithm is not supplied;
  all path formulas are independently asserted from the contract.
- These are authored tests, not execution evidence. See TEST-PLAN.md for every
  requirement's coverage and remaining gaps. A new session copy is in
  qa/test-plan-extension.md relative to the sandbox root. The preexisting
  qa/test-plan.md was not read or overwritten: it is outside the permitted
  existing-test read surface.

## Future negative control

After a real artifact-backed suite run, temporarily change TEST-1's exact
`moox ` version prefix to `wrong-product `, run:

```sh
go test -run '^TestCLI_MooxVersionPrintsSemverWithoutStartingServer$' -count=1
```

Require a behavioral assertion failure, save that failure verbatim, and restore
the expectation. A skipped test, compiler error, missing executable or harness
setup error does not prove this negative control.

## Files Read

Input paths are relative to the supplied sandbox except the explicitly requested
dispatch task file. No implementation source or real HOME was read.

- `/tmp/claudish-mux-501/launch-Ji59b1/turn-1.md`
- `./spec.md`
- `./contract/architecture-contracts.md`
- `./contract/cli-usage.md`
- `./contract/resurrect-snapshot-format.md`
- `./contract/snapshot-format.md`
- `./contract/state-paths.md`
- `./contract/test-environment.md`
- `./contract/validation-criteria.md`
- `./loadout/dev_9.5.0_skills_core_testing-strategies_SKILL.md`
- `./loadout/dev_9.5.0_skills_discipline_test-driven-development_SKILL.md`
- `./loadout/go_0.2.0_knowledge_references_testing-patterns.md`
- `./loadout/go_0.2.0_knowledge_roles_tester_best-practices.md`
- `./loadout/terminal_5.5.0_skills_terminal-interaction_SKILL.md`
- `./tests/TEST-PLAN.md`
- `./tests/README.md`
- `./tests/go.mod`
- `./tests/harness_test.go`
- `./tests/cli_test.go`
- `./tests/setup_test.go`
- `./tests/snapshot_test.go`
- `./tests/socket_test.go`
- `./tests/roundtrip_test.go`
- `./tests/save_events_test.go`
- `./tests/tt_test.go`

The formatter additionally parsed the newly authored cli_contract_test.go,
snapshot_schema_test.go, import_test.go, state_paths_test.go, doctor_stale_test.go,
save_guards_test.go, contents_test.go and import_contents_test.go under tests/.
No product source was read. No files were opened outside the sandbox after
the explicitly requested dispatch file.
