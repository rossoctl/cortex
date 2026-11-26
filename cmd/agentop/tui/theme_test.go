package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The palette's choice: AGENTOP_THEME wins over the setting, then COLORFGBG, then lipgloss's
// own query. A bad value is reported and ignored.
func TestApplyTheme(t *testing.T) {
	orig := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(orig) })
	for _, tc := range []struct {
		env, setting, fgbg string
		want               string
		dark, wantErr      bool
	}{
		{env: "light", setting: "dark", want: "light"},
		{env: "DARK", want: "dark", dark: true},
		{setting: "light", fgbg: "0;15", want: "light"},
		{env: "auto", setting: "dark", fgbg: "15;0", want: "dark", dark: true}, // auto: the terminal decides
		{fgbg: "0;default;15", want: "light"},
		{fgbg: "15;8", want: "dark", dark: true},
		{env: "solarized", fgbg: "0;7", want: "light", wantErr: true},
		{want: ""},
	} {
		lipgloss.SetHasDarkBackground(!tc.dark) // the opposite, so a no-op shows
		got, err := ApplyTheme(tc.env, tc.setting, tc.fgbg)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%+v: got %q, err %v", tc, got, err)
		}
		if got != "" && lipgloss.HasDarkBackground() != tc.dark {
			t.Errorf("%+v: dark background = %v", tc, lipgloss.HasDarkBackground())
		}
	}
}

// The zebra row on a light palette is a soft grey, not the dark palette's near-black.
func TestZebraRowOnALightPalette(t *testing.T) {
	orig := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(orig) })
	if _, err := ApplyTheme("light", "", ""); err != nil {
		t.Fatal(err)
	}
	if colorZebraBg.Light != "#F3F4F6" || lipgloss.HasDarkBackground() {
		t.Errorf("the light zebra is %q, dark background %v", colorZebraBg.Light, lipgloss.HasDarkBackground())
	}
}
