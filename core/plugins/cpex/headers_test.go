package cpex

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsSensitive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Authorization deliberately passes through (jwt resolvers need it).
		{"Authorization", false},
		{"authorization", false},
		{"Content-Type", false},
		{"X-Request-Id", false},
		// Stripped (case-insensitive, exact + prefix).
		{"Cookie", true},
		{"cookie", true},
		{"Set-Cookie", true},
		{"X-Api-Key", true},
		{"x-api-key", true},
		{"X-CSRF-Token", true},
		{"x-csrf-token", true},
		{"Proxy-Authorization", true},
		{"X-Amz-Security-Token-Extra", true}, // prefix match
	}
	for _, tc := range cases {
		if got := isSensitive(tc.name); got != tc.want {
			t.Errorf("isSensitive(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFlattenHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"Bearer abc"},
		"Cookie":        {"session=xyz"},
		"X-Api-Key":     {"secret"},
		"X-Csrf-Token":  {"t"},
		"Content-Type":  {"application/json"},
		"X-Multi":       {"a", "b"},
	}
	out := flattenHeaders(h)

	// Authorization passes through.
	if out["Authorization"] != "Bearer abc" {
		t.Errorf("Authorization should pass through, got %q", out["Authorization"])
	}
	// Sensitive headers stripped (case-insensitively).
	for _, k := range []string{"Cookie", "X-Api-Key", "X-Csrf-Token"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should have been stripped: %v", k, out)
		}
	}
	// Plain header preserved.
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type lost: %v", out)
	}
	// Multi-value comma-joined per RFC 7230 §3.2.2.
	if out["X-Multi"] != "a, b" {
		t.Errorf("X-Multi = %q, want %q", out["X-Multi"], "a, b")
	}
}

func TestFlattenHeaders_Empty(t *testing.T) {
	if flattenHeaders(nil) != nil {
		t.Error("nil header → nil map")
	}
	// All-sensitive input yields nil (not an empty map).
	only := http.Header{"Cookie": {"x"}, "X-Api-Key": {"y"}}
	if flattenHeaders(only) != nil {
		t.Errorf("all-sensitive header set → nil, got %v", flattenHeaders(only))
	}
}

func TestApplyHeaderWriteBack(t *testing.T) {
	base := func() http.Header {
		return http.Header{
			"Authorization":  {"Bearer caller"},
			"X-Auth-User-Id": {"root"},
			"Accept":         {"a", "b"},
			"Cookie":         {"sid=1"},
			"X-Api-Key":      {"k"},
			"Content-Length": {"10"},
		}
	}
	sentOf := func(h http.Header) map[string]string { return flattenHeaders(h) }

	t.Run("strip", func(t *testing.T) {
		h := base()
		ret := sentOf(h)
		delete(ret, "X-Auth-User-Id")
		if changed, dropped, err := applyHeaderWriteBack(h, sentOf(base()), ret); err != nil || !changed || dropped != nil {
			t.Fatalf("changed = %v, dropped = %v", changed, dropped)
		}
		if _, ok := h["X-Auth-User-Id"]; ok {
			t.Fatal("stripped header still forwarded")
		}
		if h.Get("Authorization") != "Bearer caller" {
			t.Fatal("unrelated header changed")
		}
	})

	t.Run("overwrite spoofed value", func(t *testing.T) {
		h := base()
		ret := sentOf(h)
		delete(ret, "X-Auth-User-Id")
		ret["x-auth-user-id"] = "alice"
		mustWriteBack(t, h, sentOf(base()), ret)
		if got := h.Values("X-Auth-User-Id"); len(got) != 1 || got[0] != "alice" {
			t.Fatalf("X-Auth-User-Id = %v, want [alice]", got)
		}
	})

	t.Run("unmodified map keeps multi-value headers", func(t *testing.T) {
		h := base()
		if changed, _, _ := applyHeaderWriteBack(h, sentOf(base()), sentOf(base())); changed {
			t.Fatal("unmodified map reported a change")
		}
		if got := h.Values("Accept"); len(got) != 2 {
			t.Fatalf("Accept = %v, want two values", got)
		}
		if len(h) != len(base()) {
			t.Fatalf("headers = %v, want unchanged", h)
		}
	})

	t.Run("nil or empty map deletes nothing", func(t *testing.T) {
		for _, ret := range []map[string]string{nil, {}} {
			h := base()
			if changed, _, _ := applyHeaderWriteBack(h, sentOf(base()), ret); changed {
				t.Fatalf("returned %v: reported a change", ret)
			}
			if h.Get("Authorization") != "Bearer caller" || len(h) != len(base()) {
				t.Fatalf("returned %v: headers = %v, want unchanged", ret, h)
			}
		}
	})

	t.Run("headers never sent are untouched", func(t *testing.T) {
		h := base()
		ret := map[string]string{"Authorization": "Bearer caller", "Cookie": "sid=evil"}
		_, dropped := mustWriteBack(t, h, sentOf(base()), ret)
		if h.Get("Cookie") != "sid=1" || h.Get("X-Api-Key") != "k" {
			t.Fatalf("unsent headers changed: %v", h)
		}
		if len(dropped) != 2 || dropped[0] != "Content-Length" || dropped[1] != "Cookie" {
			t.Fatalf("dropped = %v, want [Content-Length Cookie]", dropped)
		}
	})

	t.Run("protected headers are never written", func(t *testing.T) {
		h := base()
		ret := sentOf(h)
		ret["host"] = "evil.example"
		ret["Content-Length"] = "999"
		ret["X-Authbridge-Direction"] = "inbound"
		ret["Proxy-Connection"] = "keep-alive"
		_, dropped := mustWriteBack(t, h, sentOf(base()), ret)
		want := []string{"Content-Length", "Host", "Proxy-Connection", "X-Authbridge-Direction"}
		if strings.Join(dropped, ",") != strings.Join(want, ",") {
			t.Fatalf("dropped = %v, want %v", dropped, want)
		}
		for _, name := range []string{"Host", "X-Authbridge-Direction", "Proxy-Connection"} {
			if _, ok := h[name]; ok {
				t.Errorf("%s was set", name)
			}
		}
		if h.Get("Content-Length") != "10" {
			t.Errorf("Content-Length = %q, want 10", h.Get("Content-Length"))
		}
	})

	t.Run("authorization can be stripped", func(t *testing.T) {
		h := base()
		ret := sentOf(h)
		delete(ret, "Authorization")
		mustWriteBack(t, h, sentOf(base()), ret)
		if _, ok := h["Authorization"]; ok {
			t.Fatal("Authorization still forwarded")
		}
	})
}

