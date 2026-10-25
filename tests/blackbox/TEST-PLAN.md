# Test Plan: moox public-contract black-box CLI and isolated tmux behavior

- **Spec**: ./spec.md
- **Contract**: ./contract/architecture-contracts.md; ./contract/validation-criteria.md; ./contract/test-environment.md; ./contract/cli-usage.md; ./contract/state-paths.md; ./contract/snapshot-format.md; ./contract/resurrect-snapshot-format.md
- **Stack**: go (+ POSIX sh safety wrapper)   **Framework**: Go standard testing, standalone module moox-blackbox   **Scenarios**: 45 families / 93 table-expanded cases (46 top-level Go tests)
- **Stop**: implement. No test execution, negative control, result or failure analysis is claimed.
- **Command for orchestrator**: from ./tests, `go test ./...` (optionally `-v -count=1`). Supply MOOX_BIN, TT_BIN, MOOX_SHARE; optional TMUX_BIN.

## Public surface

`moox version|--version`, `root`, `setup --yes --dry-run --mode replace|separate --no-claude-hooks --accessible`, `uninstall --yes|--dry-run`, `doctor --all --fix stale`, `save --direct --force`, `snapshot show --json`, `snapshot unpin FILE|--all`, `restore --snapshot FILE --dry-run`, and `import --from DIR|FILE --contents FILE --out FILE|--publish --report FILE`. Selectors follow their verbs exactly as §8.1 specifies; snapshot selectors follow show/unpin. Live servers use only nonce-owned names. Offline default-key import/unpin uses an inert tmux dependency that never runs tmux or contacts any socket; no default server is used. `tt --version`, `sessions --json`, `serve --ensure/--status --json/--stop`, and global `--tmux-socket`/`--tmux-bin` (before/after verbs). Environment/options/pointer files explicitly named in architecture-contracts.md are observable public state, not private internals.

## Safety and oracles

Every test/subcase owns fresh HOME, XDG_CONFIG_HOME/STATE_HOME/DATA_HOME/RUNTIME_DIR, TMPDIR and TMUX_TMPDIR. TMUX and user rc injection are not inherited. All panes are explicit /bin/sh; configured shell defaults are /bin/sh. Both cold servers load a temporary wrapper which sources the actual supplied moox.conf, never a mocked config. Manual-save tests disable automatic saves (1h debounce/max-wait, backup 0); event tests explicitly set 500ms debounce. No real HOME, real snapshot, default socket, implementation source or build-module import is used. A POSIX sh tmux guard blocks foreign/default sockets, all kill-server, and tool-client run-shell. Servers end by session IDs. Cleanup requests tt stop, inventories known server/pane/daemon descendants, checks process birth identity, and fails on surviving owned processes. No broad process kills.

All expected product values come from the supplied contracts. The 45s command and 5–35s eventual waits are harness deadlock bounds, not product performance promises. No internal-call mocks, invocation-count assertions, coverage-percentage claims, or pass-only tests. Missing binary/share variables skip the relevant tests exactly as required. No t.Parallel: this keeps process-tree bookkeeping simple while every case remains independently runnable.

## Scenarios

#### TEST-1: Moox version is an exact semver line and starts no server
- **Type**: integration
- **Priority**: high
- **Given** the supplied moox executable and an isolated environment. **When** --version is requested. **Then** exit 0, an exact moox <semver> line, and no environment file changes.
- **Requirement**: REQ-1; shared REQ-33.

#### TEST-2: tt version is an exact product semver line and starts no TUI
- **Type**: integration
- **Priority**: high
- **Given** the supplied tt executable. **When** --version is requested. **Then** exit 0, tt <semver> (moox), and no file changes.
- **Requirement**: REQ-1; shared REQ-33.

#### TEST-3: Root identifies the supplied share and required public artifacts
- **Type**: integration
- **Priority**: high
- **Given** a supplied installed share tree. **When** moox root is requested. **Then** the printed root resolves to MOOX_SHARE and all named required artifacts exist.
- **Requirement**: REQ-2, REQ-26; shared REQ-33.

#### TEST-4: Unknown verb returns usage exit 2 without writes
- **Type**: integration
- **Priority**: medium
- **Given** a fresh HOME. **When** a nonexistent verb is requested. **Then** exit 2 and unchanged files.
- **Requirement**: REQ-35; shared REQ-33.

#### TEST-5: Missing and tmux 3.5 dependencies return exit 69
- **Type**: integration
- **Priority**: high
- **Given** a missing dependency or harmless version-output dependency double. **When** setup --dry-run is requested. **Then** exit 69 and unchanged HOME (two subcases).
- **Requirement**: REQ-3, REQ-35; shared REQ-33.

#### TEST-6: Import publishing without an explicit socket is a usage error
- **Type**: integration
- **Priority**: high
- **Given** no selected socket and no live state. **When** import --publish is requested. **Then** exit 2 and unchanged HOME; this does not test valid import.
- **Requirement**: REQ-23, REQ-35; shared REQ-33.

