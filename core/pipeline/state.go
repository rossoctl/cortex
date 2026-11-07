package pipeline

// SetState stashes a typed value on pctx under key. Intended for plugin-
// private per-request state — e.g., a rate-limiter remembering how many
// tokens were available when OnRequest saw the call, for OnResponse to
// consult. The generic type parameter is documentary: it forces callers
// to pass *T rather than an unrelated interface, which pairs with the
// symmetric type-assert in GetState.
//
// Convention: `key` should be the plugin's Name() so keys from unrelated
// plugins don't collide. SetState is not safe for concurrent use — pctx
// is single-threaded per request in the pipeline.
func SetState[T any](pctx *Context, key string, v *T) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[key] = v
}

// GetState retrieves a typed value previously stored via SetState. Returns
// nil when the key is absent or when the stored value is not a *T —
// safe-fails rather than panicking so a mid-pipeline type migration
// (plugin version skew) degrades instead of crashing the handler.
func GetState[T any](pctx *Context, key string) *T {
	if pctx.Extensions.Custom == nil {
		return nil
	}
	v, ok := pctx.Extensions.Custom[key].(*T)
	if !ok {
		return nil
	}
	return v
}
