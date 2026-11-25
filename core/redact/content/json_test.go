package content

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// pngHeader is the 8-byte signature every PNG file starts with.
var pngHeader = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

func redactJSON(s string) (string, JSONStats) {
	out, st := New().JSON([]byte(s))
	return string(out), st
}

// A body with no secret comes back byte for byte, as the same slice: key order, whitespace,
// number forms and escapes ("<" as Go writes it, "\/", a surrogate pair) are kept.
func TestJSON_UnchangedIsByteIdentical(t *testing.T) {
	lt := string([]byte{92, 'u', '0', '0', '3', 'c'})
	for _, in := range []string{
		`{}`, `[]`, `"plain"`, `1.5e+10`, `null`,
		`{"z":1,"a":[true,false,null],"m":{"k":"v"}}`,
		"{\n  \"model\": \"x\",\n  \"max_tokens\": 1024,\n  \"temperature\": 0.70\n}",
		`{"html":"` + lt + `b>bold` + lt + `/b>","path":"a\/b","emoji":"\ud83d\ude00","tab":"a\tb"}`,
		`{"messages":[{"role":"user","content":"what is 2+2?"}]}`,
	} {
		b := []byte(in)
		out, st := New().JSON(b)
		if !bytes.Equal(out, b) || len(st.Redacted) != 0 || st.Text {
			t.Errorf("%q changed: %q (%+v)", in, out, st)
		}
		if len(out) > 0 && &out[0] != &b[0] {
			t.Errorf("%q: a copy was returned", in)
		}
	}
}

// A secret in a string value is replaced; the rest of the body is byte for byte the same, and
// the output is valid JSON.
func TestJSON_SecretInAValue(t *testing.T) {
	key := fakes()["anthropic"]
	in := `{"a":1, "text":"my key is ` + key + ` ok",  "z":[ 2 ]}`
	out, st := redactJSON(in)
	want := `{"a":1, "text":"my key is [REDACTED:api_key] ok",  "z":[ 2 ]}`
	if out != want || st.Redacted[catAPIKey] != 1 {
		t.Errorf("out = %q, stats %+v", out, st)
	}
	mustBeJSON(t, "the output", out)
}

// The key a value sits under is its context: a secret-named key makes the value a candidate.
// Keys themselves are not redacted.
func TestJSON_KeyContext(t *testing.T) {
	pw := fakePassword() + "x"
	for _, tc := range []struct{ in, want string }{
		{`{"password":"` + pw + `"}`, `{"password":"[REDACTED:password]"}`},
		{`{"db":{"password":"` + pw + `","user":"bob"}}`, `{"db":{"password":"[REDACTED:password]","user":"bob"}}`},
		{`{"password":["` + pw + `","` + pw + `"]}`, `{"password":["[REDACTED:password]","[REDACTED:password]"]}`},
		{`{"passwords":["` + pw + `"]}`, `{"passwords":["[REDACTED:password]"]}`},
		{`{"comment":"` + pw + `"}`, `{"comment":"` + pw + `"}`},
		{`{"` + fakes()["anthropic"] + `":1}`, `{"` + fakes()["anthropic"] + `":1}`},
		// a number under a secret name, as text would be; it becomes a string
		{`{"password":12345678}`, `{"password":"[REDACTED:password]"}`},
		{`{"count":12345678,"pin":-1.5}`, `{"count":12345678,"pin":-1.5}`},
		// JSON in a string takes the string's key for its top-level values (an array's elements),
		// as plain JSON does; an object's own keys take over inside it
		{`{"password":"[\"` + pw + `\"]"}`, `{"password":"[\"[REDACTED:password]\"]"}`},
		{`{"password":"{\"note\":\"` + pw + `\"}"}`, `{"password":"{\"note\":\"` + pw + `\"}"}`},
	} {
		out, _ := redactJSON(tc.in)
		if out != tc.want {
			t.Errorf("%q: out = %q, want %q", tc.in, out, tc.want)
		}
		mustBeJSON(t, "the output", out)
	}
}

