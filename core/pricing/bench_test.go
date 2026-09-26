package pricing

import "testing"

func BenchmarkResolve_BundledTable(b *testing.B) {
	tab, err := NewTable(Bundled())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, p := tab.Resolve("api.anthropic.com:443", "claude-opus-4-1", 250_000); p == ProvNone {
			b.Fatal("unpriced")
		}
	}
}

func BenchmarkResolve_Miss(b *testing.B) {
	tab, err := NewTable(Bundled())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tab.Resolve("api.openai.com", "gpt-5", 1000)
	}
}

func BenchmarkRegistryCost_BundledTable(b *testing.B) {
	tab, err := NewTable(Bundled())
	if err != nil {
		b.Fatal(err)
	}
	reg := NewRegistry(tab)
	u := Usage{Input: 1000, CacheWrite: 2000, CacheRead: 200_000, Output: 500}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := reg.Cost("api.anthropic.com:443", "claude-opus-5", u); !ok {
			b.Fatal("unpriced")
		}
	}
}
