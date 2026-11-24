package content

import (
	"encoding/base64"
	"strings"
	"testing"
)

// REGRESSIONS from the maintainer's review: secret names ending in a digit, wrapped base64, and
// reference and call checks that looked at one character instead of a shape.

// hntPassword is a password with mixed letters and digits, from pieces.
func hntPassword() string { return "Hnt7r" + "Qx2Lp" }

// A name's trailing digit is not part of its word: Django's password1 and password2, TOKEN2.
func TestRegression_NamesEndingInADigit(t *testing.T) {
	pw, tok := hntPassword(), mixed(27)
	for _, tc := range []struct{ in, secret string }{
		{"password1=" + pw, pw},
		{"password2=" + pw, pw},
		{`{"password2":"` + pw + `"}`, pw},
		{"pwd1=" + pw, pw},
		{"token2=" + tok, tok},
		{"api_key2=" + tok, tok},
		{"API_KEY_2=" + tok, tok},
	} {
		if out, _ := redact(tc.in); strings.Contains(out, tc.secret) {
			t.Errorf("%q: leaked: %q", tc.in, out)
		}
	}
	for _, key := range []string{"password2", "newPassword2"} {
		if e := New().Find(pw, Context{Key: key}); len(e) != 1 {
			t.Errorf("Find under %q: %v", key, e)
		}
	}
}

// Wrapped base64 (GNU base64 wraps at 76 columns; MIME and PEM at 64 or 76) is decoded as one run,
// so a secret that crosses a line break is found, in text and in raw JSON, and the block is
// replaced whole.
// The maintainer's sweep: .env bodies of 60 to 110 bytes ending in a 40-character API_TOKEN,
// wrapped at 76. A two-line block whose base64 needs no padding is decoded as one run too.
func TestRegression_WrappedBase64TwoLineSweep(t *testing.T) {
	tok := mixed(40)
	for n := 60; n <= 110; n++ {
		tail := "\nAPI_TOKEN=" + tok
		env := "# " + strings.Repeat("x", max(0, n-len(tail)-2)) + tail
		enc := base64.StdEncoding.EncodeToString([]byte(env))
		var lines []string
		for i := 0; i < len(enc); i += 76 {
			lines = append(lines, enc[i:min(len(enc), i+76)])
		}
		for name, in := range map[string]string{
			"text":     "env:\n" + strings.Join(lines, "\n") + "\n",
			"raw JSON": `{"out":"` + strings.Join(lines, `\n`) + `\n"}`,
		} {
			if out, st := redact(in); st[catEncoded] != 1 || strings.Contains(out, enc[len(enc)-12:]) {
				t.Errorf("%d bytes (%d of base64), %s: not replaced whole: %q", len(env), len(enc), name, out)
			}
		}
	}
}

// A longer line just above a narrower wrapped block (a digest, a one-line base64 value) does not
// take the block's first line as its own last line: the block is decoded from its first line.
func TestRegression_WrappedBlockBelowALongLine(t *testing.T) {
	gh := fakes()["github"]
	env := "DB_HOST=db.internal.example\nAPI_TOKEN=" + gh + "\nLOG_LEVEL=info\nREGION=eu\n"
	enc := base64.StdEncoding.EncodeToString([]byte(env))
	var lines []string
	for i := 0; i < len(enc); i += 64 {
		lines = append(lines, enc[i:min(len(enc), i+64)])
	}
	block := strings.Join(lines, "\n")
	one := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("one line value ", 4)))[:76]
	for name, above := range map[string]string{
		"hex digest":        strings.Repeat("3f9a0c7e", 12),
		"a one-line value":  one,
		"a 128-char digest": "SHA2-512(.env)= " + strings.Repeat("ab12cd34", 16),
	} {
		in := above + "\n" + block + "\n"
		if out, st := redact(in); st[catEncoded] == 0 || strings.Contains(out, lines[1]) {
			t.Errorf("%s above: the block was not found: %q (%v)", name, out, st)
		}
	}
}

