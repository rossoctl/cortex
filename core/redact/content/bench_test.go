package content

import (
	"strings"
	"testing"
	"time"
)

// LINEAR TIME. Each input repeats one rule's trigger, or a near miss of its pattern, about a
// megabyte's worth; a rule that rescans from every trigger would take minutes here. The bound
// is generous, for a slow and shared CI machine: it catches a quadratic rule, not a slow one.
func TestLinearOnAdversarialInput(t *testing.T) {
	const size = 1 << 20
	rep := func(s string) string { return strings.Repeat(s, size/len(s)) }
	for name, in := range map[string]string{
		"scheme separators":   rep("://"),
		"user info no at":     rep("a://b:c"),
		"long user info":      "x://" + rep("u") + ":" + rep("p"),
		"keyword assignments": rep("password="),
		"keyword words":       rep("pass"),
		"keyword long value":  "password=" + rep("a="),
		"key headers":         rep("-----" + "BEGIN RSA " + "PRIVATE KEY-----"),
		"key headers and end": rep("-----"+"BEGIN RSA "+"PRIVATE KEY-----") + "-----END RSA PRIVATE KEY-----",
		"key footers":         rep("-----END "),
		"bearers":             rep("Bearer "),
		"authorizations":      rep("authorization:"),
		"jwt starts":          rep("eyJ"),
		"provider prefixes":   rep("sk-ghp_AKIAxox"),
		"one base64 run":      rep("A"),
		"base64 lines":        rep(strings.Repeat("Q", 70) + "\n"),
		"curl flags":          rep("curl -u "),
		"docker logins":       rep("docker login "),
		"mysql commands":      rep("mysql -u "),
		"command prose":       rep("run curl with care, then docker login and mysql later "),
		"password flags":      rep("--password --token "),
		"rejected values":     rep("auth_url=https://x.example.io/a?b=c "),
	} {
		start := time.Now()
		redact(in)
		d := time.Since(start)
		t.Logf("%s: %v", name, d)
		if d > 3*time.Second {
			t.Errorf("%s: %v for %d bytes: not linear", name, d, len(in))
		}
	}
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
		parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`))
	}
	b.WriteString(`{"messages":[`)
	for i := 0; b.Len() < n; i++ {
		b.WriteString(`{"role":"user","content":"`)
		b.WriteString(parts[i%len(parts)])
		b.WriteString(`"},`)
	}
	b.WriteString(`{"role":"user","content":"end"}]}`)
	return b.String()
}

func BenchmarkText1MB(b *testing.B)        { benchText(b, textBody(1<<20)) }
func BenchmarkText10MB(b *testing.B)       { benchText(b, textBody(10<<20)) }
func BenchmarkImage10MB(b *testing.B)      { benchText(b, randomImageJSON(10<<20)) }
func BenchmarkImage40MB(b *testing.B)      { benchText(b, randomImageJSON(40<<20)) }
func BenchmarkImageLines10MB(b *testing.B) { benchText(b, randomImageJSON(10<<20, 76)) }
