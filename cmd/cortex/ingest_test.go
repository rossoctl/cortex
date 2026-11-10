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
	if opts := ingestOptions(praxisCfg(false), nil, "tok", nil); opts != nil {
		t.Fatalf("got %v; want nil", opts)
	}
}

func TestReadGatewayToken_NeedsTokenFile(t *testing.T) {
	_, err := readGatewayToken(func(string) string { return "" },
		func(string) ([]byte, error) { return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "CORTEX_GATEWAY_TOKEN_FILE") {
		t.Fatalf("err = %v, want one that names CORTEX_GATEWAY_TOKEN_FILE", err)
	}
}

func TestReadGatewayToken_Unreadable(t *testing.T) {
	_, err := readGatewayToken(func(string) string { return "/x" },
		func(string) ([]byte, error) { return nil, errors.New("no such file") })
	if err == nil || !strings.Contains(err.Error(), "read the gateway token file") {
		t.Fatalf("err = %v, want an unreadable-file error", err)
	}
}

func TestReadGatewayToken_EmptyToken(t *testing.T) {
	_, err := readGatewayToken(func(string) string { return "/x" },
		func(string) ([]byte, error) { return []byte(" \n"), nil })
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("err = %v, want an empty-token error", err)
	}
}

func TestIngestOptions_ReadsToken(t *testing.T) {
	token, err := readGatewayToken(
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
	if err != nil {
		t.Fatal(err)
	}
	opts := ingestOptions(praxisCfg(true), nil, token, nil)
	if len(opts) != 1 {
		t.Fatalf("got %d options; want 1", len(opts))
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
