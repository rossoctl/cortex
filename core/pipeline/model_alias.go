package pipeline

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// The data types below live in core/model, so that the session store, the archive and the
// cost packages can use them without the plugin engine. These aliases keep the old names.
// They are aliases (=), not new types: a new type would drop the methods, MarshalJSON too.

// client.go: see core/model for the docs.
type EventClient = model.EventClient

const UnknownClientLabel = model.UnknownClientLabel

// maxClientLen is model's cap on a User-Agent field, which context.go and subagent.go
// also apply. TestParseUserAgent_* in client_test.go fails if the two differ.
const maxClientLen = 128

func capUA(s string) string { return TruncateLabel(s, maxClientLen) }

func SanitizeControlRunes(s string) string                      { return model.SanitizeControlRunes(s) }
func SanitizeLogValue(s string) string                          { return model.SanitizeLogValue(s) }
func HasControlRunes(s string) bool                             { return model.HasControlRunes(s) }
func IsControlRune(r rune) bool                                 { return model.IsControlRune(r) }
func TruncateLabel(s string, n int) string                      { return model.TruncateLabel(s, n) }
func IsKnownAgent(name string) bool                             { return model.IsKnownAgent(name) }
func ParseUserAgent(ua string) *EventClient                     { return model.ParseUserAgent(ua) }
func CanonicalAgent(name string) string                         { return model.CanonicalAgent(name) }
func FoldAgentNames(agents map[string]string) map[string]string { return model.FoldAgentNames(agents) }
func AgentSpellings(name string) []string                       { return model.AgentSpellings(name) }
func AgentName(label string) string                             { return model.AgentName(label) }

// capture.go: see core/model for the docs.
type (
	RequestCapture  = model.RequestCapture
	ResponseCapture = model.ResponseCapture
	RawCapture      = model.RawCapture
	RawBody         = model.RawBody
)

const MaxResponseCapture = model.MaxResponseCapture

// tokens.go: see core/model for the docs.
func PromptTokensOf(resp *InferenceExtension) int { return model.PromptTokensOf(resp) }

// facts.go: see core/model for the docs.
type (
	FactState    = model.FactState
	SessionFact  = model.SessionFact
	SessionFacts = model.SessionFacts
)

const (
	FactNone      = model.FactNone
	FactNoSession = model.FactNoSession
	FactPresent   = model.FactPresent
)

// direction.go: see core/model for the docs.
type Direction = model.Direction

const (
	Inbound  = model.Inbound
	Outbound = model.Outbound
)

// extensions.go: see core/model for the docs.
type (
	Extensions          = model.Extensions
	MCPExtension        = model.MCPExtension
	MCPError            = model.MCPError
	A2AExtension        = model.A2AExtension
	A2APart             = model.A2APart
	InferenceExtension  = model.InferenceExtension
	AgentRole           = model.AgentRole
	InferenceMessage    = model.InferenceMessage
	RawJSON             = model.RawJSON
	InferenceTool       = model.InferenceTool
	InferenceToolCall   = model.InferenceToolCall
	InferenceThinking   = model.InferenceThinking
	InferenceToolResult = model.InferenceToolResult
	SecurityExtension   = model.SecurityExtension
	InvocationAction    = model.InvocationAction
	Invocations         = model.Invocations
	InvocationPhase     = model.InvocationPhase
	Invocation          = model.Invocation
	DelegationExtension = model.DelegationExtension
	DelegationHop       = model.DelegationHop
)

const (
	PluginEventSuffix       = model.PluginEventSuffix
	AgentRoleMain           = model.AgentRoleMain
	AgentRoleSubagent       = model.AgentRoleSubagent
	ActionAllow             = model.ActionAllow
	ActionDeny              = model.ActionDeny
	ActionSkip              = model.ActionSkip
	ActionModify            = model.ActionModify
	ActionObserve           = model.ActionObserve
	InvocationPhaseRequest  = model.InvocationPhaseRequest
	InvocationPhaseResponse = model.InvocationPhaseResponse
)

