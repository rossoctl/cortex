package redact

import (
	"net/http"
	"testing"
)

func TestHeaders_DropsCredentialsAndCopies(t *testing.T) {
	in := http.Header{"Authorization": {"Bearer x"}, "X-Api-Key": {"k"}, "cookie": {"c"}, "Anthropic-Beta": {"a", "b"}}
	out := Headers(in)
	if len(out) != 1 || len(out["Anthropic-Beta"]) != 2 {
		t.Fatalf("Headers = %v, want only Anthropic-Beta", out)
	}
	out["Anthropic-Beta"][0] = "changed"
	if in["Anthropic-Beta"][0] != "a" {
		t.Fatal("Headers shares its values with its input")
	}
}

// Some APIs take the key in the URL (Gemini's ?key=). A captured query keeps everything else.
func TestQuery_DropsCredentialParameters(t *testing.T) {
	for in, want := range map[string]string{
		"beta=true":                         "beta=true",
		"key=AIza123&alt=sse":               "alt=sse",
		"api_key=k&apikey=k&access_token=t": "",
		"token=t&x=1":                       "x=1",
		"z=1&a=%2F&b":                       "z=1&a=%2F&b",
		"z=1&key=k&a=2":                     "z=1&a=2",
		"%zz":                               "",
		"":                                  "",
	} {
		if got := Query(in); got != want {
			t.Errorf("Query(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaders_DropsCloudAndProxyCredentials(t *testing.T) {
	for _, h := range []string{"X-Amz-Security-Token", "X-Auth-Token", "X-Authorization", "X-Session-Token",
		"X-Secret-Token", "X-Csrf-Token", "X-Platform-Secret", "X-Authbridge-Secret"} {
		if out := Headers(http.Header{h: {"s"}}); len(out) != 0 {
			t.Errorf("%s survived", h)
		}
	}
}

// A credential is judged by the words of its name, split at "-" and "_", so a header or
// parameter with a credential word needs no entry of its own.
func TestHeadersAndQuery_DropNamesWithACredentialWord(t *testing.T) {
	names := []string{"Ocp-Apim-Subscription-Key", "Private-Token", "X-Vault-Token", "X-Goog-Iam-Authorization-Token"}
	for h := range SecretHeaders {
		names = append(names, h)
	}
	for _, h := range names {
		if out := Headers(http.Header{h: {"s"}}); len(out) != 0 {
			t.Errorf("%s survived", h)
		}
	}
	in := "X-Amz-Credential=c&X-Amz-Signature=s&X-Amz-Security-Token=t&subscription-key=k&X-Amz-Expires=60"
	if got := Query(in); got != "X-Amz-Expires=60" {
		t.Errorf("Query(%q) = %q, want X-Amz-Expires=60", in, got)
	}
	if got := Query("max_tokens=5&pageToken=p"); got != "max_tokens=5&pageToken=p" {
		t.Errorf("Query kept %q, want both: neither name has a credential word", got)
	}
}

// The names docs/laptop-service.md gives as examples of the word rule.
func TestCredentialName_DocumentedExamples(t *testing.T) {
	for name, want := range map[string]bool{"Idempotency-Key": true, "Sec-WebSocket-Key": true, "accessToken": false} {
		if got := CredentialName(name); got != want {
			t.Errorf("CredentialName(%q) = %v, want %v", name, got, want)
		}
	}
}
