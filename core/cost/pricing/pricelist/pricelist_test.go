package pricelist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/pricing"
)

// priceMap is LiteLLM's shape in miniature: a non-model row, a first-party Anthropic model
// and the same model through Bedrock, which carries its own rates and must not be read.
const priceMap = `{
  "sample_spec": {"input_cost_per_token": 0},
  "claude-opus-5-5": {"litellm_provider": "anthropic", "input_cost_per_token": 4e-06,
    "cache_creation_input_token_cost": 5e-06, "cache_read_input_token_cost": 2e-07,
    "output_cost_per_token": 2e-05},
  "us.anthropic.claude-opus-5-5": {"litellm_provider": "bedrock_converse",
    "input_cost_per_token": 4.4e-06, "output_cost_per_token": 2.2e-05}
}`

var fetchTime = time.Date(2026, 10, 8, 21, 0, 0, 0, time.UTC)

// server serves body under etag and answers 304 to a request carrying it.
type server struct {
	mu    sync.Mutex
	body  string
	etag  string
	code  int
	calls int
	sent  []string // If-None-Match of each request
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.sent = append(s.sent, r.Header.Get("If-None-Match"))
	if s.code != 0 {
		w.WriteHeader(s.code)
		return
	}
	if s.etag != "" && r.Header.Get("If-None-Match") == s.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", s.etag)
	_, _ = w.Write([]byte(s.body))
}

func newFetcher(t *testing.T, s *server) (*Fetcher, string) {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	cache := filepath.Join(t.TempDir(), "price-list.json")
	return &Fetcher{URL: ts.URL, CacheFile: cache, Now: func() time.Time { return fetchTime }}, cache
}

func inputPerMillion(t *testing.T, l *pricing.List, model string) float64 {
	t.Helper()
	tab, err := pricing.BuildWithList(nil, l)
	if err != nil {
		t.Fatalf("BuildWithList: %v", err)
	}
	r, p := tab.Resolve("api.anthropic.com", model, 0)
	if p == pricing.ProvNone {
		t.Fatalf("%s is not priced by the downloaded list", model)
	}
	return r.Base[pricing.TierInput] * 1e6
}

func TestFetch_ReturnsTheAnthropicRates(t *testing.T) {
	f, _ := newFetcher(t, &server{body: priceMap, etag: `"v1"`})
	l, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if l == nil {
		t.Fatal("Fetch returned no list for a 200")
	}
	if got := inputPerMillion(t, l, "claude-opus-5-5"); got < 3.999 || got > 4.001 {
		t.Errorf("input rate = %v, want 4.00 from the first-party row, not Bedrock's 4.40", got)
	}
	if !l.FetchedAt.Equal(fetchTime) {
		t.Errorf("FetchedAt = %v, want %v", l.FetchedAt, fetchTime)
	}
}

func TestFetch_AnUnchangedListIsNotDownloadedAgain(t *testing.T) {
	s := &server{body: priceMap, etag: `"v1"`}
	f, _ := newFetcher(t, s)
	if _, err := f.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	l, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if l != nil {
		t.Error("second Fetch returned a list, want nil: the server said 304")
	}
	if s.sent[1] != `"v1"` {
		t.Errorf("second request sent If-None-Match %q, want the ETag from the first", s.sent[1])
	}
}

// An OpenAI-only list is usable, which is the other half of the case above: the
// allow-list carries two providers now, so neither one alone makes a list unusable.
// Without this, deleting openai from the allow-list would leave the suite green —
// the refusal case above would simply start passing for the wrong reason.
func TestFetch_AcceptsAnOpenAIOnlyList(t *testing.T) {
	s := &server{body: `{"gpt-9": {"litellm_provider": "openai", "input_cost_per_token": 1e-06}}`}
	f, _ := newFetcher(t, s)
	l, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch refused an openai-only list: %v", err)
	}
	if l == nil || len(l.Entries) == 0 {
		t.Fatalf("Fetch returned no entries for an openai-only list: %v", l)
	}
	if l.Entries[0].Model != "gpt-9" {
		t.Errorf("entry = %q, want gpt-9", l.Entries[0].Model)
	}
}

func TestFetch_RefusesAListItCannotUse(t *testing.T) {
	for name, s := range map[string]*server{
		"server error": {code: http.StatusInternalServerError},
		"not json":     {body: "<html>rate limited</html>"},
		// A row from a provider the bundled table does not carry. Was "no anthropic
		// row" with an openai body, which stopped being unusable when openai joined
		// the allow-list — vertex_ai is a real mirror we still exclude, so the case
		// keeps testing what it was for: a list with nothing we can price from.
		"no bundled provider": {body: `{"vertex_ai/claude-opus-9": {"litellm_provider": "vertex_ai", "input_cost_per_token": 4e-06}}`},
		"no rate":             {body: rateless},
		"renamed rate":        {body: `{"claude-opus-9": {"litellm_provider": "anthropic", "input_cost_per_token_v2": 4e-06}}`},
		"too large":           {body: `{"x": "` + strings.Repeat("a", maxBytes) + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f, cache := newFetcher(t, s)
			l, err := f.Fetch(context.Background())
			if err == nil {
				t.Fatalf("Fetch returned %v and no error", l)
			}
			if _, statErr := os.Stat(cache); !os.IsNotExist(statErr) {
				t.Errorf("a refused list was written to the cache (stat: %v)", statErr)
			}
		})
	}
}

