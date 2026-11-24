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

// bareArgChar is one character of a bare (unquoted) argument: anything but whitespace and
// quotes, where a backslash comes with the byte it escapes. A shell escape ("-pMy\$x", raw JSON's
// "\\$") is then part of the value, but a backslash before a quote ("\"", the end of a JSON
// string), before whitespace or in a "\n" "\r" "\t" escape (the next line, in raw JSON) ends
// it, so the escape stays and the JSON stays valid.
const bareArgChar = `(?:[^\s'"` + "`" + `\\]|\\[^\s'"` + "`" + `nrt])`

// argValue is a flag's value: double-quoted or single-quoted (spaces allowed), or bare. The
// value is whichever of its three groups matched (anchored.anyGroup).
const argValue = `(?:"([^"\\\r\n]+)"|'([^'\\\r\n]+)'|(` + bareArgChar + `+))`

// curlUser is curl's "user:password" argument, quoted or bare; the password is whichever of
// its three groups matched. A quoted password may hold spaces ('alice:one two3').
const curlUser = `(?:'[^':\\\r\n]{1,64}:([^'\\\r\n]+)'|"[^":\\\r\n]{1,64}:([^"\\\r\n]+)"|[^:\s'"` + "`" + `\\]{1,64}:(` + bareArgChar + `+))`

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
		// The first -p, and the password attached: escaped-double-quoted (raw JSON),
		// double-quoted, single-quoted, or bare. A bare one may not start with "-", so "-p -h db"
		// is kept. A quoted one must end its argument (a space, ";", "|", "&", ")", the end, the
		// other kind of quote closing an outer one (sh -c 'mysql -p"..."'), or in raw JSON the
		// string's end or an escaped break), so a quote that closes a JSON string
		// ("mysql -p", 3306) or a quoted phrase ('mysql -p' and 'mysql -h') is not taken for one.
		// A double-quoted one may not hold JSON's punctuation at either end, nor, when an outer
		// single quote closes it, a single quote ('mysql -p"' + x + '"' is two pieces of code);
		// a single-quoted one may not hold a double quote: what lies between two JSON strings,
		// or two quoted pieces of code, is not a password.
		cat: catPassword, triggers: []string{"mysql"}, anyGroup: true, span: 1024, before: true, segment: true,
		re: regexp.MustCompile(`^mysql(?:dump|admin|sh)?[ \t]+(?:[^\n|;&]*?[ \t])??-p(?:` +
			`\\"([^"\\\r\n]+)\\"(?:[\s;|&)"']|\\[nrt]|$)|` +
			`"([^"\\\r\n,:\[\]{}\s](?:[^"\\\r\n]*[^"\\\r\n,:\[\]{}\s])?)"(?:[\s;|&)]|$)|` +
			`"([^"'\\\r\n,:\[\]{}\s](?:[^"\\\r\n]*[^"'\\\r\n,:\[\]{}\s])?)"'|` +
			`'([^'"\\\r\n]+)'(?:[\s;|&)"]|\\[nrt"]|$)|` +
			`((?:[^\s'"` + "`" + `\\-]|\\[^\s'"` + "`" + `nrt])` + bareArgChar + `*))`),
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
