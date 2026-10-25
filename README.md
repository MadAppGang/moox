# moox

An agentic tmux distribution: one command gives you a complete tmux setup —
config, theme, pinned plugins, the `tt` session manager and sidebar, Claude Code
continuity, and an event-driven save/restore engine that replaces
tmux-resurrect and tmux-continuum.

## Install

macOS or Linux with Homebrew:

```sh
brew install madappgang/tap/moox
moox
```

Linux without Homebrew:

```sh
curl -fsSL https://raw.githubusercontent.com/MadAppGang/moox/main/install.sh | sh
moox
```

The installer verifies the archive against the release's `checksums.txt`, unpacks
it into `~/.local/share/moox/<version>/`, points `~/.local/share/moox/current` at
it and links `moox` and `tt` into `~/.local/bin`. The copy-image helper
`rpaster` stays in `~/.local/share/moox/current/libexec/` (brew: the keg's
`libexec/`) and is never linked as `rpaster`, so a separately installed rpaster
(`madappgang/tap/rpaster`, or its own installer's `~/.local/bin/rpaster`) is left
alone; run moox's copy as `moox rpaster ...`. It only warns about
missing dependencies (tmux >= 3.6, fzf, jq, zsh or bash, optionally
tmux-fingers and ruby); it never installs them. `install.sh --uninstall` removes
exactly what it installed (see `~/.local/share/moox/install.json`).

The first `moox` runs `moox setup`. It asks before it changes anything, backs
up an existing `~/.tmux.conf`, and records every change so `moox uninstall`
restores your files byte for byte. `moox setup --dry-run` lists the actions
without writing anything.

## Commands

```
moox [-L NAME | -S PATH] [-s SESSION]   start or attach
moox setup [--yes] [--dry-run] [--mode replace|separate] [--claude-hooks]
moox doctor [--json] [--fix stale|legacy-hooks|restoring|all]
moox uninstall [--yes] [--dry-run] [--purge]
moox save | restore | restart | import | snapshot show|unpin
moox root | version
moox rpaster [--which | ARGS...]         run the bundled copy-image helper (rpaster)
tt                                       session manager TUI (also: tt serve, tt sessions)
```

`tt` here is moox's tmux session manager. It conflicts with Tarantool's `tt`;
the Homebrew formula declares the conflict.

## Licence

MIT, see `LICENSE`. Vendored plugins keep their own licences; see
`THIRD_PARTY.md` and `share/moox/LICENSES/`.
