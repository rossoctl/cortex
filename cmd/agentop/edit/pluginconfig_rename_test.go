package edit

import (
	"strings"
	"testing"
)

func rename(path []string, to string) ConfigChange {
	return ConfigChange{Chain: "outbound", Plugin: "inference-router", Path: path, RenameTo: to}
}

// renameErr is SetPluginConfig's error for ch on src, failing when there is none.
func renameErr(t *testing.T, src string, ch ConfigChange) string {
	t.Helper()
	_, err := SetPluginConfig([]byte(src), ch)
	if err == nil {
		t.Fatal("SetPluginConfig: want an error, got none")
	}
	return err.Error()
}

// Only the key's own text changes: its value, the comment on its value's line, the
// comments around it and its place among its siblings all stay.
func TestSetPluginConfig_RenamesAKeyInPlace(t *testing.T) {
	src := strings.Replace(withRouter, "          servers:\n            ete:\n",
		"          servers:\n            # the external gateway\n            ete:   # added by agentop\n", 1)
	// A second server after it, so the order is seen to stay.
	src = strings.Replace(src, "              key: sk-ete   # rotated in October\n",
		"              key: sk-ete   # rotated in October\n            glm:\n              url: https://glm.example.com\n              key: sk-glm\n", 1)
	got := apply(t, src, rename([]string{"servers", "ete"}, "litellm-external"))
	assertYAML(t, got, strings.Replace(src, "            ete:   # added by agentop\n", "            litellm-external:   # added by agentop\n", 1))
}

// A name YAML would read back as a number or a boolean is quoted, so it stays the
// string the router keys servers by.
func TestSetPluginConfig_RenameQuotesANameThatWouldNotReadBackAsAString(t *testing.T) {
	for _, name := range []string{"123", "true", "null"} {
		got := apply(t, withRouter, rename([]string{"servers", "ete"}, name))
		if !strings.Contains(got, "            \""+name+"\":\n") {
			t.Errorf("rename to %s:\n%s", name, got)
		}
	}
}

func TestSetPluginConfig_RenamesAQuotedKey(t *testing.T) {
	for _, quoted := range []string{`"ete"`, `'ete'`} {
		src := strings.Replace(withRouter, "            ete:\n", "            "+quoted+":\n", 1)
		got := apply(t, src, rename([]string{"servers", "ete"}, "zai"))
		assertYAML(t, got, strings.Replace(src, "            "+quoted+":\n", "            zai:\n", 1))
	}
}

func TestSetPluginConfig_RenameRefusesAMissingKey(t *testing.T) {
	if err := renameErr(t, withRouter, rename([]string{"servers", "glm"}, "zai")); !strings.Contains(err, "does not exist") {
		t.Errorf("err = %s", err)
	}
	if err := renameErr(t, localConfig, rename([]string{"servers", "ete"}, "zai")); !strings.Contains(err, "has no inference-router entry") {
		t.Errorf("err = %s", err)
	}
}

func TestSetPluginConfig_RenameRefusesATakenName(t *testing.T) {
	src := strings.Replace(withRouter, "          agents:\n",
		"            zai:\n              url: https://zai.example.com\n              key: sk-zai\n          agents:\n", 1)
	if err := renameErr(t, src, rename([]string{"servers", "ete"}, "zai")); !strings.Contains(err, "already has zai") {
		t.Errorf("err = %s", err)
	}
}

// What cannot be edited as one key's text on one line is refused, not guessed at.
func TestSetPluginConfig_RenameRefusesWhatItCannotEditLineByLine(t *testing.T) {
	for name, edit := range map[string][2]string{
		"servers in flow style": {"          servers:\n            ete:\n              url: https://ete.example.com\n              key: sk-ete   # rotated in October\n",
			"          servers: {ete: {url: https://ete.example.com, key: sk-ete}}\n"},
		"a tagged key":  {"            ete:\n", "            !!str ete:\n"},
		"a complex key": {"            ete:\n", "            ? ete\n            :\n"},
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(withRouter, edit[0], edit[1], 1)
			if src == withRouter {
				t.Fatal("the fixture edit did not apply")
			}
			if err := renameErr(t, src, rename([]string{"servers", "ete"}, "zai")); !strings.Contains(err, "by hand") {
				t.Errorf("err = %s, want it to say to rename by hand", err)
			}
		})
	}
}

// A value written as an environment variable reference stays that reference: the
// rename never copies a value, so nothing is expanded into the file.
func TestSetPluginConfig_RenameKeepsEnvReferencesLiteral(t *testing.T) {
	src := strings.Replace(withRouter, "key: sk-ete   # rotated in October", "key: ${ETE_KEY}", 1)
	got := apply(t, src, rename([]string{"servers", "ete"}, "zai"))
	if !strings.Contains(got, "            zai:\n              url: https://ete.example.com\n              key: ${ETE_KEY}\n") {
		t.Errorf("result:\n%s", got)
	}
}

func TestSetPluginConfig_RenameWantsNoValue(t *testing.T) {
	ch := rename([]string{"servers", "ete"}, "zai")
	ch.Value = ScalarValue("x")
	if err := renameErr(t, withRouter, ch); !strings.Contains(err, "a rename sets no value") {
		t.Errorf("err = %s", err)
	}
}
