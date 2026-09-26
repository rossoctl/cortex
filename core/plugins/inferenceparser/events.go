package inferenceparser

// knownAnthropicEvents is the set of Anthropic stream event types
// foldAnthropicFrame handles, including the ones it deliberately ignores
// (ping, message_stop). A type outside this set means the wire format has moved
// ahead of the parser: the frame still parses, so nothing errors, but any usage
// it carried is dropped while completion text keeps accumulating — token
// telemetry then reads as absent rather than wrong. foldAnthropicFrame logs
// such a type at Debug so that outcome is diagnosable.
//
// Kept beside the switch it mirrors. TestKnownAnthropicEvents_MatchesSwitch
// parses foldAnthropicFrame's cases out of the source and asserts the two agree
// in BOTH directions, so a case added to the switch without a map entry — or a
// map entry with no case — fails rather than silently going unlogged.
//
// intentionallyIgnored names the types that are deliberately absent from the
// switch: they are valid, expected, and carry nothing the parser needs. They
// belong in the map (so they log nothing) but have no case, and the test needs
// to tell them apart from an accidental omission.
var knownAnthropicEvents = map[string]bool{
	"message_start":       true,
	"content_block_start": true,
	"content_block_delta": true,
	"content_block_stop":  true,
	"message_delta":       true,
	"message_stop":        true,
	"ping":                true,
	"error":               true,
}

// intentionallyIgnored are known event types with no case in
// foldAnthropicFrame: nothing in them affects the running completion or usage.
var intentionallyIgnored = map[string]bool{
	"message_stop": true, // terminator; finalize is driven by the listener's last=true
	"ping":         true, // keepalive
}
