# Third-party software in moox

moox is MIT-licensed (`LICENSE`). It ships the third-party software below. Each
plugin is vendored at the pinned commit, with its `.git`, CI config, tests and
media removed; its license is in `share/moox/LICENSES/<name>.LICENSE`. This file is
generated from `share/moox/plugins/PINS` by `scripts/third-party-md.sh`.

## tt

`cmd/tt` seeded `internal/observer/adapters/` from **ssnxd/agentbar** (MIT). Its
notice, with the full license text and exactly what was taken, is
`cmd/tt/THIRD_PARTY.md`.

## Vendored tmux plugins

| Plugin | Upstream | Commit | License | Shipped as |
|---|---|---|---|---|
| tmux-sensible | https://github.com/tmux-plugins/tmux-sensible | `25cb91f42d020f675bb0a2ce3fbd3a5d96119efa` | MIT (`LICENSE.md`) | unmodified |
| tmux-yank | https://github.com/tmux-plugins/tmux-yank | `acfd36e4fcba99f8310a7dfb432111c242fe7392` | MIT (`LICENSE.md`) | unmodified |
| tmux-open | https://github.com/tmux-plugins/tmux-open | `763d0a852e6703ce0f5090a508330012a7e6788e` | MIT (`LICENSE.md`) | unmodified |
| tmux-pain-control | https://github.com/tmux-plugins/tmux-pain-control | `32b760f6652f2305dfef0acd444afc311cf5c077` | MIT (`LICENSE.md`) | unmodified |
| tmux-jump | https://github.com/schasse/tmux-jump | `2ff4940f043cd4ad80fa25c6efa33063fb3b386b` | GPL-3.0 (`LICENSE`) | unmodified |
| tmux-fzf | https://github.com/sainnhe/tmux-fzf | `05af76daa2487575b93a4f604693b00969f19c2f` | MIT (`LICENSE`) | unmodified |
| tmux-fzf-url | https://github.com/wfxr/tmux-fzf-url | `c71d2113b4cb00eb159ad723fc5072ff5ec738b8` | MIT (`LICENSE.txt`) | unmodified |
| vim-tmux-navigator | https://github.com/christoomey/vim-tmux-navigator | `e41c431a0c7b7388ae7ba341f01a0d217eb3a432` | MIT (`License.md`) | unmodified |
| catppuccin-tmux | https://github.com/catppuccin/tmux | `d2d25bd3393fe43f19eb4fff6cdd2bdf5578e622` | MIT (`LICENSE`) | unmodified |
| tmux-claude-continuity | https://github.com/MadAppGang/tmux-claude-continuity | `3dc7b5ac7f41aa0b6bf565cc9096af9076ec2195` | MIT (`LICENSE`) | **modified by moox** (below) |
| tmux-copy-image | https://github.com/MadAppGang/tmux-copy-image | `f1c33b434c3922c5370e1f9a8911a1d19eb174e6` | MIT (`LICENSE`) | plugin half in `share/moox/plugins/tmux-copy-image`, Go source in `third_party/tmux-copy-image` |

**tmux-jump is GPL-3.0.** moox distributes it unmodified, as a separate program in
an aggregate, with its complete source (`share/moox/plugins/tmux-jump`) and its
license (`share/moox/LICENSES/tmux-jump.LICENSE`). It is not relicensed, and no
moox code links to it; tmux runs it as a script. It needs `ruby`.

**tmux-fzf** is MIT and also carries the Anti-996 License; both texts are shipped
(`LICENSES/tmux-fzf.LICENSE`, `LICENSES/tmux-fzf.Anti-996-LICENSE`).

**tmux-fzf-url**'s `LICENSE.txt` says `MIT © wfxr` and points at
wfxr.mit-license.org; that file is shipped as-is and the MIT text it refers to is
shipped beside it (`LICENSES/tmux-fzf-url.MIT-text`).

**tmux-fingers** is not vendored. It is a dependency of the Homebrew formula and is
loaded with `tmux-fingers load-config` when it is on `PATH`.

**tmux-copy-image** (MIT, MadAppGang): the pinned commit is upstream's
`moox-vendor-pin` branch: one change adds the license, renames the Go module to
`github.com/MadAppGang/tmux-copy-image` and makes the plugin's hooks indexed
(`[105]`), backgrounded and silent; the next makes `paste.sh` work from its key
binding (the pane id is passed in, a run-shell job has no `TMUX_PANE`) and parse
the `/image/meta` reply as data instead of `eval`-ing it. moox ships that tree
unmodified. Its helper `rpaster` is built from `third_party/tmux-copy-image` and
installed in moox's `libexec/`, not on `PATH` (run it as `moox rpaster`), so it
never collides with a separately installed rpaster.

## Edits moox carries in tmux-claude-continuity

The upstream tree at the pinned commit, with these changes (the moox repository's
history has the exact diffs):

- **Removed:** `scripts/pre_save.sh`, `pre_restore.sh`, `post_restore.sh`,
  `verify_launch.sh`, `doctor.sh`, `tmux-claude-continuity.tmux`, `docs/`, and the tests
  of the removed scripts. The moox engine saves and restores; `moox doctor` checks.
- **`claude-continuity.zsh`:** the gate's `MOOX_*` environment is trusted only when
  `MOOX_SOCKET` equals the pane's own `${TMUX%%,*}`; the no-env branch is only the real
  login default server (realpath compare); a participation marker (or, without env, a
  veto file) is re-tested at every record, clear and consume; first-prompt resume from
  `MOOX_RESUME`/`MOOX_RESTORE_CONTENT`/`MOOX_RESUME_ACK` (record, ack, run).
- **`claude-continuity.bash`:** new, the same contract for bash 3.2+.
- **`scripts/lib/cc_common.sh`:** `TMUX_CMD` addresses the process's own server
  (never the bare default, always `-u`); `_cc_participating`, `_cc_on_legacy_default`,
  `_cc_by_pid_dir`, `_cc_panes_dir`, `_cc_start_token`, `_cc_lit`,
  `_cc_require_server`; `_cc_socket_ns` uses `MOOX_STATE_KEY`; the default log moved
  to the per-socket state dir.
- **`on_session_start.sh`, `on_stop.sh`:** position sidecars only for a participating
  server; the shared by-pid registry with a `<pid>.start` token; no tmux call without
  a pane.
- **`cc_freeze.sh`, `cc_thaw.sh`, `cc_popup.sh`:** the by-pid dir from
  `_cc_by_pid_dir`; saves through `moox save --wait 30s` instead of resurrect; thaw
  refuses on a non-participating server and clears `allow-set-title`; titles through
  `_cc_lit`.
- **`cc_tab_reconcile.sh`:** ignores Claude records whose process does not descend
  from one of this server's panes.
- **`tests/`:** the kept upstream tests set `@claude-continuity-by-pid-dir`, tear down
  their scratch server without `kill-server`, and run with `TMUX` unset.
