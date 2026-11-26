package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// THE PALETTE: light or dark. Every colour here is a lipgloss.AdaptiveColor, whose Light and Dark
// halves lipgloss picks by asking the terminal for its background colour. Where the terminal
// cannot answer (the Windows console, Windows Terminal, many SSH sessions), lipgloss assumes
// DARK, and on a light terminal the dark palette paints every second table row near-black.
//
// So the choice can be made:
//   - AGENTOP_THEME=light|dark|auto, which wins;
//   - else `theme: light|dark|auto` in agentop's user config;
//   - auto (the default) reads COLORFGBG first, which xterm, rxvt, Konsole and some others set
//     ("15;0": foreground white, background black). Where it is not set, lipgloss asks the
//     terminal, and assumes dark when the terminal does not answer.

// ThemeEnv is the variable that picks the palette.
const ThemeEnv = "AGENTOP_THEME"

// ApplyTheme sets the palette before the first render, from AGENTOP_THEME (env), the user
// config's theme (setting), and COLORFGBG (fgbg). It returns what it chose ("light", "dark" or
// "" when it left the choice to lipgloss) and an error naming a value it did not understand,
// which is then ignored.
func ApplyTheme(env, setting, fgbg string) (string, error) {
	var bad error
	for _, v := range []struct{ name, val string }{{ThemeEnv, env}, {"theme", setting}} {
		switch strings.ToLower(strings.TrimSpace(v.val)) {
		case "":
			continue
		case "light":
			lipgloss.SetHasDarkBackground(false)
			return "light", bad
		case "dark":
			lipgloss.SetHasDarkBackground(true)
			return "dark", bad
		case "auto":
		default:
			if bad == nil {
				bad = fmt.Errorf("%s=%q: want light, dark or auto; using auto", v.name, v.val)
			}
			continue
		}
		break // auto: stop at the first source that names it
	}
	if dark, ok := darkFromColorFGBG(fgbg); ok {
		lipgloss.SetHasDarkBackground(dark)
		if dark {
			return "dark", bad
		}
		return "light", bad
	}
	return "", bad
}

// darkFromColorFGBG reads the background from COLORFGBG ("fg;bg" or "fg;default;bg"): colours
// 0 to 6 and 8 are dark, 7 and 9 to 15 light, as rxvt and xterm number them.
func darkFromColorFGBG(v string) (dark, ok bool) {
	if v == "" {
		return false, false
	}
	parts := strings.Split(v, ";")
	bg, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil || bg < 0 || bg > 15 {
		return false, false
	}
	return bg <= 6 || bg == 8, true
}
