package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/redact"
	"gopkg.in/yaml.v3"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestMetricsPushConfig_ResolveDefaults(t *testing.T) {
	got := (*MetricsPushConfig)(nil).Resolve(envOf(nil))
	if !got.IsEnabled() || got.URL != "" || got.Token != "" || got.Interval != 30*time.Second {
		t.Errorf("Resolve(nil) = %+v, want enabled, no URL, no token, 30s", got)
	}
	if got.On() {
		t.Error("On() with no URL, want false: nothing is sent without a URL")
	}
}

func TestMetricsPushConfig_ResolveEnvOverrides(t *testing.T) {
	off := false
	m := &MetricsPushConfig{Enabled: &off, URL: "https://a.example", Token: "file", Interval: time.Minute}
	got := m.Resolve(envOf(map[string]string{
		"METRICS_PUSH":          "true",
		"METRICS_PUSH_URL":      "https://b.example",
		"METRICS_PUSH_TOKEN":    "env",
		"METRICS_PUSH_INTERVAL": "10s",
	}))
	if !got.IsEnabled() || got.URL != "https://b.example" || got.Token != "env" || got.Interval != 10*time.Second {
		t.Errorf("Resolve = %+v, want the env values", got)
	}
	if !got.On() {
		t.Error("On() = false, want true")
	}
	// The receiver is not changed.
	if *m.Enabled || m.URL != "https://a.example" {
		t.Errorf("Resolve changed its receiver: %+v", m)
	}
	// An unreadable METRICS_PUSH turns the push off: nothing leaves the machine on a typo.
	if got := m.Resolve(envOf(map[string]string{"METRICS_PUSH": "yes please", "METRICS_PUSH_URL": "https://b.example"})); got.On() {
		t.Error("an unreadable METRICS_PUSH left the push on")
	}
	// An unreadable interval keeps the configured one.
	if got := m.Resolve(envOf(map[string]string{"METRICS_PUSH_INTERVAL": "soon"})); got.Interval != time.Minute {
		t.Errorf("interval = %v, want the configured 1m", got.Interval)
	}
}

func TestMetricsPushConfig_Validate(t *testing.T) {
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"", true},
		{"https://collector.example.com", true},
		{"https://collector.example.com/cortex", true},
		{"http://localhost:4318", true},
		{"http://127.0.0.1:4318/cortex", true},
		{"http://[::1]:4318", true},
		{"http://collector.example.com", false},
		{"http://10.0.0.5:4318", false},
		{"ftp://collector.example.com", false},
		{"collector.example.com", false},
	} {
		err := MetricsPushConfig{URL: c.url}.Validate()
		if (err == nil) != c.ok {
			t.Errorf("Validate(%q) = %v, want ok=%v", c.url, err, c.ok)
		}
	}
	if err := (MetricsPushConfig{Interval: -time.Second}).Validate(); err == nil {
		t.Error("a negative interval was accepted")
	}
}

func TestMetricsPushConfig_YAMLAndValidateRefusesHTTP(t *testing.T) {
	var c Config
	src := "metrics_push:\n  enabled: true\n  url: http://collector.example.com\n  token: t\n  interval: 15s\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	if c.MetricsPush == nil || c.MetricsPush.URL != "http://collector.example.com" || c.MetricsPush.Interval != 15*time.Second {
		t.Fatalf("decoded %+v", c.MetricsPush)
	}
	c.Mode = ModeProxySidecar
	ApplyPreset(&c)
	c.Listener.ReverseProxyBackend = "http://127.0.0.1:9"
	if err := Validate(&c); err == nil || !strings.Contains(err.Error(), "metrics_push.url") {
		t.Errorf("Validate = %v, want a metrics_push.url error", err)
	}
}

// The token is served redacted on /config, which marshals the Config as JSON.
func TestMetricsPushConfig_TokenIsRedactedOnConfig(t *testing.T) {
	raw, err := json.Marshal(&Config{MetricsPush: &MetricsPushConfig{URL: "https://c.example", Token: "s3cret-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(redact.JSON(raw)); strings.Contains(out, "s3cret-token") {
		t.Errorf("redacted /config still holds the token: %s", out)
	}
}

// A credential in the URL's user info or query never reaches an error: Validate quotes the URL
// only as SafeURL gives it.
func TestMetricsPushConfig_ErrorsNeverQuoteURLCredentials(t *testing.T) {
	for _, raw := range []string{"http://u:p@collector.example.com/x?token=t", "https://u:p@[bad/x?token=t"} {
		err := MetricsPushConfig{URL: raw}.Validate()
		if err == nil {
			t.Fatalf("Validate(%q) accepted it", raw)
		}
		for _, secret := range []string{"u:p", "token=t", raw} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Validate(%q) = %q, quotes %q", raw, err, secret)
			}
		}
	}
	if got := SafeURL("https://u:p@host/x?token=t#frag"); got != "https://host/x" {
		t.Errorf("SafeURL = %q, want https://host/x", got)
	}
}
