package observe

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/internal/wirefixture"
)

// agentop decodes testdata/config.json too (runningConfigFields), so a renamed key on
// either side fails a test.
func TestConfigEndpoint_CoversTheWireFixture(t *testing.T) {
	cfg := newTestConfig()
	cfg.TLSBridge = &config.TLSBridgeConfig{Mode: "enabled", CADir: "/ca"}
	w := httptest.NewRecorder()
	serveMux(cfg, auth.NewStats()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
	wirefixture.Covers(t, "testdata/config.json", w.Body.Bytes())
}