// A changed string is decoded, redacted and encoded again: escapes in it are read as what they
// stand for, so a secret written with escapes is found, and the output decodes to the redacted
// text.
func TestJSON_EscapesInAChangedString(t *testing.T) {
	key := fakes()["github"]
	// "\u0067hp_..." is "ghp_...": the escape hides the prefix from a raw scan.
	in := `{"t":"say \"hi\"\n` + `\u0067` + key[1:] + ` \u00e9"}`
	out, st := redactJSON(in)
	mustBeJSON(t, "the output", out)
	var v struct{ T string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.T != "say \"hi\"\n[REDACTED:api_key] é" || st.Redacted[catAPIKey] != 1 {
		t.Errorf("out = %q (decoded %q, %v), stats %+v", out, v.T, err, st)
	}
}

// JSON in a JSON string (tool arguments, tool results) is scanned as JSON, so its keys are
// context too, and both levels stay valid.
func TestJSON_JSONInJSON(t *testing.T) {
	pw := fakePassword() + "x"
	inner := `{"password":"` + pw + `","n":1}`
	in := `{"type":"tool_use","input_json":` + encodeJSONString(inner) + `}`
	out, st := redactJSON(in)
	mustBeJSON(t, "the output", out)
	var v struct {
		InputJSON string `json:"input_json"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.InputJSON != `{"password":"[REDACTED:password]","n":1}` || st.Redacted[catPassword] != 1 {
		t.Errorf("out = %q, stats %+v", out, st)
	}
	// Five levels deep: the fifth is past maxJSONDepth, so it is redacted as text; a secret
	// in it is still found by its shape.
	key := fakes()["anthropic"]
	doc := `{"k":"` + key + `"}`
	for range 5 {
		doc = `{"v":` + encodeJSONString(doc) + `}`
	}
	out, _ = redactJSON(doc)
	mustBeJSON(t, "the output", out)
	if strings.Contains(out, key[10:]) {
		t.Errorf("leaked five levels deep: %q", out)
	}
}

// Images, audio and documents are skipped unread, by where they sit, and counted. The text
// around them is still redacted.
func TestJSON_BinarySkips(t *testing.T) {
	key := fakes()["anthropic"]
	img := randomImageJSON(256 << 10)
	data := img[strings.Index(img, `"data":"`)+8 : strings.Index(img, `"}}`)]
	png := base64.StdEncoding.EncodeToString(append(append([]byte{}, pngHeader...), make([]byte, 70<<10)...))
	for name, tc := range map[string]struct {
		in      string
		skipped int
	}{
		"anthropic image":             {strings.Replace(img, "what is in this image?", key, 1), 1},
		"source data before its type": {`{"source":{"data":"` + data + `","media_type":"image/png","type":"base64"},"t":"` + key + `"}`, 1},
		"openai image_url":            {`{"image_url":{"url":"data:image/jpeg;base64,` + data + `"},"t":"` + key + `"}`, 1},
		"responses file_data":         {`{"type":"input_file","file_data":"data:application/pdf;base64,` + data + `","t":"` + key + `"}`, 1},
		"input_audio":                 {`{"input_audio":{"data":"` + data + `","format":"wav"},"t":"` + key + `"}`, 1},
		"png anywhere, long":          {`{"blob":"` + png + `","t":"` + key + `"}`, 1},
	} {
		out, st := redactJSON(tc.in)
		mustBeJSON(t, name, out)
		if st.SkippedBinary != tc.skipped || st.SkippedBinaryBytes < 64<<10 {
			t.Errorf("%s: skipped %d (%d bytes), want %d", name, st.SkippedBinary, st.SkippedBinaryBytes, tc.skipped)
		}
		if strings.Contains(out, key) || st.Redacted[catAPIKey] != 1 {
			t.Errorf("%s: the text's key was not redacted (stats %+v)", name, st)
		}
	}
}

// What only looks like binary is read: a skip needs both the place and content that is all base64
// of a known binary format. So a text document's source data, a text data URI, text that starts
// with a data URI, text where an image would sit (in a tool's arguments, in JSON in JSON, behind a
// duplicate "type"), and a long base64 string of no known format are all read.
func TestJSON_TextIsNotSkipped(t *testing.T) {
	key := fakes()["anthropic"]
	img := randomImageJSON(1 << 10)
	uri := "data:image/png;base64," + img[strings.Index(img, `"data":"`)+8:strings.Index(img, `"}}`)]
	for name, in := range map[string]string{
		"text document":          `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"cfg: ` + key + `"}}`,
		"text data URI":          `{"url":"data:text/plain;base64,` + base64.StdEncoding.EncodeToString([]byte("k="+key)) + `"}`,
		"text after a data URI":  `{"type":"tool_result","content":"` + uri + `\n\nANTHROPIC_API_KEY=` + key + `"}`,
		"a data URI's prefix":    `{"text":"data:image/png;base64,AAAA then my key ` + key + `"}`,
		"a bad media type":       `{"text":"data:image/x ` + key + ` ;base64,AAAA"}`,
		"other data":             `{"result":{"data":"` + key + `"}}`,
		"base64 source, text":    `{"source":{"type":"base64","data":"key ` + key + `"}}`,
		"text, then base64 type": `{"source":{"data":"key ` + key + `","type":"base64"}}`,
		"in a tool's arguments":  `{"type":"tool_use","input":{"source":{"type":"base64","data":"` + key + `"}}}`,
		"in JSON in JSON":        `{"content":` + encodeJSONString(`{"source":{"type":"base64","data":"`+key+`"}}`) + `}`,
		"a duplicate type":       `{"source":{"type":"text","data":"` + key + `","type":"base64"}}`,
		"input_audio, text":      `{"input_audio":{"data":"AWS key ` + fakes()["aws"] + `","format":"wav"}}`,
		"base64 of text":         `{"blob":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 70<<10)+" "+key)) + `"}`,
	} {
		out, st := redactJSON(in)
		mustBeJSON(t, name, out)
		if st.SkippedBinary != 0 {
			t.Errorf("%s: skipped", name)
		}
		if name == "base64 of text" {
			continue // the key is only in the base64: nothing to see in the output
		}
		if out == in || strings.Contains(out, key) || strings.Contains(out, fakes()["aws"]) {
			t.Errorf("%s: not redacted: %q", name, out)
		}
	}
}

