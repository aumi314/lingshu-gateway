package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/relay/usage"
	"github.com/aumi314/lingshu/internal/store"
)

// 传输层伪装（指纹检测用，和 scripts/probe/probe.py 采官方样本的请求一致）：Claude 号池类中转只接受像 Claude Code 的请求。
// 号池的校验（docs/research/claude-probe.md 第九节，2026-11-17 在 kirisame 上二分实测）：x-stainless-* 一整套 SDK 请求头要齐、
// 要有 anthropic-version、X-Claude-Code-Session-Id 要和 metadata 里的 session_id 一致、UA 的版本号要等于计费头 cc_version 去掉最后一段。
const (
	probeClaudeUA      = "claude-cli/2.1.319 (external, cli)"
	probeClaudeBilling = "x-anthropic-billing-header: cc_version=2.1.319.781; cc_entrypoint=cli;"
	probeClaudeBeta    = "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24"
)

var probeClaudeSDKHeaders = map[string]string{
	"Anthropic-Dangerous-Direct-Browser-Access": "true", "X-Stainless-Arch": "arm64", "X-Stainless-Lang": "js", "X-Stainless-Os": "MacOS",
	"X-Stainless-Package-Version": "0.112.1", "X-Stainless-Retry-Count": "0", "X-Stainless-Runtime": "node",
	"X-Stainless-Runtime-Version": "v26.3.0", "X-Stainless-Timeout": "600",
}

