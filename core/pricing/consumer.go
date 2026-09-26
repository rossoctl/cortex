package pricing

// ResolverConsumer is implemented by plugins that need to turn tokens into
// dollars. plugins.BuildWithDeps injects the process Resolver before Configure
// runs, so a plugin's configuration code can reach it.
//
// This mirrors spiffe.ProviderConsumer exactly, and for the same reason: plugin
// factories take no construction arguments (see plugins.PluginFactory), so a
// process-wide dependency has to arrive by injection rather than through the
// plugin's own config. Sharing the shape means the registry's injection path and
// its need-detection probe work the same way for both.
//
// A plugin must tolerate never being called — a build that opted out of pricing
// passes nothing, and the plugin's zero-value Resolver must then report traffic as
// unpriced rather than panicking. Holding a *Registry satisfies that for free,
// since a nil *Registry resolves to ProvNone.
type ResolverConsumer interface {
	SetPricingResolver(Resolver)
}