// rateless is an Anthropic row that decodes but carries no rate the table reads.
const rateless = `{"claude-opus-9": {"litellm_provider": "anthropic", "input_cost_per_token": 0}}`

func TestFetch_AListWithNoRateLeavesTheSavedCopyAlone(t *testing.T) {
	s := &server{body: priceMap, etag: `"v1"`}
	f, cache := newFetcher(t, s)
	if _, err := f.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	s.mu.Lock()
	s.body, s.etag = rateless, `"v2"`
	s.mu.Unlock()
	if l, err := f.Fetch(context.Background()); err == nil {
		t.Fatalf("Fetch returned %v and no error for a list with no rate", l)
	}
	if _, err := f.Fetch(context.Background()); err == nil {
		t.Fatal("third Fetch returned no error")
	}
	if s.sent[2] != `"v1"` {
		t.Errorf("third request sent If-None-Match %q, want %q: the refused list's ETag was kept", s.sent[2], `"v1"`)
	}
	l, err := (&Fetcher{CacheFile: cache}).Cached()
	if err != nil || l == nil {
		t.Fatalf("Cached = %v, %v; want the first download", l, err)
	}
	if got := inputPerMillion(t, l, "claude-opus-5-5"); got < 3.999 || got > 4.001 {
		t.Errorf("cached input rate = %v, want 4.00 from the first download", got)
	}
}

func TestCached_ASavedListWithNoRateIsAnError(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "price-list.json")
	saved := `{"etag": "\"v1\"", "fetchedAt": "2026-10-08T21:00:00Z", "prices": ` + rateless + `}`
	if err := os.WriteFile(cache, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{body: priceMap, etag: `"v1"`}
	f, _ := newFetcher(t, s)
	f.CacheFile = cache
	if l, err := f.Cached(); err == nil {
		t.Fatalf("Cached = %v and no error for a saved list with no rate", l)
	}
	l, err := f.Fetch(context.Background())
	if err != nil || l == nil {
		t.Fatalf("Fetch = %v, %v; want a download: the refused copy's ETag must not answer 304", l, err)
	}
}

func TestCached_ARestartStartsFromTheLastDownload(t *testing.T) {
	s := &server{body: priceMap, etag: `"v1"`}
	f, cache := newFetcher(t, s)
	if _, err := f.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	restarted := &Fetcher{URL: f.URL, CacheFile: cache}
	l, err := restarted.Cached()
	if err != nil {
		t.Fatalf("Cached: %v", err)
	}
	if l == nil {
		t.Fatal("Cached returned no list after a successful download")
	}
	if got := inputPerMillion(t, l, "claude-opus-5-5"); got < 3.999 || got > 4.001 {
		t.Errorf("cached input rate = %v, want 4.00", got)
	}
	if !l.FetchedAt.Equal(fetchTime) {
		t.Errorf("cached FetchedAt = %v, want the original download's %v", l.FetchedAt, fetchTime)
	}
	// The saved ETag goes out with the first request, so a restart costs a 304, not a download.
	if l, err := restarted.Fetch(context.Background()); err != nil || l != nil {
		t.Errorf("Fetch after restart = %v, %v; want nil, nil (304)", l, err)
	}
}

func TestCached_NoFileIsNoList(t *testing.T) {
	f := &Fetcher{CacheFile: filepath.Join(t.TempDir(), "absent.json")}
	l, err := f.Cached()
	if l != nil || err != nil {
		t.Errorf("Cached = %v, %v; want nil, nil", l, err)
	}
}

func TestCached_ACorruptFileIsAnError(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "price-list.json")
	if err := os.WriteFile(cache, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Fetcher{CacheFile: cache}
	if l, err := f.Cached(); err == nil {
		t.Errorf("Cached = %v and no error for a corrupt file", l)
	}
}

func TestRun_AppliesTheCacheThenEachChange(t *testing.T) {
	s := &server{body: priceMap, etag: `"v1"`}
	f, cache := newFetcher(t, s)
	if _, err := f.Fetch(context.Background()); err != nil {
		t.Fatalf("seeding the cache: %v", err)
	}

	// Upstream changes while the proxy is down.
	s.mu.Lock()
	s.body = strings.Replace(priceMap, `"input_cost_per_token": 4e-06`, `"input_cost_per_token": 3e-06`, 1)
	s.etag = `"v2"`
	s.mu.Unlock()

	var mu sync.Mutex
	var applied []float64
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := &Fetcher{URL: f.URL, CacheFile: cache, Now: f.Now}
	go func() {
		restarted.Run(ctx, time.Hour, func(l *pricing.List) {
			mu.Lock()
			defer mu.Unlock()
			applied = append(applied, inputPerMillion(t, l, "claude-opus-5-5"))
			if len(applied) == 2 {
				close(done)
			}
		})
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if applied[0] < 3.999 || applied[0] > 4.001 || applied[1] < 2.999 || applied[1] > 3.001 {
		t.Errorf("applied input rates = %v, want [4 3]: the cached list first, then the change", applied)
	}
}
