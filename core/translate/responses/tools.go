package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// origin is the Request.Origin of this protocol (rule R2a): it maps each
// flat tool name to the tool that the agent declared.
type origin struct {
	tools map[string]toolRef
}

// toolRef is one tool that the agent declared. kind is "function" or
// "custom".
type toolRef struct {
	kind, namespace, name string
}

// customSchema is the input schema of a custom tool on a server that has
// only function tools (rule R8).
var customSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`)

// maxName is the longest tool name that Chat and Anthropic servers accept.
const maxName = 64

// flatName gives the tool name for the target server (rule R9).
func flatName(namespace, name string) string {
	flat := name
	if namespace != "" && namespace != "functions" {
		flat = namespace + "__" + name
	}
	if len(flat) <= maxName && strings.IndexFunc(flat, invalidNameRune) < 0 {
		return flat
	}
	var b strings.Builder
	for i := 0; i < len(flat) && b.Len() < 55; i++ {
		if c := flat[i]; c < 0x80 && !invalidNameRune(rune(c)) {
			b.WriteByte(c)
		}
	}
	sum := sha256.Sum256([]byte(flat))
	return b.String() + "_" + hex.EncodeToString(sum[:4])
}

func invalidNameRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
}

// tools adds the tools of a tools list (rule R8). namespace is "" at the
// top level.
func (d *decoder) tools(raw json.RawMessage, namespace string) error {
	var items []json.RawMessage
	if err := json.Unmarshal(orNull(raw), &items); err != nil {
		return fmt.Errorf("tools: %w", err)
	}
	for i, item := range items {
		m, err := object(item)
		if err != nil {
			return fmt.Errorf("tools[%d]: %w", i, err)
		}
		var kind, name, desc string
		var params, sub json.RawMessage
		var format struct{ Syntax, Definition string }
		if err := errors.Join(take(m, "type", &kind), take(m, "name", &name), take(m, "description", &desc)); err != nil {
			return fmt.Errorf("tools[%d]: %w", i, err)
		}
		switch {
		case kind == "function":
			takeRaw(m, "parameters", &params)
		case kind == "custom":
			if err := take(m, "format", &format); err != nil {
				return fmt.Errorf("tools[%d]: %w", i, err)
			}
			if format.Definition != "" {
				desc = strings.TrimPrefix(desc+"\n\nInput format ("+format.Syntax+"):\n"+format.Definition, "\n\n")
			}
			params = customSchema
		case kind == "namespace" && namespace == "":
			takeRaw(m, "tools", &sub)
			if err := d.tools(sub, name); err != nil {
				return err
			}
			continue
		default:
			d.drop(kind)
			continue
		}
		// strict, defer_loading and any other member have no place.
		for k := range m {
			d.drop(k)
		}
		if err := d.addTool(toolRef{kind: kind, namespace: namespace, name: name}, desc, params); err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) addTool(ref toolRef, desc string, schema json.RawMessage) error {
	flat := flatName(ref.namespace, ref.name)
	if prev, ok := d.origin.tools[flat]; ok {
		return refuse(fmt.Sprintf("Cortex cannot send two tools with the same name %q: %s and %s", flat, prev, ref))
	}
	d.origin.tools[flat] = ref
	d.req.Tools = append(d.req.Tools, pivot.Tool{Name: flat, Description: desc, InputSchema: schema})
	return nil
}

// String names the tool as the agent declared it.
func (r toolRef) String() string {
	if r.namespace == "" {
		return strconv.Quote(r.name)
	}
	return strconv.Quote(r.namespace + "." + r.name)
}