#### TEST-7: Dry-run lists actions without writes with and without existing config
- **Type**: integration
- **Priority**: critical
- **Given** seeded personal files and either an existing tmux config or none. **When** setup --dry-run is requested. **Then** exit 0, visible proposed actions, and an identical complete isolated tree (two subcases).
- **Requirement**: REQ-5; shared REQ-33.

#### TEST-8: Yes setup creates new config and a repeat preserves configuration
- **Type**: integration
- **Priority**: high
- **Given** fake HOME without a tmux config and a preexisting local.conf. **When** setup --yes is requested twice. **Then** exit 0 twice, a tmux config referencing moox.conf, unchanged second-run config, and untouched local.conf.
- **Requirement**: REQ-6, REQ-7, REQ-27; shared REQ-33.

#### TEST-9: Existing config stays recoverable and uninstall restores exact bytes
- **Type**: integration
- **Priority**: critical
- **Given** existing config with CRLF and no trailing newline plus shell/Claude/local files. **When** setup --yes is repeated, then uninstall confirmed. **Then** original config remains kept/backed up, backups do not multiply, and all originals are restored byte-identically.
- **Requirement**: REQ-6, REQ-7, REQ-9; shared REQ-33.

#### TEST-10: Uninstall restores absence after a new installation
- **Type**: integration
- **Priority**: high
- **Given** no original tmux config but seeded shell/Claude/local files. **When** setup and uninstall are performed. **Then** new tmux config disappears and all seeded originals match byte-for-byte.
- **Requirement**: REQ-9; shared REQ-33.

#### TEST-11: Doctor reports checks and succeeds on a matched server
- **Type**: integration
- **Priority**: high
- **Given** an isolated matched server using the actual shipped config. **When** doctor is requested. **Then** exit 0 and visible checks; stale cleanup is covered separately in TEST-40.
- **Requirement**: REQ-10, REQ-35; shared REQ-33.

#### TEST-12: Direct save publishes a private valid same-directory pointer
- **Type**: integration
- **Priority**: critical
- **Given** a matched server with more than one window. **When** save --direct is requested. **Then** exit 0, sequence +1, a nonempty 0600 snapshot, a one-line 0600 basename pointer, a 0700 directory, and an active marker.
- **Requirement**: REQ-21, REQ-22, REQ-35; shared REQ-33.

#### TEST-13: Unchanged save succeeds without republishing
- **Type**: integration
- **Priority**: high
- **Given** a saved, unchanged server with contents capture off. **When** save --direct is repeated. **Then** exit 0, unchanged sequence, pointer, and target.
- **Requirement**: REQ-21, REQ-35; shared REQ-33.

#### TEST-14: Snapshot show is successful and nonmutating
- **Type**: integration
- **Priority**: medium
- **Given** a real generated snapshot. **When** snapshot show is requested. **Then** exit 0 and identical snapshot-directory payloads.
- **Requirement**: REQ-22, REQ-35; shared REQ-33.

#### TEST-15: Nothing-to-unpin returns 0 and preserves the snapshot
- **Type**: integration
- **Priority**: high
- **Given** a generated snapshot and nothing pinned. **When** snapshot unpin --all is requested. **Then** exit 0, preserved pointer and byte-identical snapshot. The call now supplies the required --all selector; TEST-34 adds nonempty pins at the exact runtime path.
- **Requirement**: REQ-22, REQ-35; shared REQ-33.

#### TEST-16: Restore refuses a populated server with exit 3
- **Type**: integration
- **Priority**: critical
- **Given** a valid saved snapshot and additional unsaved work. **When** restore is requested. **Then** exit 3, unchanged panes/geometry/metadata, and unchanged pointer.
- **Requirement**: REQ-39, REQ-35; shared REQ-33.

#### TEST-17: Local restore dry-run succeeds without changing work
- **Type**: integration
- **Priority**: high
- **Given** a valid saved snapshot on a populated server. **When** restore --dry-run is requested. **Then** exit 0 and no pane or snapshot mutation.
- **Requirement**: REQ-35; shared REQ-33.

#### TEST-18: Corrupt snapshot local dry-run returns exit 4
- **Type**: integration
- **Priority**: high
- **Given** an actual generated snapshot replaced with invalid bytes. **When** restore --dry-run is requested. **Then** exit 4 and unchanged current panes.
- **Requirement**: REQ-35; shared REQ-33.

#### TEST-19: An unparsable restoring option blocks a direct save
- **Type**: integration
- **Priority**: critical
- **Given** a saved workspace and the documented unparsable restore guard. **When** save --direct is requested. **Then** exit 75 (§12.4 "busy: … a restore/restart owns the server"; orchestrator ruling 2026-10-25 — 5 is only the clobber guard on snapshot content), unchanged snapshot files, and unchanged sequence. Test: `TestSnapshot_UnparseableRestoreGuardBlocksDirectSaveWithBusy75`.
- **Requirement**: REQ-17, REQ-35; shared REQ-33.