func TestRegression_WrappedBase64(t *testing.T) {
	tok, pw := mixed(54), hntPassword()+"Rt9k"+"Wm2q"
	env := "APP_NAME=demo\nDB_HOST=db.internal\nAPI_TOKEN=" + tok + "\nDB_PASSWORD=" + pw + "\nLOG_LEVEL=info\n"
	enc := base64.StdEncoding.EncodeToString([]byte(env))
	wrap := func(s string, w int, br string) string {
		var lines []string
		for i := 0; i < len(s); i += w {
			lines = append(lines, s[i:min(len(s), i+w)])
		}
		return strings.Join(lines, br)
	}
	for _, w := range []int{76, 64} {
		for name, in := range map[string]string{
			"text":     "env:\n" + wrap(enc, w, "\n") + "\nend",
			"CRLF":     "env:\r\n" + wrap(enc, w, "\r\n") + "\r\nend",
			"raw JSON": `{"out":"` + wrap(enc, w, `\n`) + `\n","n":1}`,
		} {
			out, st := redact(in)
			if st[catEncoded] != 1 || strings.Contains(out, enc[60:70]) {
				t.Errorf("width %d, %s: not replaced whole (%v): %q", w, name, st, out)
			}
			if !strings.HasSuffix(out, "end") && !strings.HasSuffix(out, `","n":1}`) {
				t.Errorf("width %d, %s: the text after the block changed: %q", w, name, out)
			}
			if name == "raw JSON" {
				mustBeJSON(t, name, out)
			}
		}
	}
	// One base64 value followed by an ordinary line is not a wrapped block: it is decoded on its
	// own, and the line after it is kept, whatever the value's length.
	for _, n := range []int{0, 1, 2, 3} {
		x := base64.RawStdEncoding.EncodeToString([]byte(env[:len(env)-n]))
		for _, after := range []string{"\nDEBUG=1\n", "\nPORT=8080\n", "\nok", "\nDone.", "\nx", "\ntotal 8", "\nLOG_LEVEL=info"} {
			in := "CONFIG_B64=" + x + after
			out, st := redact(in)
			if st[catEncoded] != 1 || !strings.HasSuffix(out, after) || strings.Contains(out, x) {
				t.Errorf("one value (%d chars) then %q: %q (%v)", len(x), after, out, st)
			}
		}
	}
	// A whole block that ends at a full line, then a short word: the word is kept.
	full := base64.StdEncoding.EncodeToString([]byte(env + strings.Repeat("#", 171-len(env)))) // 228 = 3 lines of 76
	for _, after := range []string{"\nx", "\ntotal 8", "\nend", "\nDone", "\nOK"} {
		out, st := redact(wrap(full, 76, "\n") + after)
		if st[catEncoded] != 1 || !strings.HasSuffix(out, after) {
			t.Errorf("3 full lines then %q: %q (%v)", after, out, st)
		}
	}
	// Two encoded files, one after the other: one fills two full lines, the other is the next,
	// shorter line. Each is replaced, and nothing of the second is left.
	aws := base64.StdEncoding.EncodeToString([]byte("[default]\naws_access_key_id = " + fakes()["aws"] + "\nregion = us-east-1\n" + strings.Repeat("# pad\n", 9)))
	gh := base64.RawStdEncoding.EncodeToString([]byte("GITHUB_TOKEN=" + fakes()["github"]))
	for _, br := range []string{"\n", "\r\n", `\n`} {
		in := wrap(aws[:152], 76, br) + br + gh + br + "end"
		if out, _ := redact(in); strings.Contains(out, gh[:20]) || strings.Contains(out, aws[:20]) {
			t.Errorf("two files (%q): left encoded text: %q", br, out)
		}
	}
	// A secret that crosses into a block's short last line is replaced with the whole block.
	tail := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("#", 150) + "\nAPI_TOKEN=" + tok + "\n"))
	if out, _ := redact(wrap(tail, 76, "\n") + "\nend"); out != "[REDACTED:encoded_secret]\nend" {
		t.Errorf("a secret in the last line: %q", out)
	}
	// A padded short line after one value is the text that follows it, and is kept.
	x := base64.RawStdEncoding.EncodeToString([]byte(env + "##"))
	for _, after := range []string{"\nAPP_KEY=\nPORT=1\n", "\nDSN=\n", "\nab==\n"} {
		out, st := redact("CONFIG_B64=" + x + after)
		if st[catEncoded] != 1 || !strings.HasSuffix(out, after) {
			t.Errorf("one value (%d chars) then %q: %q", len(x), after, out)
		}
	}
	// A run that decodes with the "n" of a "\n" escape before it: the marker starts after the
	// letter, so the raw JSON stays valid.
	in := `{"c":"\n` + enc[1:] + `"}`
	if out, st := redact(in); st[catEncoded] != 1 || strings.Contains(out, `\[REDACTED`) {
		t.Errorf("a run on an escape's letter: %q (%v)", out, st)
	} else {
		mustBeJSON(t, "a run on an escape's letter", out)
	}
	// A wrapped block over 64 KiB is decoded line by line, as an unwrapped run is.
	long := base64.StdEncoding.EncodeToString([]byte("API_TOKEN=" + tok + "\n" + strings.Repeat("# filler line of text\n", 3500)))
	if _, st := redact(wrap(long, 76, "\n")); st[catEncoded] == 0 {
		t.Errorf("a wrapped block over 64 KiB: not found")
	}
	// A run right after a backslash in plain text ("C:\" then base64 that starts with "b") is
	// still decoded whole.
	for _, in := range []string{`C:\` + enc, `{"p":"C:\\` + enc + `"}`} {
		if _, st := redact(in); st[catEncoded] != 1 {
			t.Errorf("%.12q...: not found (%v)", in, st)
		}
	}
	// Wrapped base64 of text with no secret, and lines of different widths, are kept.
	plain := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("just a readme line\n", 12)))
	for _, in := range []string{
		wrap(plain, 76, "\n"),
		wrap(enc[:76], 76, "\n") + "\n" + enc[76:76+80] + "\n", // a longer line: not the block's
	} {
		if out, st := redact(in); in == wrap(plain, 76, "\n") && (out != in || len(st) != 0) {
			t.Errorf("changed: %q (%v)", out, st)
		}
	}
}

// A reference is kept only when it has a reference's whole shape; a password that merely starts
// with "$", "%", "{", "~", "<" or "[" is redacted.
func TestRegression_ReferenceShapes(t *testing.T) {
	pw := hntPassword()
	for _, in := range []string{
		`{"password":"$` + pw + `"}`,
		`password: "$` + pw + `!"`,
		"DB_PASSWORD=%" + pw,
		"DB_PASSWORD=~" + pw,
		// a bare value cannot start with "{" or "<" (code), so these are quoted
		`password: "{` + pw + `}"`,
		`{"password":"<` + pw + `"}`,
	} {
		if out, _ := redact(in); strings.Contains(out, pw) {
			t.Errorf("%q: leaked: %q", in, out)
		}
	}
	if e := New().Find("$"+pw, Context{Key: "password"}); len(e) != 1 {
		t.Errorf("Find($...) under password: %v", e)
	}
	for _, in := range []string{
		"DB_PASSWORD=${DB_PASSWORD}", `password: "${DB_PASSWORD:-x}"`, "password=$DB_PASSWORD",
		"password=$dbPassword", "password=%DB_PASSWORD%", `password: "{{ db_password }}"`, "password={password}",
		"password=<your-password>", "password=~/secrets/db.txt", "password=[REDACTED:password]",
		"password=$user.password",
		// GitHub Actions, Helm, Jinja, shell and Kubernetes substitutions
		`password: "${{ secrets.DB_PASSWORD }}"`, `{"password": "${{ secrets.DB_PASSWORD }}"}`,
		`api_key: "${{secrets.OPENAI_API_KEY}}"`, `password: "{{ .Values.postgresql.password }}"`,
		`password: "{{ lookup('env', 'DB_PASSWORD') }}"`, `PGPASSWORD="$(cat /run/secrets/db_password)"`,
		`password: "$(DB_PASSWORD)"`, `password: "${DB_PASSWORD?}"`, `password: "$HOME/.pw"`,
		`password: "$DB_USER:$DB_PASS"`, `password: "%(db_password)s"`, `password: "{password!r}"`,
		`password: "{env:DB_PASSWORD}"`, `password: "<<DB_PASSWORD>>"`, `password: "[MASKED]"`,
		`password: "~admin"`, `password = "${data.vault_generic_secret.db.data["password"]}"`,
		// PowerShell and PHP
		"$password = $env:DB_PASSWORD", "$password = $this->password;", "$pass = $user->getPassword()",
		// Symfony, Puppet, Azure Pipelines, Compose, batch, PHP
		"secret: '%env(APP_SECRET)%'", "password: '%env(DATABASE_PASSWORD)%'", `password: "%{lookup('db_password')}"`,
		`password: "%{DB_PASSWORD}"`, `password: "$[variables.dbPassword]"`, "PASSWORD=$$DB_PASSWORD",
		`password: "{$password}"`, `password: "[$DB_PASSWORD]"`, `password: "%~dp0secret.txt"`, "password=$DB_PASS2", "password=$HOME/.pw",
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q", in, out)
		}
	}
}

// A chosen password after "$" (a word and digits, perhaps a symbol) is no reference: "$NAME"
// needs capitals to end in digits, and to stand at the end or before "/ : . - } { [".
func TestRegression_DollarWordAndDigits(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"DB_PASSWORD=$Summer" + "2024", "Summer2024"},
		{`{"password":"$Summer` + `2024!"}`, "Summer2024"},
		{`{"password":"$hunter` + `22"}`, "hunter22"},
		{`password: "$Wel` + `come1"`, "Welcome1"},
	} {
		if out, _ := redact(tc.in); strings.Contains(out, tc.secret) {
			t.Errorf("%q: leaked: %q", tc.in, out)
		}
	}
	if e := New().Find("$Summer"+"2024", Context{Key: "password"}); len(e) != 1 {
		t.Errorf("Find under password: %v", e)
	}
}

// A bare value followed by "(", "[" or "<" is code only when it is an identifier.
func TestRegression_CallShapes(t *testing.T) {
	pw := hntPassword()
	for _, in := range []string{"DB_PASSWORD=" + pw + "(9)", "DB_PASSWORD=" + pw + "[1]", "DB_PASSWORD=" + pw + "<x"} {
		if out, _ := redact(in); strings.Contains(out, pw) {
			t.Errorf("%q: leaked: %q", in, out)
		}
	}
	for _, in := range []string{
		"password = hash_password(raw)", "const passwordRef = useRef<HTMLInputElement>(null)",
		"password: Mapped[str]", "password = self.make_password(x)", "password = getPassword()",
		// qualified calls, and identifiers with acronyms and digits
		"'password' => Hash::make($request->password)", `let password = env::var("DB_PASSWORD")?`,
		"pwd = decodeB64(enc)", "password = makeMD5(x)", "password = SHA256Hasher(x)",
		"password = Digest::SHA256.hexdigest(raw)",
		// Ruby, negation, Razor, and names with a number in them
		"password = @options.fetch(:password)", "password = @user.password_digest[0]",
		"password = ::Rails.application.credentials.dig(:db, :password)", "password = !checkPassword(x)",
		"password = @Html.Password(x)", "password = md5Hex(pw)", "password = sha256Hex(pw)",
		"password = b64Decode(pw)", "password = x509Cert(raw)", "password = pbkdf2Sync(x)",
		"password = md5ToHex(x)", "password = utf8ToB64(x)", "password = IPv4ToIPv6(x)",
		"password = s3v4Sign(x)", "password = loadX509KeyPair(x)", "token = pbkdf2HmacSha256(x)",
		"secret = Rfc2898DeriveBytes(x)",
		// a compiler pass is not a password
		"pass1 = optimize_loops", "pass2: inline_functions",
	} {
		if out, st := redact(in); out != in || len(st) != 0 {
			t.Errorf("%q changed: %q", in, out)
		}
	}
}
