// Command gen regenerates the bundled price table from LiteLLM's public price map.
//
// Usage:
//
//	make pricing-table COMMIT=<litellm commit sha>
//
// The commit is required rather than defaulting to main: the whole point of the
// pin is that the table records which upstream state it came from, and a table
// generated from "whatever main was that day" cannot be diffed against anything.
//
// Writes two files, both committed:
//
//   - pricing/bundled.go, the generated table.
//   - pricing/testdata/model_prices.snapshot.json, the Anthropic-provider subset
//     of the upstream map. The golden test regenerates from this snapshot and
//     compares, so it proves the checked-in table matches its source without
//     needing a network — and catches a hand-edit to either file.
//
// Network note: github.com is behind a TLS-intercepting local proxy in this
// environment, so the Makefile target clears HTTPS_PROXY / HTTP_PROXY / ALL_PROXY.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/rossoctl/cortex/core/pricing/internal/pricegen"
)

const priceMapURL = "https://raw.githubusercontent.com/BerriAI/litellm/%s/model_prices_and_context_window.json"

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func main() {
	commit := flag.String("commit", "", "BerriAI/litellm commit sha (40 hex chars) to generate from")
	outDir := flag.String("dir", ".", "the pricing package directory")
	flag.Parse()

	if !shaRE.MatchString(*commit) {
		log.Fatalf("gen: -commit must be a full 40-character sha, got %q\n"+
			"Find one with:\n"+
			"  curl -sS 'https://api.github.com/repos/BerriAI/litellm/commits?path=model_prices_and_context_window.json&per_page=1'",
			*commit)
	}

	raw, err := fetch(fmt.Sprintf(priceMapURL, *commit))
	if err != nil {
		log.Fatalf("gen: %v", err)
	}

	snapshot, err := pricegen.Filter(raw, *commit)
	if err != nil {
		log.Fatalf("gen: %v", err)
	}
	entries, err := pricegen.Entries(snapshot)
	if err != nil {
		log.Fatalf("gen: %v", err)
	}
	src, err := pricegen.Render(entries, *commit)
	if err != nil {
		log.Fatalf("gen: %v", err)
	}

	snapPath := filepath.Join(*outDir, "testdata", "model_prices.snapshot.json")
	if err := os.WriteFile(snapPath, append(snapshot, '\n'), 0o644); err != nil {
		log.Fatalf("gen: write snapshot: %v", err)
	}
	tablePath := filepath.Join(*outDir, "bundled.go")
	if err := os.WriteFile(tablePath, src, 0o644); err != nil {
		log.Fatalf("gen: write table: %v", err)
	}
	fmt.Printf("gen: %d entries from litellm %s\n  %s\n  %s\n",
		len(entries), (*commit)[:12], tablePath, snapPath)
}

func fetch(url string) ([]byte, error) {
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d (is the commit sha real?)", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
