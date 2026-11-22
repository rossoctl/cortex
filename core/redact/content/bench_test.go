package content

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// LINEAR TIME. Each input repeats one rule's trigger, or a near miss of its pattern. A rule that
// rescans from every trigger is quadratic on such an input. The test does not use an absolute
// bound, which a slow or loaded CI machine (or -race) breaks: it times each input at n and at
// 4n bytes on the same machine and requires the larger to take less than 10 times as long. A
// linear rule takes about 4 times; a quadratic one, 16.
func TestLinearOnAdversarialInput(t *testing.T) {
	const n = 64 << 10
	rep := func(s string, size int) string { return strings.Repeat(s, size/len(s)) }
	r := func(s string) func(int) string { return func(size int) string { return rep(s, size) } }
	for name, in := range map[string]func(int) string{
		"scheme separators":   r("://"),
		"user info no at":     r("a://b:c"),
		"long user info":      func(size int) string { return "x://" + rep("u", size/2) + ":" + rep("p", size/2) },
		"keyword assignments": r("password="),
		"keyword words":       r("pass"),
		"keyword long value":  func(size int) string { return "password=" + rep("a=", size) },
		"quoted values":       r(`password="`),
		"cookies":             r("Cookie: a=b; "),
		"cookie lists":        r("Cookie: " + strings.Repeat("a=b;", 1022) + "\n"),
		"armor header quotes": func(size int) string {
			return "-----" + "BEGIN PGP " + "PRIVATE KEY BLOCK-----\n" + rep(`Comment: \"x\" \"y\"`+"\n", size) + "-----END PGP PRIVATE KEY BLOCK-----"
		},
		"key headers": r("-----" + "BEGIN RSA " + "PRIVATE KEY-----"),
		"key headers and end": func(size int) string {
			return rep("-----"+"BEGIN RSA "+"PRIVATE KEY-----", size) + "-----END RSA PRIVATE KEY-----"
		},
		"key footers": r("-----END "),
		"armor headers": func(size int) string {
			return "-----" + "BEGIN PGP " + "PRIVATE KEY BLOCK-----\n" + rep("Comment: <x>\n", size)
		},
		"bearers":           r("Bearer "),
		"authorizations":    r("authorization:"),
		"digests":           r("Digest x=1, "),
		"jwt starts":        r("eyJ"),
		"provider prefixes": r("sk-ghp_AKIAxox"),
		"one base64 run":    r("A"),
		"base64 lines":      r(strings.Repeat("Q", 70) + "\n"),
		"curl flags":        r("curl -u "),
		"docker logins":     r("docker login "),
		"mysql commands":    r("mysql -u "),
		"command prose":     r("run curl with care, then docker login and mysql later "),
		"password flags":    r("--password --token "),
		"rejected values":   r("auth_url=https://x.example.io/a?b=c "),
		"rejected URLs": func(size int) string {
			return "auth_url=https://x.example.io/" + rep("a", size) + "?" + rep("password=", size)
		},
	} {
		small, large := bestOf(2, func() { redact(in(n)) }), bestOf(2, func() { redact(in(4 * n)) })
		ratio := float64(large) / float64(max(small, time.Microsecond))
		t.Logf("%s: %v at %d bytes, %v at %d (x%.1f)", name, small, n, large, 4*n, ratio)
		if ratio > 10 && large > 50*time.Millisecond {
			t.Errorf("%s: %v at %d bytes but %v at %d, x%.1f for 4 times the input: not linear", name, small, n, large, 4*n, ratio)
		}
	}
}

// bestOf runs f k times and returns its shortest time: the least disturbed by the machine.
func bestOf(k int, f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for range k {
		start := time.Now()
		f()
		best = min(best, time.Since(start))
	}
	return best
}

// Benchmarks: raw bodies redacted whole, as text. Each reports its throughput (MB/s).

func benchText(b *testing.B, in string) {
	r := New()
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.String(in)
	}
}

// textBody is a request body of about n bytes of prose, code and tool JSON, with no secret.
func textBody(n int) string {
	var b strings.Builder
	parts := []string{}
	for _, s := range falsePositiveCorpus() {
		enc, _ := json.Marshal(s) // quoted and escaped, control characters included
		parts = append(parts, string(enc))
	}
	b.WriteString(`{"messages":[`)
	for i := 0; b.Len() < n; i++ {
		b.WriteString(`{"role":"user","content":`)
		b.WriteString(parts[i%len(parts)])
		b.WriteString(`},`)
	}
	b.WriteString(`{"role":"user","content":"end"}]}`)
	return b.String()
}

// textBodyChecked is textBody, checked to be valid JSON before it is timed.
func textBodyChecked(b *testing.B, n int) string {
	body := textBody(n)
	if !json.Valid([]byte(body)) {
		b.Fatal("the benchmark body is not valid JSON")
	}
	return body
}

func BenchmarkText1MB(b *testing.B)        { benchText(b, textBodyChecked(b, 1<<20)) }
func BenchmarkText10MB(b *testing.B)       { benchText(b, textBodyChecked(b, 10<<20)) }
func BenchmarkImage10MB(b *testing.B)      { benchText(b, randomImageJSON(10<<20)) }
func BenchmarkImage40MB(b *testing.B)      { benchText(b, randomImageJSON(40<<20)) }
func BenchmarkImageLines10MB(b *testing.B) { benchText(b, randomImageJSON(10<<20, 76)) }