// A skip decodes its whole string: a binary format's header in front of base64 text does not
// make the text binary. MIME-wrapped base64 ("\n" every 76 characters) is still skipped, and the
// skipped bytes are counted exactly.
func TestJSON_BinarySkipReadsTheWholeString(t *testing.T) {
	key := fakes()["anthropic"]
	hdr := base64.StdEncoding.EncodeToString(append(append([]byte{}, pngHeader...), 1, 2, 3, 4)) // 16 characters, no padding
	fake := hdr + base64.StdEncoding.EncodeToString([]byte("config: ANTHROPIC_API_KEY="+key+" end of config"))
	for name, in := range map[string]string{
		"source data":    `{"source":{"type":"base64","media_type":"image/png","data":"` + fake + `"}}`,
		"data URI":       `{"url":"data:image/png;base64,` + fake + `"}`,
		"input_audio":    `{"input_audio":{"data":"` + fake + `"}}`,
		"padding inside": `{"source":{"type":"base64","data":"` + hdr[:12] + "AA==" + hdr + `"}}`,
		"non-English text": `{"source":{"type":"base64","data":"` + hdr +
			base64.StdEncoding.EncodeToString([]byte(strings.Repeat("שלום עולם, ", 20)+"מפתח: "+key)) + `"}}`,
	} {
		out, st := redactJSON(in)
		mustBeJSON(t, name, out)
		if st.SkippedBinary != 0 {
			t.Errorf("%s: skipped", name)
		}
		if name != "padding inside" && out == in {
			t.Errorf("%s: the base64 text was not read: %q", name, out)
		}
	}
	img := randomImageJSON(256<<10, 76)
	data := img[strings.Index(img, `"data":"`)+8 : strings.Index(img, `"}}`)]
	out, st := redactJSON(img)
	if out != img || st.SkippedBinary != 1 || st.SkippedBinaryBytes != len(data) {
		t.Errorf("MIME-wrapped image: skipped %d (%d bytes, want %d)", st.SkippedBinary, st.SkippedBinaryBytes, len(data))
	}
}

