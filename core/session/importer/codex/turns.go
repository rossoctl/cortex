package codex

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/project"
	"github.com/rossoctl/cortex/core/session"
)

// responsesPath is the path the live parser reads a Codex (Responses API) body for.
const responsesPath = "/v1/responses"

// line is one rollout line.
type line struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// usage is Codex's TokenUsage. input_tokens includes cached_input_tokens.
type usage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// outputTypes are the response_item types that the model makes. A "message" is output only
// when its role is assistant.
var outputTypes = map[string]bool{
	"reasoning": true, "function_call": true, "custom_tool_call": true, "local_shell_call": true,
	"web_search_call": true, "tool_search_call": true, "image_generation_call": true,
}

type fileResult struct {
	events  []model.SessionEvent
	skipped int
}

// parser holds what the rollouts of one session share.
type parser struct {
	home     string
	projects map[string]string // cwd -> project label
	in       session.Interner  // interns each event when it is made, as the claude source does
}

// file parses one rollout. thread is the file's thread id; subagent is "" for the root.
func (p *parser) file(path, thread, subagent string) (fileResult, error) {
	f, err := openRollout(path)
	if err != nil {
		return fileResult{}, err
	}
	defer f.Close()
	var res fileResult
	role := model.AgentRoleMain
	if subagent != "" {
		role = model.AgentRoleSubagent
	}

	conv := []json.RawMessage{}
	var out []json.RawMessage
	var mdl, cwd, version string
	var inAt, prevAt, firstOutAt, lastOutAt time.Time
	n := 1

	closeTurn := func(at time.Time, u *usage) {
		if len(out) == 0 {
			return
		}
		if at.IsZero() {
			at = lastOutAt
		}
		turnOut := out
		out = nil
		reqBody, _ := json.Marshal(struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
		}{mdl, conv})
		req := inference.ParseRequest(responsesPath, nil, reqBody)
		if req == nil {
			res.skipped++
			return
		}
		req.AgentRole = role
		respBody, _ := json.Marshal(struct {
			Model  string            `json:"model"`
			Output []json.RawMessage `json:"output"`
			Usage  *responsesUsage   `json:"usage,omitempty"`
		}{mdl, turnOut, toResponsesUsage(u)})
		resp := *req
		inference.ReadResponse(responsesPath, false, respBody, &resp)
		if th := thinking(turnOut); th.Blocks > 0 {
			resp.ResponseThinking = &th
		}

		reqAt := inAt
		if reqAt.IsZero() {
			reqAt = firstOutAt
		}
		if reqAt.Before(prevAt) {
			reqAt = prevAt
		}
		rid := thread + ":" + strconv.Itoa(n)
		client := func() *model.EventClient { return &model.EventClient{Name: agentName, Version: version} }
		proj := p.project(cwd)
		res.events = append(res.events,
			model.SessionEvent{At: reqAt, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: rid,
				Inference: req, Client: client(), Project: proj, Subagent: subagent},
			model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionResponse, RequestID: rid,
				Inference: &resp, Client: client(), Project: proj, Subagent: subagent})
		for i := len(res.events) - 2; i < len(res.events); i++ {
			p.in.InternEvent(&res.events[i])
		}
		prevAt = at
		conv = append(conv, turnOut...)
		n++
	}

	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		if raw = bytes.TrimSpace(raw); len(raw) > 0 {
			var l line
			if json.Unmarshal(raw, &l) != nil || l.Type == "" {
				res.skipped++
			} else {
				switch l.Type {
				case "session_meta":
					var m struct {
						Cwd        string `json:"cwd"`
						CLIVersion string `json:"cli_version"`
					}
					_ = json.Unmarshal(l.Payload, &m)
					cwd, version = m.Cwd, m.CLIVersion
				case "turn_context":
					var c struct {
						Model string `json:"model"`
						Cwd   string `json:"cwd"`
					}
					_ = json.Unmarshal(l.Payload, &c)
					mdl, cwd = cmp.Or(c.Model, mdl), cmp.Or(c.Cwd, cwd)
				case "response_item":
					var it struct {
						Type string `json:"type"`
						Role string `json:"role"`
					}
					_ = json.Unmarshal(l.Payload, &it)
					if outputTypes[it.Type] || (it.Type == "message" && it.Role == "assistant") {
						if len(out) == 0 {
							firstOutAt = l.Timestamp
						}
						out = append(out, l.Payload)
						lastOutAt = l.Timestamp
						break
					}
					closeTurn(time.Time{}, nil)
					conv = append(conv, l.Payload)
					inAt = l.Timestamp
				case "event_msg":
					var e struct {
						Type string `json:"type"`
						Info *struct {
							LastTokenUsage *usage `json:"last_token_usage"`
						} `json:"info"`
					}
					// A token_count with no info, or with no open turn (a rate-limit update, or a
					// second one for the same turn), makes no turn.
					if json.Unmarshal(l.Payload, &e) == nil && e.Type == "token_count" && e.Info != nil {
						closeTurn(l.Timestamp, e.Info.LastTokenUsage)
					}
				case "token_usage_record":
					var rec struct {
						Usage *usage `json:"usage"`
					}
					if json.Unmarshal(l.Payload, &rec) == nil {
						closeTurn(l.Timestamp, rec.Usage)
					}
				case "compacted":
					closeTurn(time.Time{}, nil)
					var c struct {
						ReplacementHistory []json.RawMessage `json:"replacement_history"`
					}
					_ = json.Unmarshal(l.Payload, &c)
					conv = append([]json.RawMessage{}, c.ReplacementHistory...)
				}
			}
		}
		if rerr != nil {
			closeTurn(time.Time{}, nil)
			if errors.Is(rerr, io.EOF) {
				return res, nil
			}
			return res, rerr
		}
	}
}

// responsesUsage is the usage block of a Responses API response.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func toResponsesUsage(u *usage) *responsesUsage {
	if u == nil {
		return nil
	}
	r := &responsesUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
	r.InputTokensDetails.CachedTokens = u.CachedInputTokens
	r.InputTokensDetails.CacheWriteTokens = u.CacheWriteInputTokens
	r.OutputTokensDetails.ReasoningTokens = u.ReasoningOutputTokens
	return r
}

// thinking counts the reasoning items of one turn's output.
func thinking(out []json.RawMessage) model.InferenceThinking {
	var th model.InferenceThinking
	for _, raw := range out {
		var it struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			EncryptedContent string `json:"encrypted_content"`
		}
		_ = json.Unmarshal(raw, &it)
		if it.Type != "reasoning" {
			continue
		}
		text := 0
		for _, s := range it.Summary {
			text += len(s.Text)
		}
		for _, c := range it.Content {
			text += len(c.Text)
		}
		th.Add("thinking", text, len(it.EncryptedContent), 0)
	}
	return th
}

func (p *parser) project(cwd string) string {
	if cwd == "" {
		return ""
	}
	label, ok := p.projects[cwd]
	if !ok {
		label = project.Label(project.Root(cwd, p.home), p.home)
		p.projects[cwd] = label
	}
	return label
}