#### TEST-20: Matching versus nonmatching cold servers cannot cross state or restore
- **Type**: e2e
- **Priority**: critical
- **Given** a saved matching socket and fake legacy nonce directories. **When** a nonmatching socket starts cold on the shipped config and attempts save. **Then** active=1 versus 0, separate exported keys, slots/keys only on the matching server, no cloned workspace, save exit 3, no excluded-key files, and byte-identical other-socket/legacy state.
- **Requirement**: REQ-12, REQ-13, REQ-37; shared REQ-33.

#### TEST-21: Default policy, name lists, globs, wildcard and nonmatching policies are honored
- **Type**: integration
- **Priority**: critical
- **Given** one fresh nondefault socket for the default policy and each of four supplied policy forms. **When** the shipped config is loaded. **Then** the expected gate state, exported environment, hook slots and bindings appear without overwriting the policy.
- **Requirement**: REQ-12; shared REQ-33.

#### TEST-22: Reload exclusion removes participation
- **Type**: integration
- **Priority**: critical
- **Given** a previously active saved server. **When** the policy excludes it and shipped config is reloaded. **Then** active=0, no owned slots/bindings or active marker, save exit 3 and unchanged snapshots.
- **Requirement**: REQ-12, REQ-37; shared REQ-33.

#### TEST-23: Reload never restores or replaces an existing workspace
- **Type**: integration
- **Priority**: critical
- **Given** a saved server with additional unsaved window data. **When** the shipped config is reloaded. **Then** current observable workspace remains identical.
- **Requirement**: REQ-27, REQ-38; shared REQ-33.

#### TEST-24: tt ensure always exits 0 silently and is idempotent
- **Type**: integration
- **Priority**: high
- **Given** matched and excluded fresh servers. **When** serve --ensure is requested twice per server. **Then** literal exit 0, no stdout/stderr, unchanged participation and no excluded-key files.
- **Requirement**: REQ-36; shared REQ-33.

#### TEST-25: Headless layout burst causes exactly one debounced save
- **Type**: e2e
- **Priority**: high
- **Given** an active server without clients, status off, debounce 500ms and backup disabled. **When** new-window, split-window and kill-pane occur in one tmux command queue. **Then** one publish sequence increment, no duplicate for >2 debounce periods, and no #() in status-right.
- **Requirement**: REQ-15; shared REQ-33.

#### TEST-26: Cold shipped-config restore preserves layout and literal metadata
- **Type**: e2e
- **Priority**: critical
- **Given** 3 sessions, 6 windows, 7 /bin/sh panes, split geometry, distinct special-character cwds/titles/window names, and an injection marker path. **When** save is followed by ending all own sessions and a cold start on the same isolated socket/state. **Then** boot PASS, exact session/window/pane geometry/cwd/title rows, no bootstrap, no executed metadata, and unchanged last pointer.
- **Requirement**: REQ-18, REQ-19, REQ-37; shared REQ-33.

#### TEST-27: tt global socket and binary flags work before and after the verb
- **Type**: integration
- **Priority**: critical
- **Given** two matched servers with distinct nonce session names. **When** sessions --json is requested for each socket with flags in either position. **Then** valid JSON contains the selected exact session name and not the other (two position subcases).
- **Requirement**: REQ-14; shared REQ-33.

#### TEST-28: tt outside tmux honors socket and binary environment variables
- **Type**: integration
- **Priority**: critical
- **Given** two matched servers and no TMUX environment. **When** sessions --json is requested with TT_TMUX_SOCKET/TT_TMUX_BIN for each. **Then** valid JSON contains only the selected server's exact session name; the guard prevents default access.
- **Requirement**: REQ-14; shared REQ-33.

## Contract extension scenarios (TEST-29–TEST-45)

#### TEST-29: Invalid public syntax returns usage exit 2 without changes (12 cases)
- **Type**: integration
- **Priority**: high
- **Given** isolated files, a valid authored import source and an inert tmux dependency. **When** a required argument is absent, an enum/duration is invalid, output/publication conflict, or legacy publishing omits --replace-live. **Then** exit 2 without writes; unrelated missing-source failures cannot satisfy the publishing checks.
- **Requirement**: REQ-23, REQ-35.

#### TEST-30: Version verb matches the version flag
- **Type**: integration
- **Priority**: medium
- **Given** the supplied executable and a fresh environment. **When** version and --version are requested. **Then** identical moox metadata and no writes.
- **Requirement**: REQ-1.

