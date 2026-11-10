package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

func praxisCfg(enabled bool) *config.Config {
	cfg := &config.Config{}
	if enabled {
		cfg.Praxis = &config.PraxisConfig{Enabled: true}
	}
	return cfg
}

func TestIngestOptions_DirectModeNone(t *testing.T) {
	getenv := func(string) string { t.Fatal("getenv called in direct mode"); return "" }
	readFile := func(string) ([]byte, error) { t.Fatal("readFile called in direct mode"); return nil, nil }
	opts, err := ingestOptions(praxisCfg(false), nil, getenv, readFile)
	if err != nil || opts != nil {
		t.Fatalf("got %v, %v; want nil, nil", opts, err)
	}
}

func TestIngestOptions_NeedsTokenFile(t *testing.T) {
	_, err := ingestOptions(praxisCfg(true), nil, func(string) string { return "" },
		func(string) ([]byte, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "CORTEX_GATEWAY_TOKEN_FILE") {
		t.Fatalf("err = %v, want one that names CORTEX_GATEWAY_TOKEN_FILE", err)
	}
	_, err = ingestOptions(praxisCfg(true), nil, func(string) string { return "/x" },
		func(string) ([]byte, error) { return nil, errors.New("no such file") })
	if err == nil {
		t.Fatal("an unreadable token file must be an error")
	}
}

func TestIngestOptions_EmptyToken(t *testing.T) {
	_, err := ingestOptions(praxisCfg(true), nil, func(string) string { return "/x" },
		func(string) ([]byte, error) { return []byte(" \n"), nil })
	if err == nil {
		t.Fatal("an empty token must be an error")
	}
}

func TestIngestOptions_ReadsToken(t *testing.T) {
	opts, err := ingestOptions(praxisCfg(true), nil,
		func(k string) string {
			if k != "CORTEX_GATEWAY_TOKEN_FILE" {
				t.Fatalf("getenv(%q)", k)
			}
			return "/tok"
		},
		func(p string) ([]byte, error) {
			if p != "/tok" {
				t.Fatalf("readFile(%q)", p)
			}
			return []byte("abc\n"), nil
		})
	if err != nil || len(opts) != 1 {
		t.Fatalf("got %d options, %v; want 1, nil", len(opts), err)
	}
	srv := sessionapi.New(":0", session.New(0, 0, 0), opts...)
	ts := httptest.NewServer(srv.Server().Handler)
	defer ts.Close()
	// The trailing newline is trimmed: "abc" is the token, "abc\n" from the file is not.
	for token, want := range map[string]int{"abc": http.StatusNoContent, "abcd": http.StatusUnauthorized} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/ingest", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Bearer for %q: status %d, want %d", token, resp.StatusCode, want)
		}
	}
}
