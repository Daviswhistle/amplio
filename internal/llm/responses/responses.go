// SPDX-License-Identifier: Apache-2.0
// Package responses implements native OpenAI Responses and ChatGPT plan calls.
package responses

import (
	"amplio/internal/llm"
	"amplio/internal/providerstate"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const outputKey = "openai_responses.output_items"

type Provider struct {
	manager     *providerstate.Manager
	mode, model string
	maxTokens   int
	args        url.Values
	request     func(context.Context, string, string, string, []byte) (*http.Response, error)
	pause       func(context.Context, time.Duration) error
}

func New(m *providerstate.Manager, mode, model string, maxTokens int, args url.Values) (llm.Provider, error) {
	if mode != "api" && mode != "chatgpt" {
		return nil, errors.New("invalid OpenAI authentication mode")
	}
	if model == "" {
		return nil, errors.New("model is required")
	}
	copied := url.Values{}
	for k, v := range args {
		switch k {
		case "reasoning.effort", "reasoning.summary", "text.verbosity":
			if len(v) != 1 {
				return nil, fmt.Errorf("model argument %s must occur once", k)
			}
			copied[k] = append([]string(nil), v...)
		default:
			return nil, fmt.Errorf("unsupported Responses argument %q", k)
		}
	}
	return &Provider{manager: m, mode: mode, model: model, maxTokens: maxTokens, args: copied, request: m.Request, pause: pause}, nil
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (p *Provider) ModelID() string { return p.model }
func (p *Provider) MaxTokens() int  { return p.maxTokens }
func imageParts(text string, images []llm.Attachment) any {
	if len(images) == 0 {
		return text
	}
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": text})
	}
	for _, a := range images {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:" + a.MimeType + ";base64," + a.Base64Data})
	}
	return parts
}
func (p *Provider) body(req llm.Request) ([]byte, error) {
	input := []any{}
	instructions := []string{req.SystemPrompt}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			instructions = append(instructions, m.Content)
		case llm.RoleUser:
			input = append(input, map[string]any{"role": "user", "content": imageParts(m.Content, m.Attachments)})
		case llm.RoleAssistant:
			if raw, ok := m.ProviderExtra[outputKey]; ok {
				b, e := json.Marshal(raw)
				if e != nil {
					return nil, e
				}
				var items []json.RawMessage
				if json.Unmarshal(b, &items) != nil {
					return nil, errors.New("invalid persisted Responses output")
				}
				if len(items) > 0 {
					for _, item := range items {
						input = append(input, item)
					}
					continue
				}
			}
			if m.Content != "" {
				input = append(input, map[string]any{"role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Name == "" || !json.Valid([]byte(tc.Arguments)) {
					return nil, errors.New("invalid historical tool call")
				}
				item := map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments}
				if p.mode == "chatgpt" {
					item["namespace"] = "amplio"
				}
				input = append(input, item)
			}
		case llm.RoleToolResult:
			if m.ToolCallID == "" {
				return nil, errors.New("tool result is missing its call ID")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			if len(m.Attachments) > 0 {
				input = append(input, map[string]any{"role": "user", "content": imageParts("", m.Attachments)})
			}
		default:
			return nil, fmt.Errorf("unsupported message role %q", m.Role)
		}
	}
	body := map[string]any{"model": p.model, "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}}
	text := strings.TrimSpace(strings.Join(instructions, "\n\n"))
	if text != "" {
		body["instructions"] = text
	}
	if len(req.Tools) > 0 {
		tools := []any{}
		for _, t := range req.Tools {
			schema := t.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": schema, "strict": false})
		}
		if p.mode == "chatgpt" {
			body["tools"] = []any{map[string]any{"type": "namespace", "name": "amplio", "description": "Amplio local tools", "tools": tools}}
		} else {
			body["tools"] = tools
		}
		if req.DisableToolCalls {
			body["tool_choice"] = "none"
		}
	}
	if p.mode == "api" {
		cap := req.MaxTokens
		if cap <= 0 {
			cap = p.maxTokens
		}
		if cap > 0 {
			body["max_output_tokens"] = cap
		}
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		}
	}
	for k, v := range p.args {
		parts := strings.SplitN(k, ".", 2)
		block, ok := body[parts[0]].(map[string]any)
		if !ok {
			block = map[string]any{}
			body[parts[0]] = block
		}
		block[parts[1]] = v[0]
	}
	return json.Marshal(body)
}
func retryable(e *providerstate.RemoteError) bool {
	switch e.Code {
	case "insufficient_quota", "subscription_sharing_usage_limit_exceeded", "subscription_sharing_user_not_eligible", "subscription_sharing_invalid_user", "invalid_api_key":
		return false
	}
	return e.Status == 429 || e.Status == 500 || e.Status == 502 || e.Status == 503 || e.Status == 504
}
func (p *Provider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	body, err := p.body(req)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, e := p.request(ctx, p.mode, "POST", "/responses", body)
		if e != nil {
			return nil, e
		}
		if resp.StatusCode == 200 {
			s := &stream{body: resp.Body, scanner: bufio.NewScanner(resp.Body), calls: map[string]llm.ToolCallStart{}}
			s.scanner.Buffer(make([]byte, 65536), 8<<20)
			return s, nil
		}
		remote := providerstate.DecodeError(resp)
		_ = resp.Body.Close()
		if attempt == 3 || !retryable(remote) {
			return nil, remote
		}
		delay := time.Duration(1<<attempt) * time.Second
		if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
		if delay > 20*time.Second {
			delay = 20 * time.Second
		}
		if e = p.pause(ctx, delay); e != nil {
			return nil, e
		}
	}
	return nil, errors.New("OpenAI request failed")
}
func (p *Provider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	s, err := p.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	for s.Next() {
	}
	if s.Err() != nil {
		return nil, s.Err()
	}
	return s.Response(), nil
}