// Numbers under a secret name: one as long as a secret under it can be is checked, in any form;
// a shorter one, or one under another name, is kept as it is.
func TestJSON_Numbers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"password":-1.5e+10}`, `{"password":"[REDACTED:password]"}`},
		{`{"password": 12345678 }`, `{"password": "[REDACTED:password]" }`},
		{`{"password":[12345678,1]}`, `{"password":["[REDACTED:password]",1]}`},
		{`{"password":12345}`, `{"password":12345}`},
		{`{"prompt_token_ids":[31337,42,1234567]}`, `{"prompt_token_ids":[31337,42,1234567]}`},
		{`{"count":123456789012}`, `{"count":123456789012}`},
		{`12345678`, `12345678`},
	} {
		out, _ := redactJSON(tc.in)
		if out != tc.want {
			t.Errorf("%q: out = %q, want %q", tc.in, out, tc.want)
		}
		mustBeJSON(t, "the output", out)
	}
}

// A body that is not JSON is redacted as text.
func TestJSON_NotJSONIsText(t *testing.T) {
	key := fakes()["anthropic"]
	for _, in := range []string{
		"<html><body>error: " + key + "</body></html>",
		`{"truncated":"` + key,
		"key=" + key,
	} {
		out, st := redactJSON(in)
		if !st.Text || strings.Contains(out, key) {
			t.Errorf("%q: out = %q, %+v", in, out, st)
		}
	}
}

// Property: for random documents with planted secrets, the output is valid JSON with the same
// structure, every string decodes to Find applied to the input's string, and no planted secret
// survives.
func TestJSON_RandomDocuments(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	secrets := []string{fakes()["anthropic"], fakes()["github"], fakes()["aws"], fakeJWT(), pemBlock("RSA")}
	pw := fakePassword() + "x"
	words := []string{"hello", "a\"q", "tab\there", "<b>", "é", "line\nbreak", "back\\slash", "x"}
	var gen func(d int) any
	gen = func(d int) any {
		switch n := rng.IntN(8); {
		case d > 3 || n < 3:
			s := words[rng.IntN(len(words))]
			if rng.IntN(4) == 0 {
				s += " " + secrets[rng.IntN(len(secrets))] + " " + words[rng.IntN(len(words))]
			}
			return s
		case n == 3:
			return rng.Float64()
		case n < 6:
			a := make([]any, rng.IntN(4))
			for i := range a {
				a[i] = gen(d + 1)
			}
			return a
		default:
			m := map[string]any{}
			for range rng.IntN(4) {
				m[words[rng.IntN(len(words))]] = gen(d + 1)
			}
			if rng.IntN(5) == 0 {
				m["password"] = pw
			}
			return m
		}
	}
	redacted := 0
	for range 2000 {
		v := gen(0)
		in, _ := json.Marshal(v)
		if rng.IntN(2) == 0 {
			in, _ = json.MarshalIndent(v, "", "  ")
		}
		out, st := New().JSON(in)
		for _, n := range st.Redacted {
			redacted += n
		}
		if !json.Valid(out) {
			t.Fatalf("invalid output for %s:\n%s", in, out)
		}
		for _, s := range append(secrets, pw) {
			if bytes.Contains(out, []byte(s)) {
				t.Fatalf("leaked %q:\n in: %s\nout: %s", s, in, out)
			}
		}
		var got any
		if err := json.Unmarshal(out, &got); err != nil || !sameShape(v, got, "") {
			t.Fatalf("structure or strings differ:\n in: %s\nout: %s", in, out)
		}
	}
	if t.Logf("%d redactions in 2000 documents", redacted); redacted < 200 {
		t.Errorf("only %d redactions: the generator plants too few secrets to test anything", redacted)
	}
}

