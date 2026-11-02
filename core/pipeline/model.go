package pipeline

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RequestModel is the model the request body asks for: its top-level "model"
// string, exactly as SetRequestModel would find and replace it. ok is false where
// SetRequestModel would refuse the body — none, not valid JSON, no top-level
// string "model", an empty one, or "model" named more than once in any letter
// case.
//
// A plugin that decides on the model reads it here rather than parsing the body
// itself, so the name it decides on is the name SetRequestModel replaces.
func (c *Context) RequestModel() (name string, ok bool) {
	name, err := requestModel(c.Body)
	return name, err == nil
}

// SetRequestModel changes which model serves this request. It is the contract for
// every plugin that does — a router, a downgrader, an A/B test — and the only way
// such a plugin should do it: not by editing the body itself, and not by writing
// the inference extension, which belongs to the parser.
//
// It replaces the body's top-level "model" value in place and changes no other
// byte. A prompt cache matches an exact prefix, so re-encoding the body would cost
// the client its cache. The new body goes through SetBody, which records
// modify/body_rewritten and the framework's body-mutation event as for any
// rewrite.
//
// When the inference parser has built Extensions.Inference, Model becomes name —
// so settlement prices the model the request was sent for, and agentop shows it —
// and RequestedModel keeps the name the body carried before the first change, the
// client's. RequestedModel is cleared when a later change returns to that name. With
// no extension, none is created: the parser leaves it nil on purpose for a request
// it could not read.
//
// The framework records modify/model_rewritten with "from" and "to" in Details.
// Under on_error: observe nothing changes, the body or the extension, and both
// records are shadows, so plugin code looks the same under enforce and observe.
//
// A name equal to the current model changes nothing and records nothing.
//
// Refused, with nothing changed and nothing recorded, from a plugin that does not
// declare WritesRequestBody, outside OnRequest, and for a body RequestModel would
// not read.
func (c *Context) SetRequestModel(name string) error {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.SetRequestModel during OnFinish — refused (the request is already sent)",
			"plugin", c.currentPlugin)
		return errors.New("pipeline: SetRequestModel refused in OnFinish: the request has already been sent")
	}
	if c.currentPhase != InvocationPhaseRequest {
		slog.Warn("pipeline: plugin called pctx.SetRequestModel outside OnRequest — refused",
			"plugin", c.currentPlugin, "phase", c.currentPhase)
		return fmt.Errorf("pipeline: SetRequestModel refused in phase %q: it is accepted only from OnRequest", c.currentPhase)
	}
	if !c.currentMayWriteRequestBody {
		return fmt.Errorf("pipeline: plugin %q called SetRequestModel without declaring WritesRequestBody", c.currentPlugin)
	}
	from, err := requestModel(c.Body)
	if err != nil {
		return err
	}
	if from == name {
		return nil
	}
	body, err := sjson.SetBytes(c.Body, "model", name)
	if err != nil {
		return fmt.Errorf("pipeline: SetRequestModel: %w", err)
	}
	details := map[string]string{"from": from, "to": name}
	// OnFinish is refused above, so a write that did not take effect is a shadow.
	if !c.SetBody(body) {
		c.Record(Invocation{Action: ActionModify, Reason: "model_rewritten", Shadow: true, Details: details})
		return nil
	}
	if ext := c.Extensions.Inference; ext != nil {
		if ext.RequestedModel == "" {
			ext.RequestedModel = from
		}
		ext.Model = name
		if ext.RequestedModel == name {
			ext.RequestedModel = ""
		}
	}
	c.Record(Invocation{Action: ActionModify, Reason: "model_rewritten", Details: details})
	return nil
}

// requestModel is body's top-level "model" string. Its errors name the problem and
// quote nothing from the body, which can carry a prompt.
//
// A body naming "model" twice is refused, not read: gjson and sjson take the first
// key spelled exactly "model", while encoding/json — the parser, and most servers —
// takes the last key that matches it in any letter case. So "Model" and "MODEL"
// count as a second "model", and a change to one would leave the other serving.
// Keys are compared decoded, so one spelled with a JSON escape counts too.
//
// The value is read only from the key spelled exactly "model", the one sjson
// replaces: for a body whose only match is "Model", sjson would add a "model"
// beside it rather than change it, so that body names no model to change.
//
// An empty "model" is refused too: RequestedModel cannot hold the client's name
// when that name is empty, so a second change would record the first one's.
func requestModel(body []byte) (string, error) {
	if !gjson.ValidBytes(body) {
		return "", errors.New("pipeline: the request body is not JSON, so it names no model to change")
	}
	var model gjson.Result
	exact, anyCase := 0, 0
	// Zero-copy: ParseBytes is Parse(string(body)) — a full copy of every routed
	// request just to scan its top-level keys. The alias is safe because gjson
	// never writes the string it parses, the body is not written concurrently on
	// the request path, and nothing that outlives the scan points into it: the
	// only string that escapes, below, is cloned.
	gjson.Parse(unsafe.String(unsafe.SliceData(body), len(body))).ForEach(func(key, value gjson.Result) bool {
		if key.Type != gjson.String || !strings.EqualFold(key.String(), "model") {
			return true
		}
		anyCase++
		if key.String() == "model" {
			model = value
			exact++
		}
		return true
	})
	switch {
	case anyCase > 1:
		return "", errors.New(`pipeline: the request body names "model" more than once, in any letter case, so which one is served is not clear`)
	case exact == 0:
		return "", errors.New(`pipeline: the request body has no top-level "model" to change`)
	case model.Type != gjson.String:
		return "", errors.New(`pipeline: the request body's "model" is not a string`)
	case model.String() == "":
		return "", errors.New(`pipeline: the request body's "model" is empty, so it names no model to change`)
	}
	// Cloned off gjson's parse copy of the body: Result.String() is a substring
	// of that copy, and a stored name (RequestedModel, invocation details) would
	// pin the whole body.
	return strings.Clone(model.String()), nil
}