func TestApplyHeaderWriteBackRejectsInvalid(t *testing.T) {
	sent := map[string]string{"Accept": "a", "X-Trace": "1"}
	cases := map[string]map[string]string{
		"CRLF in value":  {"Accept": "a", "X-Trace": "1\r\nX-Injected: yes"},
		"LF in value":    {"Accept": "a", "X-Added": "v\nx"},
		"NUL in value":   {"Accept": "a", "X-Added": "v\x00"},
		"space in name":  {"Accept": "a", "X-Trace": "1", "Bad Name": "v"},
		"colon in name":  {"Accept": "a", "X-Trace": "1", "X-Bad:": "v"},
		"CRLF in name":   {"Accept": "a", "X-Trace": "1", "X-A\r\nX-B": "v"},
		"empty name":     {"Accept": "a", "X-Trace": "1", "": "v"},
		"case collision": {"Accept": "a", "X-Trace": "1", "x-new": "1", "X-New": "2"},
	}
	for name, ret := range cases {
		t.Run(name, func(t *testing.T) {
			h := http.Header{"Accept": {"a"}, "X-Trace": {"1"}}
			changed, _, err := applyHeaderWriteBack(h, sent, ret)
			if err == nil {
				t.Fatal("invalid write-back accepted")
			}
			if changed || len(h) != 2 || h.Get("X-Trace") != "1" {
				t.Fatalf("headers written despite the error: changed=%v %v", changed, h)
			}
		})
	}
	// A case collision with equal values is not ambiguous.
	h := http.Header{"Accept": {"a"}}
	if _, _, err := applyHeaderWriteBack(h, map[string]string{"Accept": "a"},
		map[string]string{"accept": "a", "Accept": "a", "X-New": "1"}); err != nil || h.Get("X-New") != "1" {
		t.Fatalf("err = %v, headers = %v", err, h)
	}
}

func TestApplyHeaderWriteBackProtectsHopByHopAndExtra(t *testing.T) {
	sent := map[string]string{"Accept": "a", "X-User-Token": "caller"}
	ret := map[string]string{
		"Accept": "a", "Te": "trailers", "Trailer": "X", "Upgrade": "h2c", "Keep-Alive": "1",
		"X-User-Token": "cortex-held",
	}
	h := http.Header{"Accept": {"a"}, "X-User-Token": {"caller"}}
	changed, dropped, err := applyHeaderWriteBack(h, sent, ret, "x-user-token")
	if err != nil || changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	want := "Keep-Alive,Te,Trailer,Upgrade,X-User-Token"
	if strings.Join(dropped, ",") != want {
		t.Fatalf("dropped = %v, want %s", dropped, want)
	}
	if h.Get("X-User-Token") != "caller" || len(h) != 2 {
		t.Fatalf("headers = %v", h)
	}
}

func mustWriteBack(t *testing.T, h http.Header, sent, ret map[string]string) (bool, []string) {
	t.Helper()
	changed, dropped, err := applyHeaderWriteBack(h, sent, ret)
	if err != nil {
		t.Fatal(err)
	}
	return changed, dropped
}