// JSON redacts values only (the open question in the PR). With secrets planted in values AND
// keys, every value secret that Bytes (the raw body as text) finds, JSON finds too; the secrets
// Bytes finds that JSON keeps are keys, counted in the log. Values and keys use different
// secrets, so a survivor tells where it was.
func TestJSON_ValuesOnlyAgainstBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 17))
	valueSecrets := []string{fakes()["anthropic"], fakes()["aws"], fakeJWT()}
	keySecret := fakes()["github"]
	words := []string{"hello", "a\"q", "tab\there", "<b>", "é", "line\nbreak", "x"}
	var gen func(d int) any
	gen = func(d int) any {
		switch n := rng.IntN(8); {
		case d > 3 || n < 3:
			s := words[rng.IntN(len(words))]
			if rng.IntN(3) == 0 {
				s += " " + valueSecrets[rng.IntN(len(valueSecrets))] + " " + words[rng.IntN(len(words))]
			}
			return s
		case n < 6:
			a := make([]any, rng.IntN(4))
			for i := range a {
				a[i] = gen(d + 1)
			}
			return a
		default:
			m := map[string]any{}
			for range rng.IntN(4) {
				k := words[rng.IntN(len(words))]
				if rng.IntN(4) == 0 {
					k = keySecret
				}
				m[k] = gen(d + 1)
			}
			return m
		}
	}
	keyMisses, valueFinds := 0, 0
	for range 5000 {
		in, _ := json.Marshal(gen(0))
		viaJSON, _ := New().JSON(in)
		viaBytes, _ := New().Bytes(in)
		for _, s := range valueSecrets {
			if bytes.Contains(in, []byte(s)) && !bytes.Contains(viaBytes, []byte(s)) {
				valueFinds++
				if bytes.Contains(viaJSON, []byte(s)) {
					t.Fatalf("Bytes finds a value secret that JSON keeps:\n  in: %s\nJSON: %s", in, viaJSON)
				}
			}
		}
		if bytes.Contains(viaJSON, []byte(keySecret)) && !bytes.Contains(viaBytes, []byte(keySecret)) {
			keyMisses++
		}
	}
	t.Logf("5000 documents: %d with a value secret Bytes finds (JSON finds every one); %d with a key secret JSON keeps and Bytes finds", valueFinds, keyMisses)
	if valueFinds < 500 || keyMisses == 0 {
		t.Errorf("the generator no longer reaches both cases: %d value finds, %d key misses", valueFinds, keyMisses)
	}
}

// sameShape reports whether b has a's structure, and each string of b is a's string redacted by
// Find with its key.
func sameShape(a, b any, key string) bool {
	switch a := a.(type) {
	case string:
		s, ok := b.(string)
		return ok && s == Apply(a, New().Find(a, Context{Key: key}))
	case []any:
		bb, ok := b.([]any)
		if !ok || len(a) != len(bb) {
			return false
		}
		for i := range a {
			if !sameShape(a[i], bb[i], key) {
				return false
			}
		}
		return true
	case map[string]any:
		bb, ok := b.(map[string]any)
		if !ok || len(a) != len(bb) {
			return false
		}
		for k, v := range a {
			if !sameShape(v, bb[k], k) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// An image-heavy request costs time in proportion to its size: its base64 is skipped unread. The
// test compares 10 MB with 40 MB, a ratio, not a wall-clock bound, so the race detector and a
// slow runner slow both alike. BenchmarkJSONImage40MB measures the speed itself.
func TestJSON_ImageBodyIsLinear(t *testing.T) {
	small, large := []byte(randomImageJSON(10<<20)), []byte(randomImageJSON(40<<20))
	ds, dl := bestOf(2, func() { New().JSON(small) }), bestOf(2, func() { New().JSON(large) })
	// timing: compares the cost at two sizes; only their ratio is asserted
	ratio := float64(dl) / float64(max(ds, time.Microsecond))
	t.Logf("image body: %v at 10 MB, %v at 40 MB (x%.1f)", ds, dl, ratio)
	if ratio > 10 && dl > 50*time.Millisecond {
		t.Errorf("image body: %v at 10 MB but %v at 40 MB, x%.1f for 4 times the input: not linear", ds, dl, ratio)
	}
}

func BenchmarkJSONImage40MB(b *testing.B) { benchJSON(b, []byte(randomImageJSON(40<<20))) }

func BenchmarkJSONText10MB(b *testing.B) {
	in := []byte(textBodyChecked(b, 10<<20)) // a request body: many short messages
	benchJSON(b, in)
}

func benchJSON(b *testing.B, in []byte) {
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for range b.N {
		New().JSON(in)
	}
}
