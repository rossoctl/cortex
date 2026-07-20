package tokenbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// =============================================================================
// Route Matching and Policy Tests
// =============================================================================

func TestTokenBroker_OnRequest_RouteMatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "acquired-token"})
	}))
	defer srv.Close()

	p := NewTokenBroker()
	config := `{
		"broker_url": "` + srv.URL + `",
		"default_policy": "passthrough",
		"routes": {
			"rules": [
				{
					"host": "api.example.com",
					"action": "broker"
				},
				{
					"host": "implicit-broker.example.com"
				},
				{
					"host": "other.example.com",
					"action": "passthrough"
				}
			]
		}
	}`
	if err := p.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	pctx1 := &pipeline.Context{
		Host: "api.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer user-token"},
		},
	}
	action1 := p.OnRequest(context.Background(), pctx1)
	if action1.Type != pipeline.Continue {
		t.Errorf("OnRequest() for broker route: action.Type = %v, want %v", action1.Type, pipeline.Continue)
	}
	if auth := pctx1.Headers.Get("Authorization"); auth != "Bearer acquired-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer acquired-token")
	}

	pctxImplicit := &pipeline.Context{
		Host: "implicit-broker.example.com",
		Headers: http.Header{
			"Authorization": []string{"Bearer implicit-token"},
		},
	}
	actionImplicit := p.OnRequest(context.Background(), pctxImplicit)
	if actionImplicit.Type != pipeline.Continue {
		t.Errorf("OnRequest() for implicit broker route: action.Type = %v, want %v", actionImplicit.Type, pipeline.Continue)
	}
	if auth := pctxImplicit.Headers.Get("Authorization"); auth != "Bearer acquired-token" {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer acquired-token")
	}

	originalToken := "Bearer original-token"
	pctx2 := &pipeline.Context{
		Host: "other.example.com",
		Headers: http.Header{
			"Authorization": []string{originalToken},
		},
	}
	action2 := p.OnRequest(context.Background(), pctx2)
	if action2.Type != pipeline.Continue {
		t.Errorf("OnRequest() for passthrough route: action.Type = %v, want %v", action2.Type, pipeline.Continue)
	}
	if auth := pctx2.Headers.Get("Authorization"); auth != originalToken {
		t.Errorf("Authorization header = %q, want %q (unchanged)", auth, originalToken)
	}
}

func TestTokenBroker_OnRequest_DefaultPolicyRouting(t *testing.T) {
	t.Run("unmatched host with broker default uses broker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"token": "default-broker-token"})
		}))
		defer srv.Close()

		p := NewTokenBroker()
		config := `{
			"broker_url": "` + srv.URL + `",
			"default_policy": "broker",
			"routes": {
				"rules": [
					{"host": "matched.example.com", "action": "passthrough"}
				]
			}
		}`
		if err := p.Configure(json.RawMessage(config)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		pctx := &pipeline.Context{
			Host: "unmatched.example.com",
			Headers: http.Header{
				"Authorization": []string{"Bearer default-token"},
			},
		}

		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Continue {
			t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
		}
		if auth := pctx.Headers.Get("Authorization"); auth != "Bearer default-broker-token" {
			t.Errorf("Authorization header = %q, want %q", auth, "Bearer default-broker-token")
		}
	})

	t.Run("unmatched host with passthrough default does not use broker", func(t *testing.T) {
		p := NewTokenBroker()
		config := `{
			"broker_url": "http://broker:8080",
			"default_policy": "passthrough",
			"routes": {
				"rules": [
					{"host": "matched.example.com", "action": "broker"}
				]
			}
		}`
		if err := p.Configure(json.RawMessage(config)); err != nil {
			t.Fatalf("Configure() error = %v", err)
		}

		originalToken := "Bearer untouched-token"
		pctx := &pipeline.Context{
			Host: "unmatched.example.com",
			Headers: http.Header{
				"Authorization": []string{originalToken},
			},
		}

		action := p.OnRequest(context.Background(), pctx)
		if action.Type != pipeline.Continue {
			t.Fatalf("OnRequest() action.Type = %v, want %v", action.Type, pipeline.Continue)
		}
		if auth := pctx.Headers.Get("Authorization"); auth != originalToken {
			t.Errorf("Authorization header = %q, want %q", auth, originalToken)
		}
	})
}
