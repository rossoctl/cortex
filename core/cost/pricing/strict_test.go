package pricing

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStrict_RejectsTyposThatWouldSilentlyRepriceEverything(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{{
		// The key is `hosts` (plural, a list). A near-miss spelling would leave it
		// empty, which means ANY endpoint — and a configured entry outranks anything
		// bundled, so rates meant for one gateway would silently reprice the whole
		// process off one stray letter.
		name: "host instead of hosts",
		src: `
endpoints:
  - host: gw.internal
    models:
      "*": {input_cost_per_million: 3.80}
`,
		want: "host",
	}, {
		// Builds fine because another tier is set, then Cost refuses every request
		// carrying input tokens and the endpoint drops out of the total entirely.
		name: "misspelled tier",
		src: `
endpoints:
  - hosts: [gw.internal]
    models:
      "*":
        input_cost_per_milion: 3.80
        output_cost_per_million: 19.00
`,
		want: "input_cost_per_milion",
	}, {
		name: "unknown top-level key",
		src:  "bundle: false\n",
		want: "bundle",
	}, {
		name: "unknown threshold key",
		src: `
endpoints:
  - hosts: ["*"]
    models:
      "*":
        input_cost_per_million: 3.0
        above:
          - prompt_token: 200000
            input_cost_per_million: 6.0
`,
		want: "prompt_token",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			err := yaml.Unmarshal([]byte(tc.src), &c)
			if err == nil {
				t.Fatalf("accepted a typo that would silently misprice: %s", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending key %q", err, tc.want)
			}
		})
	}
}

func TestStrict_AcceptsEveryDocumentedField(t *testing.T) {
	// Guards the reflection: a field renamed in Go but not here would start being
	// rejected, and this is what says so rather than an operator's config failing.
	src := `
bundled: false
endpoints:
  - hosts: ["gw.internal"]
    models:
      "*claude-opus-*":
        input_cost_per_million: 3.80
        cache_write_cost_per_million: 4.75
        cache_read_cost_per_million: 0.38
        output_cost_per_million: 19.00
        input_cost_per_token: 0
        cache_write_cost_per_token: 0
        cache_read_cost_per_token: 0
        output_cost_per_token: 0
        above:
          - prompt_tokens: 200000
            input_cost_per_million: 7.60
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("rejected a fully-documented config: %v", err)
	}
	if len(c.Endpoints) != 1 || len(c.Endpoints[0].Hosts) != 1 || c.Endpoints[0].Hosts[0] != "gw.internal" {
		t.Fatalf("decode lost data: %+v", c)
	}
	if len(c.Endpoints[0].Models["*claude-opus-*"].Above) != 1 {
		t.Error("threshold did not decode")
	}
}
