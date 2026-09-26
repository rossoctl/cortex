package redact

import (
	"encoding/json"
	"testing"
)

func TestJSON_RedactsClientSecret(t *testing.T) {
	in := json.RawMessage(`{"client_secret":"s3cret","issuer":"https://idp"}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["client_secret"] != "[REDACTED]" {
		t.Errorf("client_secret = %v, want [REDACTED]", m["client_secret"])
	}
	if m["issuer"] != "https://idp" {
		t.Errorf("issuer = %v, want https://idp", m["issuer"])
	}
}

func TestJSON_RedactsJudgeBearer(t *testing.T) {
	in := json.RawMessage(`{"judge_bearer":"tok","model":"gpt-4"}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["judge_bearer"] != "[REDACTED]" {
		t.Errorf("judge_bearer = %v, want [REDACTED]", m["judge_bearer"])
	}
	if m["model"] != "gpt-4" {
		t.Errorf("model = %v, want gpt-4", m["model"])
	}
}

func TestJSON_RedactsNestedKeys(t *testing.T) {
	in := json.RawMessage(`{"identity":{"jwt_password":"xyz","url":"https://kc"}}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	inner := m["identity"].(map[string]any)
	if inner["jwt_password"] != "[REDACTED]" {
		t.Errorf("jwt_password = %v, want [REDACTED]", inner["jwt_password"])
	}
	if inner["url"] != "https://kc" {
		t.Errorf("url = %v, want https://kc", inner["url"])
	}
}

func TestJSON_PreservesNonSensitiveKeys(t *testing.T) {
	in := json.RawMessage(`{"bypass_paths":["/.well-known/*"],"issuer":"https://idp"}`)
	out := JSON(in)
	if string(out) == "" {
		t.Fatal("output is empty")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["issuer"] != "https://idp" {
		t.Errorf("issuer changed unexpectedly: %v", m["issuer"])
	}
}

func TestJSON_NonObjectPassthrough(t *testing.T) {
	tests := []string{`"just a string"`, `[1,2,3]`, `null`, `42`}
	for _, tt := range tests {
		in := json.RawMessage(tt)
		out := JSON(in)
		if string(out) != tt {
			t.Errorf("JSON(%s) = %s, want passthrough", tt, string(out))
		}
	}
}

func TestJSON_EmptyInput(t *testing.T) {
	out := JSON(nil)
	if out != nil {
		t.Errorf("JSON(nil) = %v, want nil", out)
	}
	out = JSON(json.RawMessage{})
	if len(out) != 0 {
		t.Errorf("JSON(empty) = %v, want empty", out)
	}
}

func TestJSON_NonStringSecretPreserved(t *testing.T) {
	in := json.RawMessage(`{"client_secret":12345}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["client_secret"] != float64(12345) {
		t.Errorf("non-string secret should not be redacted: %v", m["client_secret"])
	}
}

func TestJSON_ContainerValuedSensitiveKey(t *testing.T) {
	in := json.RawMessage(`{"token":{"access_token":"secret","url":"https://idp"}}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	inner := m["token"].(map[string]any)
	if inner["access_token"] != "[REDACTED]" {
		t.Errorf("access_token inside container-valued sensitive key = %v, want [REDACTED]", inner["access_token"])
	}
	if inner["url"] != "https://idp" {
		t.Errorf("url inside container-valued sensitive key = %v, want https://idp", inner["url"])
	}
}

func TestJSON_RedactsApiKey(t *testing.T) {
	in := json.RawMessage(`{"api_key":"k3y","endpoint":"https://api"}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["api_key"] != "[REDACTED]" {
		t.Errorf("api_key = %v, want [REDACTED]", m["api_key"])
	}
	if m["endpoint"] != "https://api" {
		t.Errorf("endpoint = %v, want https://api", m["endpoint"])
	}
}

func TestJSON_NestedArrayRedaction(t *testing.T) {
	in := json.RawMessage(`{"data":[[{"client_secret":"xyz","url":"https://a"}]]}`)
	out := JSON(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	outer := m["data"].([]any)
	inner := outer[0].([]any)
	obj := inner[0].(map[string]any)
	if obj["client_secret"] != "[REDACTED]" {
		t.Errorf("nested array secret = %v, want [REDACTED]", obj["client_secret"])
	}
	if obj["url"] != "https://a" {
		t.Errorf("nested array url = %v, want https://a", obj["url"])
	}
}
