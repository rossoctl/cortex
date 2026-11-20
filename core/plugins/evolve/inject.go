package evolve

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// dialect is the wire format of an inference request, chosen by path suffix as
// inference-parser chooses it.
type dialect int

const (
	dialectNone dialect = iota
	dialectAnthropic
	dialectChat
	dialectResponses
)

func dialectOf(path string) dialect {
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		return dialectAnthropic
	case strings.HasSuffix(path, "/chat/completions"):
		return dialectChat
	case strings.HasSuffix(path, "/responses"):
		return dialectResponses
	}
	return dialectNone
}

func pathOnly(target string) string {
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if len(target) > 1 && strings.HasSuffix(target, "/") {
		target = strings.TrimRight(target, "/")
	}
	return target
}

const blockTag = "evolve-guidelines"

// tagPattern matches anything that could open or close the guidelines block, so
// guideline text cannot end the block early or forge a second one.
var tagPattern = regexp.MustCompile(`(?i)</?\s*` + blockTag + `[^>]*>`)

// blockPattern matches a whole injected block, for removal from trajectories.
var blockPattern = regexp.MustCompile(`(?is)<` + blockTag + `\b[^>]*>.*?</` + blockTag + `>`)

// formatBlock renders guidelines as the delimited block appended to the system
// prompt. The output is a pure function of its input: the same guidelines give
// the same bytes on every turn, so the provider's prompt cache keeps hitting.
func formatBlock(gs []Guideline) string {
	ids := make([]string, len(gs))
	var b strings.Builder
	for i, g := range gs {
		ids[i] = g.ID
	}
	fmt.Fprintf(&b, "<%s source=\"altk-evolve\" ids=\"%s\">\n", blockTag, strings.Join(ids, ","))
	b.WriteString("Guidelines learned from earlier sessions on this machine. Apply them when they are relevant to the task; ignore them otherwise.\n")
	for i, g := range gs {
		content := strings.TrimSpace(tagPattern.ReplaceAllString(g.Content, ""))
		content = strings.ReplaceAll(content, "\n", " ")
		fmt.Fprintf(&b, "%d. %s\n", i+1, content)
	}
	fmt.Fprintf(&b, "</%s>", blockTag)
	return b.String()
}

var errUnsupportedShape = errors.New("unsupported request shape")

const (
	placementSystem = "system"
	placementUser   = "user"
)

// injectBlock adds block to the request and returns the new body: after the
// system instructions, or with placement "user" as a <system-reminder> leading
// the first user message. It edits the raw bytes with sjson rather than
// re-marshalling, so every field it does not touch — cache_control markers
// included — survives byte-for-byte.
func injectBlock(body []byte, d dialect, placement, block string) ([]byte, error) {
	if placement == placementUser {
		return injectFirstUser(body, d, "<system-reminder>\n"+block+"\n</system-reminder>")
	}
	switch d {
	case dialectAnthropic:
		return injectAnthropic(body, block)
	case dialectChat:
		return injectChat(body, block)
	case dialectResponses:
		return injectResponses(body, block)
	}
	return nil, errUnsupportedShape
}

func injectAnthropic(body []byte, block string) ([]byte, error) {
	system := gjson.GetBytes(body, "system")
	switch {
	case !system.Exists():
		return sjson.SetBytes(body, "system", block)
	case system.Type == gjson.String:
		return sjson.SetBytes(body, "system", joinText(system.String(), block))
	case system.IsArray():
		// A new trailing block, after any cache_control breakpoint: the cached
		// prefix is unchanged, and the block itself is stable across turns.
		return sjson.SetBytes(body, "system.-1", map[string]string{"type": "text", "text": block})
	}
	return nil, errUnsupportedShape
}

func injectChat(body []byte, block string) ([]byte, error) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil, errUnsupportedShape
	}
	first := messages.Get("0")
	if role := first.Get("role").String(); role == "system" || role == "developer" {
		if content := first.Get("content"); content.Type == gjson.String {
			return sjson.SetBytes(body, "messages.0.content", joinText(content.String(), block))
		}
	}
	msgs := make([]any, 0, len(messages.Array())+1)
	msgs = append(msgs, map[string]string{"role": "system", "content": block})
	for _, m := range messages.Array() {
		msgs = append(msgs, rawJSON(m.Raw))
	}
	return sjson.SetBytes(body, "messages", msgs)
}

func injectResponses(body []byte, block string) ([]byte, error) {
	instructions := gjson.GetBytes(body, "instructions")
	switch {
	case !instructions.Exists() || instructions.Type == gjson.Null:
		return sjson.SetBytes(body, "instructions", block)
	case instructions.Type == gjson.String:
		return sjson.SetBytes(body, "instructions", joinText(instructions.String(), block))
	}
	return nil, errUnsupportedShape
}

// injectFirstUser puts reminder ahead of the first user message's text, where
// agents like Claude Code carry their own harness context. The conversation's
// first message is the same on every turn, so the edit is too: the cached
// prefix is broken only on the session's first request.
func injectFirstUser(body []byte, d dialect, reminder string) ([]byte, error) {
	path, textType := "messages", "text"
	if d == dialectResponses {
		path, textType = "input", "input_text"
	}
	items := gjson.GetBytes(body, path)
	if !items.IsArray() {
		return nil, errUnsupportedShape
	}
	for i, m := range items.Array() {
		if m.Get("role").String() != "user" {
			continue
		}
		at := fmt.Sprintf("%s.%d.content", path, i)
		content := m.Get("content")
		switch {
		case content.Type == gjson.String:
			return sjson.SetBytes(body, at, joinText(reminder, content.String()))
		case content.IsArray():
			parts := []any{map[string]string{"type": textType, "text": reminder}}
			for _, part := range content.Array() {
				parts = append(parts, rawJSON(part.Raw))
			}
			return sjson.SetBytes(body, at, parts)
		}
		return nil, errUnsupportedShape
	}
	return nil, errUnsupportedShape
}

func joinText(existing, block string) string {
	if strings.TrimSpace(existing) == "" {
		return block
	}
	return existing + "\n\n" + block
}

// rawJSON carries an existing array element through sjson unchanged.
type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) { return []byte(r), nil }

// reminderPattern matches the harness context Claude Code prepends to user
// messages; it is not the user's task.
var reminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

const maxTaskChars = 2000

// taskText derives the retrieval query from the conversation's first user
// message with text, cleaned of harness context and injected guidelines.
func taskText(messages []messageText) string {
	for _, m := range messages {
		if m.role != "user" {
			continue
		}
		text := clean(m.content)
		if text == "" {
			continue
		}
		if len(text) > maxTaskChars {
			text = text[:maxTaskChars]
		}
		return text
	}
	return ""
}

type messageText struct{ role, content string }
