// Package pivot holds the protocol-neutral form of an inference request
// and reply, and the contract that each protocol package implements.
//
// The pivot type holds the union of what the protocols can say, and loses
// nothing that a later turn needs (rule H2).
package pivot

import "encoding/json"

// Extra holds the fields that the pivot type has no place for. A decoder
// puts each such field here under the key "<protocol>.<field>". An encoder
// reads only the keys of its own protocol.
type Extra map[string]json.RawMessage

// BlockKind is the kind of a content block.
type BlockKind string

// The block kinds of rule H2.
const (
	BlockText             BlockKind = "text"
	BlockImage            BlockKind = "image"
	BlockDocument         BlockKind = "document"
	BlockToolUse          BlockKind = "tool_use"
	BlockToolResult       BlockKind = "tool_result"
	BlockThinking         BlockKind = "thinking"
	BlockRedactedThinking BlockKind = "redacted_thinking"
)

// Request is one inference request in the pivot form.
type Request struct {
	Model       string
	System      []Block // text blocks
	Messages    []Message
	Tools       []Tool
	ToolChoice  *ToolChoice
	MaxTokens   int
	Stop        []string
	Temperature *float64
	TopP        *float64
	Stream      bool
	Effort      string    // "" when none
	Thinking    *Thinking // nil when none
	Extra       Extra
}

// Message is one turn of the conversation. Role is user, assistant or
// system.
type Message struct {
	Role    string
	Content []Block
	Extra   Extra
}

// Block is one content block. Kind tells which fields apply.
type Block struct {
	Kind         BlockKind
	Text         string          // text; thinking text
	SourceType   string          // image/document: "base64" or "url"
	MediaType    string          // image/document
	Data         string          // base64 data; redacted_thinking data
	URL          string          // image/document with SourceType "url"
	ID, Name     string          // tool_use
	Input        json.RawMessage // tool_use input, raw
	ToolUseID    string          // tool_result
	Content      []Block         // tool_result content
	IsError      bool            // tool_result
	Signature    string          // thinking
	CacheControl json.RawMessage // nil when none
	Extra        Extra
}

// Tool is one tool that the model can call.
type Tool struct {
	Name, Description string
	InputSchema       json.RawMessage
	Type              string // "" for a function tool; else a server tool type
	CacheControl      json.RawMessage
	Extra             Extra
}

// ToolChoice tells the model how to use the tools. Type is auto, any,
// tool or none. Name is set when Type is tool.
type ToolChoice struct {
	Type            string
	Name            string
	DisableParallel bool
}

// Thinking is the extended-thinking setting. Type is enabled, adaptive or
// disabled.
type Thinking struct {
	Type         string
	BudgetTokens int
}

// Usage counts tokens with the Anthropic meaning: Input does not include
// the cache tokens. Each decoder converts to this meaning.
type Usage struct {
	Input, Output, CacheRead, CacheCreation int
}