#### TEST-31: Explicit replace and separate setup modes preserve the right originals (2 cases)
- **Type**: integration
- **Priority**: critical
- **Given** an existing tmux config plus shell/Claude/local files. **When** setup --yes --mode replace|separate --no-claude-hooks --accessible is repeated, then uninstall --yes. **Then** replace installs moox and backs up under XC/moox/backups; separate keeps the existing config; both create XC/moox/setup.json, preserve Claude settings, are idempotent and uninstall byte-identically.
- **Requirement**: REQ-6, REQ-7, REQ-9, REQ-13.

#### TEST-32: Uninstall dry-run is nonmutating
- **Type**: integration
- **Priority**: high
- **Given** an installed replace-mode setup. **When** uninstall --dry-run precedes uninstall --yes. **Then** dry-run changes nothing and the confirmed uninstall restores originals.
- **Requirement**: REQ-9.

#### TEST-33: Exact nondefault versus legacy-default path placement (2 tests)
- **Type**: integration
- **Priority**: critical
- **Given** distinct fake HOME and XDG roots. **When** a nondefault server saves, or an offline `-L default` import publishes (no --replace-live) under the relocated TMUX_TMPDIR that test-environment.md requires. **Then** nondefault snapshots/recordings/pending/panes/runtime/log options use the specified XS key tree; the relocated `default` is NOT the legacy key (ruling 2026-10-25, F1: legacy = canonical default socket at the stock path only): it gets `default@<hash>`, publishes under XS/moox/sockets/default@<hash>/, writes nothing to XD/tmux/resurrect, XS/moox/sockets/default, XS/tt/sockets/default, ~/.config/tmux-claude or frozen/default, and its unpin leaves a legacy runtime pins file untouched. Independent whole-fake-HOME/XDG scans reject stray copies. Tests: `TestPaths_NondefaultSaveUsesExactPerKeyPathsAndNoStrayLegacyArtifacts`, `TestPaths_RelocatedDefaultIsNotLegacyKeyAndWritesNoLegacyPaths`. TEST-29's `legacy_publish_missing_replace_live` case is removed (not applicable off the legacy key).
- **Requirement**: REQ-13, REQ-21, REQ-22, REQ-23, REQ-37.

#### TEST-34: Selective unpin and unpin-all preserve snapshots and update warning (2 cases)
- **Type**: integration
- **Priority**: critical
- **Given** two genuine-format pinned snapshot files and pins at the exact runtime path. **When** snapshot show --json, snapshot unpin FILE, and/or --all are requested. **Then** show is JSON, only selected basenames disappear, the warning counts down and clears, and JSON/pointer bytes remain unchanged.
- **Requirement**: REQ-22, REQ-35.

#### TEST-35: Save emits strict v1 schema and exact live values
- **Type**: integration
- **Priority**: critical
- **Given** real plain-shell panes, including empty and Unicode titles. **When** saved. **Then** all documented fields are present with correct types, no unknown writer fields, correct format/version/source, RFC3339 time, one-space indentation and trailing newline; socket/server/session/pane values match independent tmux observations and absent objects/strings are null/empty.
- **Requirement**: REQ-21.

#### TEST-36: Local validation is lenient for additive fields, strict for invalid/newer input (22 cases)
- **Type**: integration
- **Priority**: critical
- **Given** independently generated snapshots with one public JSON mutation. **When** restore --snapshot FILE --dry-run is requested. **Then** unknown additive fields succeed; missing required top-level/pane fields, wrong format/type, duplicates, empty/missing relations, absent client, invalid sid and mismatched layout format return 4; version 2 also says upgrade moox. Live panes, source snapshots and candidate bytes are unchanged.
- **Requirement**: REQ-21, REQ-35.

#### TEST-37: Authored resurrect rows import correctly from FILE or DIR (3 cases)
- **Type**: integration
- **Priority**: critical
- **Given** a normal pane, never-realigned collapsed title, realigned empty title, continuation line, SID/base64 launch/replay tags, first-space-escaped cwd, classic and JSON V2 layouts and exactly two-field state row. **When** import --from FILE|DIR --out FILE --report FILE reads a direct file, symlink last, or regular-file last. **Then** exact sessions/windows/panes/cwds/titles, commands, launch, SID, replay, layouts and empty client.last_session survive; the original tree stays byte-identical, report exists and --out publishes no pointer.
- **Requirement**: REQ-23, REQ-21.

#### TEST-38: Non-resurrect input is refused with exit 4
- **Type**: integration
- **Priority**: high
- **Given** an authored text file that is not resurrect. **When** import --from FILE --out FILE runs. **Then** exit 4 and no JSON output/pointer.
- **Requirement**: REQ-23, REQ-35.

#### TEST-39: Offline publishing accepts exact -L NAME and -S PATH syntax (2 cases)
- **Type**: integration
- **Priority**: critical
- **Given** the authored fixture and an inert tmux dependency. **When** import --publish explicitly selects a nonce-owned name or scratch path. **Then** a valid imported snapshot/pointer appears only under XS/moox/sockets/<exported snapshot key>/snapshots, with no legacy duplicate or source modifications.
- **Requirement**: REQ-23, REQ-13.

