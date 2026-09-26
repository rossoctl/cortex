// Package tlsbridge implements AuthBridge's outbound TLS bridge: it forges a
// per-origin leaf so the agent's egress TLS terminates at AuthBridge, the
// existing outbound pipeline runs on the decrypted request, and the call is
// relayed over a separately-verified upstream TLS connection. Un-bridgeable or
// pinned traffic falls open to a plain tunnel and self-heals via an auto-skip set.
package tlsbridge
