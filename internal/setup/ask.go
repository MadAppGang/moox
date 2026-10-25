package setup

import (
	"errors"

	"github.com/charmbracelet/huh"
)

// Ask is the interactive form (§8.3 step 3): at most two questions — what to
// do about an existing tmux config, and whether to add the Claude Code
// hooks. Only asked when there is something to decide. The caller shows it
// only when stdin and stdout are terminals and --yes was not given.
// huh.ErrUserAborted becomes ErrUserAborted (exit 130, nothing written).
func Ask(env Env, p Plan, f Facts, accessible bool) (Plan, error) {
	c := p.Choices
	var groups []*huh.Group
	mode := string(c.Mode)
	if f.UserConfig() {
		groups = append(groups, huh.NewGroup(
			huh.NewSelect[string]().
				Title("You already have a tmux config").
				Description("moox can leave it alone and run as its own server, or take over plain `tmux`.").
				Options(
					huh.NewOption("Keep my config, run moox as its own server (`moox`)", string(Separate)),
					huh.NewOption("Back up my config and replace it (plain `tmux` becomes moox)", string(Replace)),
				).
				Value(&mode),
		))
	}
	hooks := c.ClaudeHooks
	if f.ClaudeDir {
		desc := "SessionStart/Stop hooks in ~/.claude/settings.json let a restored pane resume its Claude session."
		if !f.Jq {
			desc += " (jq is not installed; the SessionStart hook needs it.)"
		}
		groups = append(groups, huh.NewGroup(
			huh.NewConfirm().
				Title("Add the Claude Code hooks?").
				Description(desc).
				Affirmative("Yes").
				Negative("No").
				Value(&hooks),
		))
	}
	if len(groups) == 0 {
		return p, nil
	}
	form := huh.NewForm(groups...).WithAccessible(accessible).WithShowHelp(true)
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return p, ErrUserAborted
		}
		return p, err
	}
	c.Mode = Mode(mode)
	c.ClaudeHooks = hooks
	return Build(env, f, c), nil
}
