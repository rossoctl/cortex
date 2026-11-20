package httpx

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/model"
)

// X-Cortex-Record is never sent upstream (#2005): not the client's copy, and not one a
// plugin wrote back into the pipeline's headers, in any case. Other headers sync as before.
func TestSyncRequestHeaders_NeverForwardsTheRecordHeader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(h http.Header)
	}{
		{"client's copy left in place", func(http.Header) {}},
		{"written back", func(h http.Header) { h.Set(model.RecordHeader, "off") }},
		{"written back non-canonical", func(h http.Header) { h["x-cortex-record"] = []string{"off"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "http://upstream.example/v1/messages", nil)
			r.Header.Set(model.RecordHeader, "off")
			r.Header.Set("X-Keep", "a")
			// The pipeline's headers start as a clone of the request's; the request's
			// NoRecord removes the header from them, then a plugin may write it back.
			h := r.Header.Clone()
			h.Del(model.RecordHeader)
			h.Set("X-Added", "b")
			tc.write(h)
			SyncRequestHeaders(r, h)
			for k := range r.Header {
				if strings.EqualFold(k, model.RecordHeader) {
					t.Fatalf("the request to send still has %s: %v", k, r.Header)
				}
			}
			if r.Header.Get("X-Keep") != "a" || r.Header.Get("X-Added") != "b" {
				t.Fatalf("other headers did not sync: %v", r.Header)
			}
		})
	}
}