#### TEST-40: Stale cleanup requires both a missing socket and newest mtime over 14 days
- **Type**: integration
- **Priority**: critical
- **Given** real owned keys with 15-day missing state, an old tree with a 13-day child, a 15-day live socket, and old fake default/legacy payloads. **When** doctor --all then doctor --all --fix stale run. **Then** the first reports but preserves stale trees; the fix removes only old missing moox/tt trees and preserves recent-child/live/default/legacy payloads. No exact-14-day clock race is introduced.
- **Requirement**: REQ-10.

#### TEST-41: Bootstrap-only saves are pristine unless explicitly forced (2 cases)
- **Type**: integration
- **Priority**: critical
- **Given** a restorable saved workspace followed by an owned bootstrap-only server with automatic restore explicitly skipped. **When** save --direct runs with or without --force. **Then** unforced save returns 0 without replacing last; forced save publishes exactly once.
- **Requirement**: REQ-17, REQ-35.

#### TEST-42: A continuous stream saves by its configured ceiling; idle stops saving
- **Type**: integration
- **Priority**: high
- **Given** no clients/status, 1s debounce, 2s max-wait and backup disabled. **When** window renames continue every ~100ms and later stop. **Then** a publish occurs despite continuing changes, the final rename is eventually saved and a clean idle interval emits no further saves. Scheduling allowance is explicit, not a 30s-boundary certification.
- **Requirement**: REQ-16, REQ-15.

#### TEST-43: Capture emits exact pane-ID gzip framing and hash
- **Type**: integration
- **Priority**: critical
- **Given** isolated /bin/sh panes that print distinct nonce markers. **When** capture-on save runs. **Then** the snapshot links a private, sha256-verified gzip; its exact server-life header and byte-length records cover every pane by saved_id, with no cross-pane text.
- **Requirement**: REQ-20, REQ-21.

#### TEST-44: Contents reuse never follows a dead position or previous server life (2 cases)
- **Type**: integration
- **Priority**: critical
- **Given** captured panes and then capture off. **When** pane A is killed, B shifts position and new C occupies another position, or the socket starts a new server life. **Then** same-life B retains B's frame by pane ID and C has no captured frame; a new server reuses none of the old life.
- **Requirement**: REQ-20, REQ-21.

#### TEST-45: Explicit resurrect contents import preserves mapping and read-only archive
- **Type**: integration
- **Priority**: critical
- **Given** a valid authored pane_contents.tar.gz with sparse ./pane_contents/pane-S:W.P entries. **When** import --from FILE --contents FILE --out FILE runs. **Then** only matching pane rows have contents, exact payloads map through saved_id into versioned gzip frames, and last/TSV/archive bytes remain unchanged.
- **Requirement**: REQ-23, REQ-20, REQ-21.

## Coverage map

Coverage is design coverage, not execution evidence. “partial” is not full requirement satisfaction. 28/39 requirements have some authored coverage: 11 full, 17 partial; 11 uncovered. Scenario types: 42 integration families (90 expanded cases), 3 e2e; no unit/component tests because no callable library/component contract was supplied.

