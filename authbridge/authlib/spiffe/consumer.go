// authlib/spiffe/consumer.go
package spiffe

// ProviderConsumer is implemented by plugins that need access to the
// process-wide SPIFFE Provider. plugins.BuildWithSPIFFE invokes
// SetSPIFFEProvider on every plugin that satisfies this interface
// before Configure runs, so plugin configuration code can use the
// Provider's sources directly.
//
// Plugins that don't need SPIFFE simply don't implement this
// interface and are unaffected.
type ProviderConsumer interface {
	SetSPIFFEProvider(p *Provider)
}
