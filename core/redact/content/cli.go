package content

import (
	"regexp"
	"strings"
)

// THE COMMAND-LINE FORMS: a secret passed as a separate argument, which the keyword rule (an
// assignment) does not see. They are kept narrow: known flag names and known commands only.
//   - "--password <v>", "--token <v>" and the like;
//   - "docker login ... -p <v>" (and podman);
//   - "mysql ... -p<v>", the password attached to the flag (with a space, mysql prompts);
//   - "curl ... -u user:<v>".
// A value that is a placeholder or a reference is kept, as for the keyword rule. A flag's
// password must not be lowercase letters only, so help text ("--password option") is kept.

// argValue is a flag's value: double-quoted or single-quoted (spaces allowed), or bare. The
// value is whichever of its three groups matched (anchored.anyGroup). No value holds a
// backslash, so in raw JSON text a value stops before an escape ("-p\"x") and the escape stays.
const argValue = "(?:\"([^\"\\\\\\r\\n]+)\"|'([^'\\\\\\r\\n]+)'|([^\\s'\"`\\\\]+))"

// curlUser is curl's "user:password" argument, quoted or bare; the password is whichever of
// its three groups matched. A quoted password may hold spaces ('alice:one two3').
const curlUser = "(?:'[^':\\\\\\r\\n]{1,64}:([^'\\\\\\r\\n]+)'|\"[^\":\\\\\\r\\n]{1,64}:([^\"\\\\\\r\\n]+)\"|[^:\\s'\"`\\\\]{1,64}:([^\\s'\"`\\\\]+))"

// cliPassword checks a password given as a flag's value: not another flag, not a lowercase word
// (help text: "--password option"), and password-shaped. A quoted one may hold spaces.
func cliPassword(v string) bool {
	mode := valueBare
	if strings.ContainsAny(v, " \t") {
		mode = valueQuoted
	}
	return v[0] != '-' && !isLowerWord(v) && secretShaped(tierPassword, v, mode)
}

// The rules that look past their command's name for a flag (docker, mysql, curl) scan to the end
// of the command at most: a line break, a pipe, ";" or "&". When one finds no flag, the next
// trigger in the same command would scan what it already did, so the rule skips to where its
// scan ended (anchored.segment). That keeps "curl -u curl -u ..." linear.
var cliRules = []*anchored{
	{
		cat: catPassword, triggers: []string{"--pass", "--pwd"}, anyGroup: true, span: 512, before: true,
		re:    regexp.MustCompile(`^--(?:password|passwd|pass|pwd)(?:[ \t]+|=)` + argValue),
		check: cliPassword,
	},
	{
		cat:      catSecretValue,
		triggers: []string{"--token", "--secret", "--api-key", "--apikey", "--client-secret", "--access-token", "--auth-token"},
		anyGroup: true, span: 512, before: true,
		re:    regexp.MustCompile(`^--(?:token|secret|api-key|apikey|client-secret|access-token|auth-token)[ \t]+` + argValue),
		check: func(v string) bool { return v[0] != '-' && secretShaped(tierSecret, v, valueBare) },
	},
	{
		cat: catPassword, triggers: []string{"docker", "podman"}, anyGroup: true, span: 1024, before: true, segment: true,
		re:    regexp.MustCompile(`^(?:docker|podman)[ \t]+login[ \t](?:[^\n|;&]*?[ \t])?(?:-p|--password)(?:[ \t]+|=)` + argValue),
		check: cliPassword,
	},
	{
		cat: catPassword, triggers: []string{"mysql"}, group: 1, span: 1024, before: true, segment: true,
		re:    regexp.MustCompile("^mysql(?:dump|admin|sh)?[ \\t](?:[^\\n|;&]*?[ \\t])?-p([^\\s'\"`\\\\-][^\\s'\"`\\\\]*)"),
		check: func(v string) bool { return len(v) >= 4 && !notASecret(v) },
	},
	{
		// -u, a combined -sSu, or --user, then user:password.
		cat: catPassword, triggers: []string{"curl"}, anyGroup: true, span: 2048, before: true, segment: true,
		re:    regexp.MustCompile("^curl[ \\t](?:[^\\n|;&]*?[ \\t])?(?:-[A-Za-z]*u|--user)(?:[ \\t]+|=)" + curlUser),
		check: func(v string) bool { return !notASecret(v) },
	},
}

// isLowerWord reports whether v is lowercase letters only: a word, not a password a tool is
// given on its command line, as far as this rule is concerned.
func isLowerWord(v string) bool {
	return strings.Trim(v, "abcdefghijklmnopqrstuvwxyz") == ""
}
