# tmux-claude-continuity (vendored in moox)

Upstream: https://github.com/MadAppGang/tmux-claude-continuity (MIT, see `LICENSE`),
vendored at the commit recorded in `share/moox/plugins/PINS`.

moox carries edits to this copy; `THIRD_PARTY.md` at the module root lists them. In short:

- **Removed:** `pre_save.sh`, `pre_restore.sh`, `post_restore.sh`, `verify_launch.sh`,
  `tmux-claude-continuity.tmux`, `scripts/doctor.sh` and their tests. moox's Go engine
  saves and restores (it replaces tmux-resurrect), and `moox doctor` replaces the checks.
  The `prefix Z` freeze manager binding moved to moox's `conf/active.conf`.
- **`claude-continuity.zsh`** trusts the gate's `MOOX_*` environment only when
  `MOOX_SOCKET` names the server the shell runs in, re-tests a participation marker at
  every write, and resumes from `MOOX_RESUME` / `MOOX_RESTORE_CONTENT` on the first
  prompt (record, then ack, then run).
- **`claude-continuity.bash`** (new): the same contract for bash 3.2 and later.
- **Scripts:** every tmux call addresses the server the process lives in (`$TMUX_CMD`,
  never the bare default); the by-pid registry is one shared directory, never derived
  from the per-socket panes dir; position sidecars are written only by a participating
  server; tab reconcile ignores Claude records from other servers; thaw clears
  `allow-set-title`; titles go through `_cc_lit`.

The hooks are loaded by `share/moox/shell/moox.zsh` and `moox.bash`, which `moox setup`
adds to the shell rc file. The Claude Code hooks (`scripts/on_session_start.sh`,
`scripts/on_stop.sh`) are registered in `~/.claude/settings.json` by `moox setup`.

Tests: `moox/tests/hook-*.test.sh` and friends (moox's own), plus the kept upstream tests
in `tests/` (`bash tests/run_all.sh`).