type stream struct {
	body     io.ReadCloser
	scanner  *bufio.Scanner
	event    llm.StreamEvent
	response *llm.Response
	err      error
	done     bool
	calls    map[string]llm.ToolCallStart
}

func (s *stream) Close()                  { _ = s.body.Close() }
func (s *stream) Event() llm.StreamEvent  { return s.event }
func (s *stream) Response() *llm.Response { return s.response }
func (s *stream) Err() error              { return s.err }
func (s *stream) frame() ([]byte, error) {
	var data []string
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if line == "" {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, io.EOF
}
func (s *stream) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	for {
		data, err := s.frame()
		if err != nil {
			s.err = errors.New("OpenAI stream ended before response.completed")
			return false
		}
		if string(data) == "[DONE]" {
			s.err = errors.New("OpenAI stream ended without response.completed")
			return false
		}
		var e struct {
			Type, Delta string
			ItemID      string `json:"item_id"`
			Item        json.RawMessage
			Response    json.RawMessage
			Code        string
		}
		if json.Unmarshal(data, &e) != nil {
			s.err = errors.New("invalid OpenAI stream event")
			return false
		}
		s.event = llm.StreamEvent{}
		switch e.Type {
		case "response.output_text.delta":
			s.event.DeltaText = e.Delta
			return true
		case "response.reasoning_summary_text.delta":
			s.event.DeltaThoughts = e.Delta
			return true
		case "response.output_item.added":
			var item struct {
				Type, ID, Name string
				CallID         string `json:"call_id"`
			}
			if json.Unmarshal(e.Item, &item) != nil {
				s.err = errors.New("invalid output item")
				return false
			}
			if item.Type == "function_call" {
				if item.CallID == "" || item.ID == "" || item.Name == "" {
					s.err = errors.New("tool call has no stable identity")
					return false
				}
				tc := llm.ToolCallStart{ID: item.CallID, Name: strings.TrimPrefix(item.Name, "amplio.")}
				s.calls[item.ID] = tc
				s.event.ToolCallStart = &tc
				return true
			}
		case "response.function_call_arguments.delta":
			tc, ok := s.calls[e.ItemID]
			if !ok {
				s.err = errors.New("arguments arrived before their tool call")
				return false
			}
			s.event.ToolCallDelta = &llm.ToolCallDelta{ID: tc.ID, ArgumentsDelta: e.Delta}
			return true
		case "response.completed":
			s.response, s.err = parseResponse(e.Response)
			s.done = true
			return false
		case "response.failed", "response.incomplete", "error":
			var r struct {
				Error             struct{ Code string }
				IncompleteDetails struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			}
			_ = json.Unmarshal(e.Response, &r)
			code := r.Error.Code
			if code == "" {
				code = e.Code
			}
			if code == "" {
				code = r.IncompleteDetails.Reason
			}
			if code == "" {
				code = e.Type
			}
			s.err = fmt.Errorf("OpenAI generation did not complete (%s)", safeEventCode(code))
			return false
		}
	}
}
func safeEventCode(s string) string {
	if len(s) > 100 {
		return "request_failed"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return "request_failed"
		}
	}
	return s
}
func parseResponse(raw json.RawMessage) (*llm.Response, error) {
	var r struct {
		Status string
		Output []json.RawMessage
		Usage  struct {
			Input   int `json:"input_tokens"`
			Output  int `json:"output_tokens"`
			Total   int `json:"total_tokens"`
			Details struct {
				Cached int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		}
	}
	if json.Unmarshal(raw, &r) != nil || r.Status != "completed" {
		return nil, errors.New("invalid completed Responses payload")
	}
	out := &llm.Response{StopReason: "stop", ProviderExtra: map[string]any{outputKey: r.Output}, Usage: llm.Usage{PromptTokens: r.Usage.Input, CompletionTokens: r.Usage.Output, TotalTokens: r.Usage.Total, CacheReadTokens: r.Usage.Details.Cached}}
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}
	seen := map[string]bool{}
	for _, blob := range r.Output {
		var item struct {
			Type, Name, Arguments string
			CallID                string `json:"call_id"`
			Content               []struct{ Type, Text, Refusal string }
			Summary               []struct{ Text string }
		}
		if json.Unmarshal(blob, &item) != nil {
			return nil, errors.New("invalid response item")
		}
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					out.Content += part.Text
				}
				if part.Type == "refusal" {
					out.Refusal = &llm.Refusal{Category: "refusal", Explanation: part.Refusal}
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				out.Thoughts += part.Text
			}
		case "function_call":
			if item.CallID == "" || item.Name == "" || seen[item.CallID] || !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("invalid completed function call")
			}
			seen[item.CallID] = true
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{ID: item.CallID, Name: strings.TrimPrefix(item.Name, "amplio."), Arguments: item.Arguments})
			out.StopReason = "tool_calls"
		}
	}
	return out, nil
}
