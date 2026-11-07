package inference

import (
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// No listener decompresses a request body, so ParseRequest decodes it for every dialect.
// Without that, json.Unmarshal fails on the compressed bytes and the request gets no
// inference extension.
func TestParseRequestDecodesZstdForEveryDialect(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer func() { _ = enc.Close() }()
	zstdHeader := http.Header{"Content-Encoding": []string{"zstd"}}

	for _, tc := range []struct {
		path, model, body string
	}{
		{"/v1/messages", "anthropic-model", `{"model":"anthropic-model","max_tokens":8,"messages":[{"role":"user","content":"hi from messages"}]}`},
		{"/v1/chat/completions", "chat-model", `{"model":"chat-model","messages":[{"role":"user","content":"hi from chat"}]}`},
		{"/v1/responses", "responses-model", `{"model":"responses-model","input":"hi from responses"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ext := ParseRequest(tc.path, zstdHeader, enc.EncodeAll([]byte(tc.body), nil))
			if ext == nil {
				t.Fatal("ParseRequest = nil, want the decoded request parsed")
			}
			if ext.Model != tc.model || len(ext.Messages) != 1 {
				t.Fatalf("ext = %+v, want model %q with one message", ext, tc.model)
			}
		})
	}
}