| Requirement | Tests | Covered |
|---|---|---|
| REQ-1: moox/tt version commands print the specified semver lines without starting a TUI | TEST-1, TEST-2, TEST-30 | yes |
| REQ-2: moox root exposes the named installed share-root artifacts | TEST-3 | yes |
| REQ-3: Use stock tmux >=3.6 and return 69 for missing/older tmux | TEST-5 | partial |
| REQ-4: First moox run automatically sets up and start/attaches; nested use warns/refuses with 2 | None | no |
| REQ-5: Setup --dry-run lists actions and makes no changes with and without existing config | TEST-7 | yes |
| REQ-6: Setup is noninteractive by default except the existing-config replace/back-up versus separate -L moox choice; explicit --mode selects both choices | TEST-8, TEST-9, TEST-31 | partial |
| REQ-7: Repeated setup is idempotent and does not multiply backups or alter managed configuration | TEST-8, TEST-9, TEST-31 | yes |
| REQ-8: Setup detects theme, offers Claude hooks/statusline once, adds the right shell hook, gives a clear non-zsh message, doctor/welcome/five keys, and uses huh wizard style | None | no |
| REQ-9: Uninstall restores preexisting configuration byte-identically and removes config that had no original; dry-run is nonmutating | TEST-9, TEST-10, TEST-31, TEST-32 | yes |
| REQ-10: moox/tt doctor diagnose and clean missing-socket state only when newest mtime >14 days; default/legacy never candidates | TEST-11, TEST-40 | partial |
| REQ-11: Restart saves, ends only its server, starts and restores after upgrade | None | no |
| REQ-12: @moox-sockets defaults to default without overriding user policy and supports lists/globs/*; excluded servers retain only keys/theme and lose participation after reload | TEST-20, TEST-21, TEST-22 | yes |
| REQ-13: Each socket isolates snapshot, recording launch/pending/panes, logs, tt config/state and freeze data; default retains legacy paths; shell policy export needs no tmux fork; no cross-socket state accesses | TEST-20, TEST-31, TEST-33, TEST-39 | partial |
| REQ-14: One tt instance/view per server; socket/bin flags before or after command/TUI, env selection outside tmux, and popups target the same server | TEST-27, TEST-28 | partial |
| REQ-15: Listed layout/session/client hooks mark dirty cheaply, debounce to one save without clients/status; status only displays save state | TEST-25, TEST-42 | partial |
| REQ-16: A steady stream saves by 30s ceiling; dirty-only ~15min backup in tt obeys deadline/no-ticker/idle/detach rules; idle never saves | TEST-42 | partial |
| REQ-17: All saves are suppressed until actual restore LAUNCH completion; bootstrap-only servers never clobber a restorable pointer | TEST-19, TEST-41 | partial |
| REQ-18: Save/end/cold start restores multiple sessions/windows/panes, layout geometry and distinct cwds automatically without clients or duplicate-session failures | TEST-26 | yes |
| REQ-19: Restored session/window/title/cwd metadata containing #, #(...), semicolons, dots, colons or backslashes remains byte-identical and never executes | TEST-26 | partial |
| REQ-20: Full engine parity: pane-local contents capture, nvim sessions, process rules (~tt observe --pane->exit), exact recorded commands/inner semicolons, Claude --resume, wt, grouped/zoomed/frozen/observer panes, heal broken last, tt handover and seven historical resume bugs | TEST-43, TEST-44, TEST-45 | partial |
| REQ-21: Snapshots are versioned, strict-write/lenient-read with all §6.1 fields, one-space JSON/newline, required/relational validation, pane-ID gzip/hash/server-life rules, atomic temp+rename, private files/dirs and non-dangling moox-last | TEST-12, TEST-13, TEST-33, TEST-35, TEST-36, TEST-37, TEST-43, TEST-44, TEST-45 | partial |
| REQ-22: moox-last/restoring-from are one basename line; pins are basename lines; non-PASS sources retained until qualifying PASS or unpin and warning visible meanwhile | TEST-12, TEST-14, TEST-15, TEST-33, TEST-34 | partial |
| REQ-23: Import current resurrect snapshot plus continuity recordings and contents without modifying resurrect last/tmux_resurrect_*/archive; --publish requires -L/-S and --replace-live for legacy default | TEST-6, TEST-29, TEST-33, TEST-37, TEST-38, TEST-39, TEST-45 | partial |
| REQ-24: Save is cheaper than pgrep resurrect at ~66 panes, targets one list-panes call and avoids O(panes×processes) scans with recordings | None | no |
| REQ-25: Release delivers four darwin/linux × amd64/arm64 tarballs, checksums/formula/README/LICENSE/THIRD_PARTY/rpaster, no build-host paths, Linux curl installer and brew install/test without HOME writes | None | no |
| REQ-26: Pinned vendored sensible/yank/fingers/open/jump/pain-control/tmux-fzf/fzf-url/vim-navigator/catppuccin/copy-image load directly without TPM, with helper binaries and licenses; continuity is vendored | TEST-3 | partial |
| REQ-27: Portable one-root config has no home/dotfiles/plugin hardcoding, reload binding uses config_files, optional local.conf is last and never overwritten | TEST-8, TEST-23 | partial |
| REQ-28: Dotfiles becomes personal-only consumer; task setup uses brew not stow/TPM/tt build; iTerm skip, README op, stale go/bin comment and fingers install fixed | None | no |
| REQ-29: Before publishing, scan emails/hosts/tailnet IDs/keys/tokens/personal paths; keep product fixture names and redact client/personal data | None | no |
| REQ-30: Create public MadAppGang/moox MIT repo/release/tap formula; CI uses scoped HOMEBREW_TAP_TOKEN or reports local fallback; copy-image MIT/module rename and upstream left intact | None | no |
| REQ-31: Final live cutover backs up, installs/points consumer/sources running config without killing server or changing session count; validate isolated snapshot first | None | no |
| REQ-32: Both zsh preexec and v1 bash PROMPT_COMMAND record/resume hooks meet the continuity contract | None | no |
| REQ-33: Testing isolation: fresh HOME/XDG/runtime/TMUX_TMPDIR, own socket/-f, TMUX unset, /bin/sh only, no default/run-shell/kill-server/broad kills, assert cleanup of owned processes | TEST-1–TEST-45 (harness; default-key cases use an inert dependency, never a default server) | yes |
| REQ-34: MIT moox license; every vendored plugin license retained; copy-image licensing/module rename permitted | None | no |
| REQ-35: Public exit matrix: 0 ok/unchanged/pristine/local dry-run, 1 unexpected, 2 usage/nesting, 3 refused/nonmatched, 4 invalid/newer/import, 5 guard, 69 tmux, 75 busy, 124 wait, 130 abort | TEST-4, TEST-5, TEST-6, TEST-11–TEST-19, TEST-29, TEST-34, TEST-36, TEST-38, TEST-41 | partial |
| REQ-36: tt serve --ensure always exits exactly 0 and prints nothing, including repeated/already-running or excluded cases | TEST-24 | yes |
| REQ-37: Socket gate exports documented path/env values and active 1/0; matched cold snapshot >1 window restores automatically and drops bootstrap; excluded has no hooks/daemon/keys/files | TEST-20, TEST-22, TEST-26, TEST-33 | partial |
| REQ-38: A config reload never initiates restore | TEST-23 | yes |
| REQ-39: Restore into a populated server is refused with exit 3 and does not mutate work | TEST-16 | yes |

