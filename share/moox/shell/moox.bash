# moox.bash — `moox setup` adds one guarded line to ~/.bashrc that sources this:
#   [ -r "<moox share root>/shell/moox.bash" ] && . "<moox share root>/shell/moox.bash"
# Source it LAST in ~/.bashrc: the hook wraps PROMPT_COMMAND, and a later
# assignment would replace the wrapper. It loads the vendored continuity hook
# from the same share root. Safe to source more than once.
# shellcheck shell=bash
_moox_shell_dir="${BASH_SOURCE[0]%/*}"
[ "$_moox_shell_dir" = "${BASH_SOURCE[0]}" ] && _moox_shell_dir=.
# shellcheck source=../plugins/tmux-claude-continuity/claude-continuity.bash
. "$_moox_shell_dir/../plugins/tmux-claude-continuity/claude-continuity.bash"
unset _moox_shell_dir
