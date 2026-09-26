package pipeline_test

import (
	"testing"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
)

// memstore.Store must satisfy pipeline.SharedStore so listeners can inject it.
func TestSharedStore_StoreSatisfiesInterface(t *testing.T) {
	var _ pipeline.SharedStore = memstore.New()
}

// Context must expose a Shared field of the interface type.
func TestSharedStore_ContextField(t *testing.T) {
	pctx := &pipeline.Context{Shared: memstore.New()}
	if pctx.Shared == nil {
		t.Fatal("Context.Shared not assignable")
	}
}