## Known gaps

- **Legacy-key branch untestable under test-environment.md** (REQ-13, REQ-23, REQ-35): the legacy `default` key (XD/tmux/resurrect snapshots, XS/moox/sockets/default runtime, `--replace-live` requirement and its exit 2) applies only to the canonical default socket at the stock path. test-environment.md mandates a fresh TMUX_TMPDIR and forbids the default socket, so no test can reach it; only the negative (a relocated `default` is not legacy) is asserted.
- **tt sessions per-server scoping** (REQ-14/V7): `tt sessions` reports Claude Code sessions and made no tmux call; a positive "only a's session" oracle needs a Claude registry fixture whose format is not in the contract.
- **Contents reuse on a structural save** (REQ-20): the contract does not say which saves are structural; a capture-off save publishes contents:null, which is accepted when no row claims contents.

- **REQ-3**: Rejects missing and 3.5; exact accepted 3.6 boundary/platform matrix is not exercised.
- **REQ-4**: §8.1 now gives start/attach syntax, but no PTY test/confirmation protocol is supplied. First-run attachment and nesting UI are not exercised.
- **REQ-6**: --mode replace/separate, --yes, --no-claude-hooks and --accessible are exercised. The interactive existing-config question and actual attachment to the separate-mode -L moox server are not certified; no manifest fields or exact prose are invented.
- **REQ-8**: Offer/form protocol, welcome key list and expected messages are not supplied; no interactive setup harness is invented.
- **REQ-10**: moox doctor healthy/report/fix-stale paths cover the supplied >14-day, newest-child, missing/live socket and never-default/legacy rules. tt doctor syntax/status fields and old/new log-location reporting remain untested.
- **REQ-11**: The safe restart request/snapshot protocol in §8.1/§6.4a is absent; user forbids kill-server from tests.
- **REQ-13**: Exact nondefault exported paths/options and snapshot/runtime/pin placement are checked independently, including scans for stray artifacts. Offline legacy-default snapshot and runtime/pin paths are checked with an inert dependency. No live default server is started, so legacy continuity-dir tightening, actual default tt paths and freeze writes remain untested; key derivation is unspecified and uses the exported key. Shell no-fork behavior is not measured.
- **REQ-14**: JSON views and env/CLI routing are covered. Popup construction and interactive TUI routing are not exposed by the supplied CLI contract.
- **REQ-15**: Tests the new/split/kill burst and no-client/status case; does not independently stimulate every named hook or measure cheapness.
- **REQ-16**: A 2s configured ceiling under a continuing stream and subsequent idle no-publish interval are covered. Exact default 30s boundary, ~15min backup, attached/detached deadline transitions and absence of an armed internal timer are not claimed.
- **REQ-17**: Unparsable guard, cold-restore pointer, bootstrap-only pristine and explicit --force transitions are covered. A real held-restore/LAUNCH transition is not authored.
- **REQ-19**: Exercises #/#(...)/;/backslash in titles/windows and space/#/;/backslash cwds. Session dot/colon/group handling is not exercised.
- **REQ-20**: The supplied contents framing enables real shell capture, same-life pane-ID-safe reuse, no cross-life reuse and archived import payloads. Nvim/process rules, actual typed-command/Claude/wt relaunch, grouped/zoomed/frozen/observer parity, heal/handover and seven historical cases still need §6.4–6.8/recording guidance. zsh/bash hook panes are expressly forbidden here.
- **REQ-21**: v1 fields/types/presence/formatting, lenient additive reading, required/relational/version validation, pointer/privacy, gzip/hash/server identity and reuse are authored. Atomic interruption/crash windows, missing/corrupt contents degradation and alternate-screen capture are not exercised; no internal call-count or writer atomicity claim is made.
- **REQ-22**: Exact pins path (including legacy snapshots elsewhere), FILE/--all clearing, warning count/clear and byte-preserving unpin are covered. Actual restoring-from creation/framing and retention through non-PASS then qualifying equal-or-larger PASS are not exercised.
- **REQ-23**: Valid authored FILE/DIR imports (symlink and regular last), all requested row shapes/tags/layouts, two-field state, --out/--report/--contents, both publication selectors, legacy --replace-live and invalid input are covered. No live workspace is copied; continuity launch/<N> file schema, --attach-live process joining, grouped-session import and archive traversal/security cases are not covered.
- **REQ-24**: Real snapshot/baseline implementation and live reads are forbidden; no performance claim or internal call-count test is made.
- **REQ-25**: No release archive/installer/formula artifact inputs; no network, brew writes or Linux container operations authorized by this sandbox task.
- **REQ-26**: Asserts public PINS/LICENSES/copy-image and shell artifacts exist, not contents, every plugin/helper, pinned revisions or runtime plugin behavior. No rpaster input is supplied.
- **REQ-27**: Covers local.conf preservation and no restore on source reload. Does not read shipped implementation or check binding command internals/source ordering.
- **REQ-28**: Consumer implementation is outside sandbox/contract and not an executable surface supplied here.
- **REQ-29**: No publish.sh or public release-tree scan artifact input. Shipped implementation inspection is forbidden.
- **REQ-30**: Publishing and repository mutations are outside the isolated black-box test task and lack contract inputs.
- **REQ-31**: Live HOME/server access is expressly forbidden. No attempt is made to claim V11 evidence.
- **REQ-32**: This test environment expressly restricts panes to /bin/sh or sleep. zsh/bash preexec/PROMPT_COMMAND hooks and launch/ack file schemas remain outside the supplied contract.
- **REQ-34**: No root LICENSE input/release archive is supplied. LICENSES existence alone is not a legal-content test.
- **REQ-35**: Concrete 0/2/3/4/5/69 cases now include invalid/newer/additive schemas, invalid/valid import, exact required syntax and pristine/force saves. Unexpected/busy/wait/abort/nesting flows remain untested; safe lock-owner/request and PTY protocols are missing.
- **REQ-37**: Active/env/hooks/keys/markers/excluded-key files and cold PASS are covered. Daemon status JSON fields and full slot table §5.1 are missing; no undocumented daemon-PID shape is asserted.