// session.go: see core/model for the docs.
type (
	SessionPhase  = model.SessionPhase
	SessionEvent  = model.SessionEvent
	TunnelReason  = model.TunnelReason
	EventTLS      = model.EventTLS
	EventIdentity = model.EventIdentity
	EventError    = model.EventError
	SessionSource = model.SessionSource
	SessionView   = model.SessionView
	ArchiveUsage  = model.ArchiveUsage
)

const (
	SessionRequest              = model.SessionRequest
	SessionResponse             = model.SessionResponse
	SessionDenied               = model.SessionDenied
	TunnelClientRejectedCA      = model.TunnelClientRejectedCA
	TunnelClientHungUp          = model.TunnelClientHungUp
	TunnelHandshakeFailed       = model.TunnelHandshakeFailed
	TunnelOriginUnverified      = model.TunnelOriginUnverified
	TunnelSkipCached            = model.TunnelSkipCached
	TunnelProgramRefused        = model.TunnelProgramRefused
	TunnelOSTrustOnly           = model.TunnelOSTrustOnly
	TunnelBridgeDisabled        = model.TunnelBridgeDisabled
	TunnelPassthroughPort       = model.TunnelPassthroughPort
	TunnelPassthroughNonTLS     = model.TunnelPassthroughNonTLS
	TunnelPassthroughHost       = model.TunnelPassthroughHost
	TunnelDialFailed            = model.TunnelDialFailed
	TunnelPassthroughUnknown    = model.TunnelPassthroughUnknown
	TunnelWebSocket             = model.TunnelWebSocket
	TunnelDestPrivate           = model.TunnelDestPrivate
	ErrorKindUpgradeDeclined    = model.ErrorKindUpgradeDeclined
	ErrorKindTurnInterrupted    = model.ErrorKindTurnInterrupted
	ErrorKindDestinationPrivate = model.ErrorKindDestinationPrivate
	SessionSourceHeader         = model.SessionSourceHeader
	SessionSourceProcess        = model.SessionSourceProcess
	SessionSourcePending        = model.SessionSourcePending
	SessionSourceAffinity       = model.SessionSourceAffinity
	SessionSourceActive         = model.SessionSourceActive
	SessionSourceConnection     = model.SessionSourceConnection
)

func NewEventTLS(state *tls.ConnectionState, peerSpiffeID string) *EventTLS {
	return model.NewEventTLS(state, peerSpiffeID)
}

// promptcontext.go: see core/model for the docs.
type (
	PromptContextFold = model.PromptContextFold
	PromptContext     = model.PromptContext
)

func PromptContextOf(events []SessionEvent) int             { return model.PromptContextOf(events) }
func MergePromptContext(a, b *PromptContext) *PromptContext { return model.MergePromptContext(a, b) }

// snapshot.go: see core/model for the docs. The helpers that take *Context stay in
// snapshot_context.go.
const StatusClientClosedRequest = model.StatusClientClosedRequest

func SnapshotA2A(ext *A2AExtension) *A2AExtension { return model.SnapshotA2A(ext) }
func SnapshotMCP(ext *MCPExtension) *MCPExtension { return model.SnapshotMCP(ext) }
func SnapshotInference(ext *InferenceExtension) *InferenceExtension {
	return model.SnapshotInference(ext)
}
func SnapshotInvocations(ext *Invocations, phase InvocationPhase) *Invocations {
	return model.SnapshotInvocations(ext, phase)
}
func SnapshotPlugins(custom map[string]any) map[string]json.RawMessage {
	return model.SnapshotPlugins(custom)
}
func DurationSince(start time.Time) time.Duration      { return model.DurationSince(start) }
func DurationBetween(from, to time.Time) time.Duration { return model.DurationBetween(from, to) }
func ExchangeFailure(ctx context.Context, err error) (int, *EventError) {
	return model.ExchangeFailure(ctx, err)
}
func TransportError(err error) *EventError { return model.TransportError(err) }
