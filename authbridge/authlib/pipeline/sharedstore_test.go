package pipeline_test

import (
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/rossocortex/authbridge/authlib/shared"
)

// shared.Store must satisfy pipeline.SharedStore so listeners can inject it.
func TestSharedStore_StoreSatisfiesInterface(t *testing.T) {
	var _ pipeline.SharedStore = shared.New()
}

// Context must expose a Shared field of the interface type.
func TestSharedStore_ContextField(t *testing.T) {
	pctx := &pipeline.Context{Shared: shared.New()}
	if pctx.Shared == nil {
		t.Fatal("Context.Shared not assignable")
	}
}