## Contract omissions and validation limitations

- The extension supplies §4.2, §6.1, §8.1 and resurrect row/archive formats. §3.3/§4.3–4.4 details, full §5.1/5.5, §6.4a/6.5/6.7/6.8, §8.3 manifest fields, §9.7 and §11 remain absent. No discovery outside the sandbox was attempted.
- All new schema assertions and resurrect fixtures come from the newly supplied contracts, not product source or real HOME. Fixtures are generated per test/subcase; no hidden implementation helpers or product-module imports are used. Default-key publication is strictly offline with a dependency fixture that cannot run real tmux.
- Previous broad --yes tests remain intact; explicit --mode cases now select both branches. Separate-mode attachment to literal -L moox is still not exercised because only owned nonce names are used by real servers and no PTY protocol was supplied. Exact dry-run prose and manifest fields are not asserted.
- Corrected old calls to the now-explicit syntax: selectors follow verbs; snapshot unpin supplies FILE or --all; uninstall supplies --yes rather than assuming a stdin prompt. Existing import no-socket refusal now has a valid --from, so it cannot pass for the wrong missing-argument reason.
- Tests pass MOOX_ROOT=MOOX_SHARE to nested scripts and require moox root to identify that supplied installed tree. Orchestrator binaries must be laid out/configured to locate the corresponding installed share; no undocumented root flag is used.
- V1 uses fake legacy HOME trees rather than real-dir hashes or a real-home sandbox-exec control, because this task forbids all real HOME reads/writes. This cannot certify the full real-data V1/V4/V5/V11 procedures. V8–V10 publishing/install procedures and the V6 historical regression inventory are likewise not claimed.
- Missing extra coverage/framework-signals reference files are not discovered outside LOADOUT. The copied Go/testing guidance suffices for these black-box tests; code-coverage gates and terminal-mux automation are not used.
- Session-artifact obstacle: qa/test-plan.md already exists outside TEST_DIR. Reading it is refused by the allowlist, so it is preserved rather than overwritten unread. The updated canonical tests/TEST-PLAN.md is also copied to the new qa/test-plan-extension.md; the caller may promote that copy to the standard session filename.

## Execution and mandatory future negative control

No suite is run under STOP_AT: implement. After the orchestrator supplies built artifacts, run the above command and save full output. Then temporarily change TEST-1's required `moox ` prefix to `wrong-product `, run `go test -run '^TestCLI_MooxVersionPrintsSemverWithoutStartingServer$' -count=1`, require an assertion failure (not SKIP/build failure), paste that failure in results, and restore the expectation. A skipped-only run or a negative control that cannot fail is not proven. Run/analyse artifacts are N/A for this delivery. V1–V11's global PASS rule is not satisfied by authored tests alone.
