package reloader

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/internal/wirefixture"
)

// agentop decodes testdata/reload-status.json too (cmd/agentop/edit), so a renamed
// key on either side fails a test.
func TestStatusHandler_CoversTheWireFixture(t *testing.T) {
	r := &Reloader{}
	r.status.Store(&Status{})
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reload/status", nil))
	wirefixture.Covers(t, "testdata/reload-status.json", w.Body.Bytes())
}