// ProbeMessages 向渠道发一次 Claude 检测请求（Messages 协议、流式），和 ProbeChat 一样直接用渠道自己的地址、key 和代理：
// 不走路由、不计费、不写调用日志、不影响渠道健康状态。req.Effort 非空时开 adaptive thinking 并设推理强度。
// session 放进 metadata.user_id 的 session_id：号池按会话路由，一个检测会话像一段 Claude Code 对话。
func (h *Handler) ProbeMessages(ctx context.Context, ch *store.Channel, upstreamModel, session string, req probe.Request) (c probe.Call) {
	c.NotSent = true
	if ch.BaseURLMessages == "" {
		c.Error = "这个渠道没有配置 Messages 协议的地址"
		return c
	}
	d := h.Disguise
	if req.DisguiseSet && !req.FullDisguise {
		d = nil
	}
	var system []map[string]any
	if d.hasClaude() { // 完整伪装：真实 Claude Code 的系统提示，检测自己的系统指令接在最后
		system = d.claudeSystem(upstreamModel, req.System)
	} else {
		system = []map[string]any{
			{"type": "text", "text": probeClaudeBilling},
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
		}
		if req.System != "" {
			system = append(system, map[string]any{"type": "text", "text": req.System})
		}
	}
	var content any = d.directPrompt(req.Prompt, len(req.Tools) > 0, d.hasClaude() && len(d.Claude.Tools) > 0)
	if req.Document != "" {
		content = []map[string]any{
			{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": req.Document}},
			{"type": "text", "text": content},
		}
	}
	device := make([]byte, 32)
	rand.Read(device)
	userID, _ := json.Marshal(map[string]string{"device_id": hex.EncodeToString(device), "account_uuid": NewProbeSession(), "session_id": session})
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024 // Keep fingerprint collection compatible with scripts/probe/probe.py.
		if d.hasClaude() && d.Claude.MaxTokens > 0 {
			maxTokens = d.Claude.MaxTokens
		}
	}
	// 前面几轮的用户消息和当时发出去的一样，前缀才对得上；完整伪装下和 Claude Code 一样在最后一条消息上打缓存断点
	hasTools := d.hasClaude() && len(d.Claude.Tools) > 0
	var messages []map[string]any
	for _, t := range req.History {
		messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": d.directPrompt(t.User, len(req.Tools) > 0, hasTools)}}},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": t.Assistant}}})
	}
	if d.hasClaude() {
		blocks, ok := content.([]map[string]any)
		if !ok {
			blocks = []map[string]any{{"type": "text", "text": content}}
		}
		blocks[len(blocks)-1]["cache_control"] = map[string]any{"type": "ephemeral"}
		content = blocks
	}
	messages = append(messages, map[string]any{"role": "user", "content": content})
	body := map[string]any{"model": upstreamModel, "max_tokens": maxTokens, "stream": true, "system": system,
		"messages": messages, "metadata": map[string]any{"user_id": string(userID)}}
	if req.Effort != "" {
		body["thinking"] = map[string]any{"type": "adaptive"}
		body["output_config"] = map[string]any{"effort": req.Effort}
	}
	var tools []map[string]any
	if d.hasClaude() {
		tools = append(tools, d.Claude.Tools...)
	}
	if tools = append(tools, req.Tools...); len(tools) > 0 {
		body["tools"] = tools
	}
	raw, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ch.BaseURLMessages, "/")+upstreamPaths[usage.Messages], bytes.NewReader(raw))
	if err != nil {
		c.Error = err.Error()
		return c
	}
	hd := httpReq.Header
	hd.Set("Content-Type", "application/json")
	if d.hasClaude() { // 真实 Claude Code 的请求头（SDK 标识、beta 列表等），会话编号按检测会话、请求编号每次新生成
		for k, v := range d.Claude.Headers {
			hd.Set(k, v)
		}
		hd.Set("X-Claude-Code-Session-Id", session)
		hd.Set("X-Client-Request-Id", NewProbeSession())
	} else {
		hd.Set("Accept", "text/event-stream")
		hd.Set("User-Agent", probeClaudeUA)
		hd.Set("X-App", "cli")
		hd.Set("Anthropic-Beta", probeClaudeBeta)
		for k, v := range probeClaudeSDKHeaders {
			hd.Set(k, v)
		}
		hd.Set("X-Claude-Code-Session-Id", session)
	}
	setUpstreamAuth(hd, usage.Messages, ch.APIKey)

	start := time.Now()
	if ctx.Err() != nil {
		c.Error = "检测已取消"
		return c
	}
	c.NotSent = false
	tracker := newProbeUsage(usage.Messages)
	defer tracker.finish(&c, req, req.Effort, start)
	resp, err := h.clientFor(ch.ProxyURL).Do(httpReq)
	if err != nil {
		c.Error = "连接失败: " + err.Error()
		c.AttemptError = probeReadError(ctx, err, "connection_error")
		return c
	}
	defer resp.Body.Close()
	c.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		captureProbeError(resp, b, tracker)
		c.Error = fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(string(redactKey(b, ch.APIKey)), 300))
		return c
	}

	quality := req.DisguiseSet && req.FullDisguise
	qualityReasoning := req.Item == "reasoning" && quality
	type thinkingDetails struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	}
	type messageUsage struct {
		OutputTokensDetails thinkingDetails `json:"output_tokens_details"`
	}
	setThinking := func(details thinkingDetails) {
		if tokens := details.ThinkingTokens; tokens != nil {
			c.ReasoningReported = true
			if qualityReasoning || *tokens > 0 {
				c.ReasoningTokens = *tokens
			}
		}
	}
	var text strings.Builder
	type block struct {
		kind, name, signature string
		input                 strings.Builder
	}
	var blocks []*block
	at := func(i int) *block {
		for len(blocks) <= i {
			blocks = append(blocks, &block{})
		}
		return blocks[i]
	}
	parse := func(data []byte) {
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				ID    string                     `json:"id"`
				Model string                     `json:"model"`
				Usage map[string]json.RawMessage `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				Signature string `json:"signature"`
				Text      string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage messageUsage `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil || ev.Type == "" {
			return
		}
		if n := len(c.EventTypes); n == 0 || c.EventTypes[n-1] != ev.Type {
			c.EventTypes = append(c.EventTypes, ev.Type)
		}
		switch ev.Type {
		case "message_start":
			c.ID, c.Model = ev.Message.ID, ev.Message.Model
			for k := range ev.Message.Usage {
				c.UsageFields = append(c.UsageFields, k)
			}
			sort.Strings(c.UsageFields)
			// message_start 里的用量是开头的占位值（输出还没生成），推理 token 以 message_delta 的最终值为准，这里不取
		case "content_block_start":
			b := at(ev.Index)
			b.kind, b.name, b.signature = ev.ContentBlock.Type, ev.ContentBlock.Name, ev.ContentBlock.Signature
			if ev.ContentBlock.Type == "text" {
				c.RecordVisible(ev.ContentBlock.Text, time.Since(start))
				text.WriteString(ev.ContentBlock.Text)
			}
		case "content_block_delta":
			if c.FirstTokenMS == 0 {
				c.FirstTokenMS = max(int(time.Since(start).Milliseconds()), 1)
			}
			b := at(ev.Index)
			switch ev.Delta.Type {
			case "text_delta":
				c.RecordVisible(ev.Delta.Text, time.Since(start))
				text.WriteString(ev.Delta.Text)
			case "signature_delta":
				b.signature += ev.Delta.Signature
			case "input_json_delta":
				b.input.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				c.FinishReason = ev.Delta.StopReason
			}
			setThinking(ev.Usage.OutputTokensDetails)
		case "message_stop":
			c.Done = true
		case "error": // 已经回了 200 再在流里报错（上下文超长、过载等）：算请求失败，不当成模型答错
			c.AttemptError = "upstream_error"
			c.Error = truncate("上游在流中报错: "+ev.Error.Type+" "+ev.Error.Message, 300)
		}
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		reader := usage.NewSSEReader(resp.Body)
		for {
			ev, err := reader.Next()
			if err != nil {
				if !c.Done && c.AttemptError == "" {
					c.AttemptError = probeReadError(ctx, err, "stream_interrupted")
				}
				if quality && c.Error == "" && !c.Done && !errors.Is(err, io.EOF) {
					c.Error = "读取流失败: " + err.Error()
				}
				break
			}
			c.Events++
			tracker.ObserveEvent(ev.Data)
			parse(ev.Data)
		}
		if quality && !c.Done && c.Error == "" {
			c.Error = "流在正常结束前断开（没有 message_stop）"
		}
	} else {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		c.BodyComplete = err == nil && json.Valid(b)
		if err != nil {
			c.AttemptError = probeReadError(ctx, err, "read_error")
		}
		if qualityReasoning && err != nil {
			c.Error = "读取响应失败: " + err.Error()
		}
		tracker.ObserveBody(b)
		var body struct {
			ID         string `json:"id"`
			Model      string `json:"model"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type      string `json:"type"`
				Text      string `json:"text"`
				Signature string `json:"signature"`
			} `json:"content"`
			Usage messageUsage `json:"usage"`
		}
		if json.Unmarshal(b, &body) == nil {
			setThinking(body.Usage.OutputTokensDetails)
			if qualityReasoning {
				c.ID, c.Model, c.FinishReason = body.ID, body.Model, body.StopReason
				for _, content := range body.Content {
					switch content.Type {
					case "thinking":
						if content.Signature != "" {
							c.SignedThinking++
						}
					case "text":
						text.WriteString(content.Text)
					}
				}
			}
		}
	}
	c.Text = text.String()
	for _, b := range blocks {
		switch b.kind {
		case "thinking":
			if b.signature != "" {
				c.SignedThinking++
			}
		case "tool_use":
			c.ToolCalls = append(c.ToolCalls, probe.ToolCall{Name: b.name, Arguments: b.input.String()})
		}
	}
	if tracker.Seen() {
		u := tracker.Result()
		c.UsageReported = true
		_, c.InputReported = tracker.ReportedInput()
		c.CacheReported = tracker.CacheReadReported()
		c.InputTokens = u.Input + u.CacheRead + u.CacheWrite
		c.CachedTokens = u.CacheRead
		c.OutputTokens = u.Output
	}
	if ctx.Err() != nil && c.Error == "" && !c.Done {
		c.Error = "超时或被取消"
	}
	if c.Error == "" && !c.Done && c.FinishReason == "" { // 同 ProbeChat：上游中途断开记成失败
		c.Error = "流没有正常结束（没有 message_stop）"
	}
	return c
}
