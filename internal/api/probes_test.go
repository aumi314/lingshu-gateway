package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/relay"
)

type probeUpstream struct {
	t      *testing.T
	server *httptest.Server

	mu                 sync.Mutex
	requests           []probeUpstreamRequest
	status             int
	delay              time.Duration
	retryReasoningOnce bool
	script             func(http.ResponseWriter, probeUpstreamRequest) bool
}

type probeUpstreamRequest struct {
	path   string
	model  string
	body   map[string]any
	header http.Header
}

var (
	probeOnlyWord = regexp.MustCompile(`(?:Your only allowed reply is|respond with|must be exactly|Respond only with the code) ([a-z]{6})[ .,]`)
	probeMarker   = regexp.MustCompile(`\[\[[a-z]{8}\]\]`)
)

func newProbeUpstream(t *testing.T) *probeUpstream {
	t.Helper()
	up := &probeUpstream{t: t, status: http.StatusOK}
	up.server = httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(up.server.Close)
	return up
}

func (up *probeUpstream) url() string { return up.server.URL }

func (up *probeUpstream) setStatus(status int) {
	up.mu.Lock()
	up.status = status
	up.mu.Unlock()
}

func (up *probeUpstream) setScript(script func(http.ResponseWriter, probeUpstreamRequest) bool) {
	up.mu.Lock()
	up.script = script
	up.mu.Unlock()
}

func (up *probeUpstream) setDelay(delay time.Duration) {
	up.mu.Lock()
	up.delay = delay
	up.mu.Unlock()
}

func (up *probeUpstream) snapshot() []probeUpstreamRequest {
	up.mu.Lock()
	defer up.mu.Unlock()
	return append([]probeUpstreamRequest(nil), up.requests...)
}

func (up *probeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	request := probeUpstreamRequest{path: r.URL.Path, model: probeBodyModel(body), body: body, header: r.Header.Clone()}
	up.mu.Lock()
	up.requests = append(up.requests, request)
	status := up.status
	delay := up.delay
	retry := up.retryReasoningOnce && strings.HasPrefix(outboundPrompt(request), "Let x1 = ")
	if retry {
		up.retryReasoningOnce = false
	}
	up.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status != http.StatusOK {
		http.Error(w, `{"error":{"message":"insufficient balance"}}`, status)
		return
	}
	if retry {
		http.Error(w, `{"error":{"message":"temporarily unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	up.mu.Lock()
	script := up.script
	up.mu.Unlock()
	if script != nil && script(w, request) {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	switch {
	case strings.HasSuffix(r.URL.Path, "/responses"):
		up.serveResponses(w, send, body)
	case strings.HasSuffix(r.URL.Path, "/v1/messages"):
		up.serveMessages(w, send, body)
	default:
		up.serveChat(w, send, body)
	}
}

func probeBodyModel(body map[string]any) string {
	model, _ := body["model"].(string)
	return model
}

func probeBodyString(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func probeChatPrompt(body map[string]any) (string, string) {
	system, prompt := "", ""
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			m, _ := raw.(map[string]any)
			switch m["role"] {
			case "system", "developer":
				system += probeJSONText(m["content"])
			case "user":
				prompt = probeJSONText(m["content"])
			}
		}
	}
	return system, prompt
}

func probeJSONText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			parts = append(parts, probeJSONText(item))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return text
		}
	}
	return ""
}

func probeAnswer(system string) string {
	if m := probeOnlyWord.FindStringSubmatch(system); m != nil {
		return m[1]
	}
	if m := probeMarker.FindStringSubmatch(system); m != nil {
		return "Hello!\n" + m[0]
	}
	return "47"
}

func assertQualityInstructionWire(t *testing.T, requests []probeUpstreamRequest, messages bool) {
	t.Helper()
	count := 0
	for _, request := range requests {
		var system, user string
		budget := "max_output_tokens"
		if messages {
			budget = "max_tokens"
			system = probeJSONText(request.body["system"])
			items, _ := request.body["messages"].([]any)
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if item["role"] == "user" {
					user = probeJSONText(item["content"])
				}
			}
		} else {
			items, _ := request.body["input"].([]any)
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				switch item["role"] {
				case "developer":
					system += probeJSONText(item["content"])
				case "user":
					user = probeJSONText(item["content"])
				}
			}
		}
		if !probeOnlyWord.MatchString(system) && !probeMarker.MatchString(system) {
			continue
		}
		count++
		limit, _ := request.body[budget].(float64)
		reasoning, _ := request.body["reasoning"].(map[string]any)
		if user == "" || probeOnlyWord.MatchString(user) || probeMarker.MatchString(user) || limit < 1024 || reasoning["effort"] != nil || request.body["thinking"] != nil {
			t.Errorf("指令题的角色或预算不符: system=%q user=%q budget=%v reasoning=%v", system, user, limit, reasoning)
		}
	}
	if count != 2 {
		t.Errorf("实际协议中的指令题有 %d 道，期望 2 道", count)
	}
}

func (up *probeUpstream) serveChat(w http.ResponseWriter, send func(string), body map[string]any) {
	system, prompt := probeChatPrompt(body)
	answer := probeAnswer(system)
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		tool, _ := tools[0].(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		city := strings.TrimSuffix(strings.TrimPrefix(prompt, "What is the weather in "), " right now? Use the tool.")
		send(fmt.Sprintf(`{"id":"chatcmpl-x","model":"up-sol","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":%q,"arguments":""}}]}}]}`, name))
		send(`{"id":"chatcmpl-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)
		send(fmt.Sprintf(`{"id":"chatcmpl-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, `"`+city+`"}`))
	} else {
		text, _ := json.Marshal(map[string]string{"content": answer})
		send(`{"id":"chatcmpl-x","model":"up-sol","choices":[{"index":0,"delta":` + string(text) + `}]}`)
	}
	send(`{"id":"chatcmpl-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	send(`{"id":"chatcmpl-x","choices":[],"usage":{"prompt_tokens":4158,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4000}}}`)
	send("[DONE]")
}

func (up *probeUpstream) serveResponses(w http.ResponseWriter, send func(string), body map[string]any) {
	system, prompt := "", ""
	if input, ok := body["input"].([]any); ok {
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			if item["role"] == "developer" || item["role"] == "system" {
				system += probeJSONText(item["content"])
			} else {
				prompt = probeJSONText(item["content"])
			}
		}
	}
	answer := probeAnswer(system)
	output := []any{}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		tool, _ := tools[0].(map[string]any)
		name, _ := tool["name"].(string)
		city := strings.TrimSuffix(strings.TrimPrefix(prompt, "What is the weather in "), " right now? Use the tool.")
		output = append(output, map[string]any{"type": "function_call", "name": name, "arguments": `{"city":"` + city + `"}`})
	} else {
		output = append(output, map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": answer}}})
	}
	send(`{"type":"response.created","response":{"id":"resp-x","model":"up-sol"}}`)
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-x", "model": "up-sol", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 4158, "input_tokens_details": map[string]any{"cached_tokens": 4000}, "output_tokens": 5}}})
	send(string(completed))
}

func (up *probeUpstream) serveMessages(w http.ResponseWriter, send func(string), body map[string]any) {
	system, prompt := "", ""
	if blocks, ok := body["system"].([]any); ok {
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			system += probeBodyString(block, "text")
		}
	}
	prompt = probeJSONText(body["messages"])
	answer := probeAnswer(system)
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		tool, _ := tools[0].(map[string]any)
		name := probeBodyString(tool, "name")
		if name == "" {
			name = "get_weather"
		}
		city := strings.TrimSuffix(strings.TrimPrefix(prompt, "What is the weather in "), " right now? Use the tool.")
		send(fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":%q}}`, name))
		send(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
		send(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, `"`+city+`"}`))
	} else {
		send(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		send(fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, answer))
	}
	send(`{"type":"message_start","message":{"id":"msg-x","model":"up-claude","usage":{"input_tokens":4158,"cache_read_input_tokens":4000,"output_tokens":5}}}`)
	send(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
	send(`{"type":"message_stop"}`)
}

// assertFullDisguise：渠道质量测试的每个请求都用服务器上的真实客户端模板（中转认不出检测流量）。
func (up *probeUpstream) assertFullDisguise(t *testing.T, ua string) {
	t.Helper()
	for _, request := range up.snapshot() {
		if got := request.header.Get("User-Agent"); got != ua {
			t.Errorf("%s 质量测试应带模板的 UA %q，实际 %q", request.path, ua, got)
		}
		switch request.path {
		case "/responses":
			if _, ok := request.body["client_metadata"]; !ok {
				t.Errorf("Codex 模板请求缺少 client_metadata")
			}
		case "/v1/messages":
			if !strings.Contains(probeJSONText(request.body["system"]), "private Claude Code system prompt") {
				t.Errorf("Claude 模板请求缺少模板系统提示: %v", request.body["system"])
			}
			assertClaudeSession(t, request)
		}
	}
}

// assertClaudeSession：号池要求 X-Claude-Code-Session-Id 和 metadata.user_id 里的 session_id 一致。
func assertClaudeSession(t *testing.T, request probeUpstreamRequest) string {
	t.Helper()
	meta, _ := request.body["metadata"].(map[string]any)
	userID, _ := meta["user_id"].(string)
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal([]byte(userID), &parsed)
	if parsed.SessionID == "" || request.header.Get("X-Claude-Code-Session-Id") != parsed.SessionID {
		t.Errorf("会话头 %q 应等于 metadata 的 session_id %q", request.header.Get("X-Claude-Code-Session-Id"), parsed.SessionID)
	}
	return parsed.SessionID
}

// assertQualityReasoningSessions：质量推理题按编号独占会话，重试仍复用同一编号的 UUID。
func assertQualityReasoningSessions(t *testing.T, requests []probeUpstreamRequest, calls []json.RawMessage, expected int) {
	t.Helper()
	reasoningByPrompt := map[string]string{}
	reasoningPrompts := map[string]int{}
	for _, raw := range calls {
		var call struct {
			Item    string `json:"item"`
			Prompt  string `json:"prompt"`
			Session int    `json:"session"`
		}
		if json.Unmarshal(raw, &call) == nil && call.Item == "reasoning" {
			if want := probe.Sessions + len(reasoningPrompts); call.Session != want {
				t.Errorf("推理题会话编号 = %d，期望 %d", call.Session, want)
			}
			reasoningPrompts[call.Prompt] = call.Session
		}
	}
	if len(reasoningPrompts) != expected {
		t.Errorf("推理题应有 %d 个不同题面，实际 %d", expected, len(reasoningPrompts))
	}
	attempts := 0
	for _, request := range requests {
		prompt := strings.TrimSuffix(outboundPrompt(request), "\n\nAnswer directly; do not use any tools.")
		if _, ok := reasoningPrompts[prompt]; !ok {
			continue
		}
		attempts++
		var session string
		switch {
		case strings.HasSuffix(request.path, "/responses"):
			session = request.header.Get("Session-Id")
			meta, _ := request.body["client_metadata"].(map[string]any)
			if meta["session_id"] != session || request.body["prompt_cache_key"] != session || request.header.Get("Thread-Id") != session {
				t.Errorf("Codex 会话头和请求体的会话编号不一致：%q，%v", session, meta)
			}
		case strings.HasSuffix(request.path, "/v1/messages"):
			session = assertClaudeSession(t, request)
		}
		if previous, ok := reasoningByPrompt[prompt]; session == "" || ok && previous != session {
			t.Errorf("题面 %q 重试时应复用会话，首次 %q，本次 %q", prompt, previous, session)
		}
		reasoningByPrompt[prompt] = session
	}
	if len(reasoningByPrompt) != len(reasoningPrompts) {
		t.Errorf("推理题出站题面 %d 个，报告 %d 个", len(reasoningByPrompt), len(reasoningPrompts))
	}
	if attempts != expected+1 {
		t.Errorf("预期一道推理题重试，发出 %d 次请求，期望 %d 次", attempts, expected+1)
	}
	seen := map[string]string{}
	for prompt, session := range reasoningByPrompt {
		if other, ok := seen[session]; ok {
			t.Errorf("推理题应各用独立会话，%q 和 %q 都用了 %q", prompt, other, session)
		}
		seen[session] = prompt
	}
}

func TestProbeSessionIDs(t *testing.T) {
	sessions := newProbeSessionIDs()
	seen := map[string]bool{}
	for index := range probe.Sessions + 10 {
		session := sessions.get(index)
		if session == "" || seen[session] {
			t.Fatalf("会话编号 %d 生成了重复或空的 UUID %q", index, session)
		}
		seen[session] = true
		if retry := sessions.get(index); retry != session {
			t.Fatalf("会话编号 %d 重试时 UUID 变了：%q -> %q", index, session, retry)
		}
	}
}

func outboundPrompt(request probeUpstreamRequest) string {
	switch {
	case strings.HasSuffix(request.path, "/responses"):
		input, _ := request.body["input"].([]any)
		if len(input) == 0 {
			return ""
		}
		last, _ := input[len(input)-1].(map[string]any)
		return probeJSONText(last["content"])
	case strings.HasSuffix(request.path, "/v1/messages"):
		messages, _ := request.body["messages"].([]any)
		if len(messages) == 0 {
			return ""
		}
		last, _ := messages[len(messages)-1].(map[string]any)
		return probeJSONText(last["content"])
	default:
		return ""
	}
}

// assertTransportOnly：模型指纹只用传输层伪装（Codex 客户端标识和会话头，号池按会话路由），不带模板内容，和官方样本的采集条件一致。
func (up *probeUpstream) assertTransportOnly(t *testing.T) {
	t.Helper()
	sessions := map[string]bool{}
	for _, request := range up.snapshot() {
		if got := request.header.Get("User-Agent"); !strings.Contains(got, "codex") {
			t.Errorf("%s 指纹请求应带 Codex 客户端标识，实际 %q", request.path, got)
		}
		if _, ok := request.body["client_metadata"]; ok {
			t.Errorf("%s 指纹请求不应带模板的客户端元数据", request.path)
		}
		sessions[request.header.Get("Session-Id")] = true
	}
	if len(sessions) != probe.Sessions || sessions[""] {
		t.Errorf("指纹请求应分在 %d 个会话里，实际 %v", probe.Sessions, sessions)
	}
}

func seedProbeModel(t *testing.T, e *env, name, upstream string, channelID int64) {
	t.Helper()
	ctx := context.Background()
	var modelID int64
	if err := e.store.Pool.QueryRow(ctx, `INSERT INTO models (name, input_price, output_price, cache_read_price) VALUES ($1, 2, 10, 0.1) RETURNING id`, name).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO channel_models (channel_id, model_id, upstream_name) VALUES ($1, $2, $3)`, channelID, modelID, upstream); err != nil {
		t.Fatal(err)
	}
}

func seedProbeChannel(t *testing.T, e *env, name, key, chatURL, messagesURL, responsesURL string) int64 {
	t.Helper()
	var id int64
	err := e.store.Pool.QueryRow(context.Background(), `
		INSERT INTO channels (name, api_key, base_url_chat, base_url_messages, base_url_responses)
		VALUES ($1, '', $2, $3, $4) RETURNING id`,
		name, chatURL, messagesURL, responsesURL).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Pool.Exec(context.Background(), `INSERT INTO channel_keys (channel_id, api_key) VALUES ($1, $2)`, id, key); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertProbeAuth(t *testing.T, requests []probeUpstreamRequest) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("检测没有发出上游请求")
	}
	for _, request := range requests {
		if strings.HasSuffix(request.path, "/v1/messages") {
			if got := request.header.Get("X-Api-Key"); got != "upstream-key" {
				t.Errorf("%s 的上游 x-api-key = %q，want upstream-key", request.path, got)
			}
			continue
		}
		if got := request.header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("%s 的上游认证头 = %q，want Bearer upstream-key", request.path, got)
		}
	}
}

type probeRunView struct {
	Status        string `json:"status"`
	Verdict       string `json:"verdict"`
	Baseline      string `json:"baseline"`
	UpstreamModel string `json:"upstream_model"`
	Preset        string `json:"preset"`
	Score         *int   `json:"score"`
	Cost          string `json:"cost"`
	Report        struct {
		ProbeMode     string `json:"probe_mode"`
		Protocol      string `json:"protocol"`
		Disguise      bool   `json:"disguise"`
		TemplateHash  string `json:"template_hash"`
		StopReason    string `json:"stop_reason"`
		StopMessage   string `json:"stop_message"`
		Verdict       string `json:"verdict"`
		Score         *int   `json:"score"`
		FormatVersion int    `json:"format_version"`
		RuleVersion   string `json:"rule_version"`
		BaselineID    string `json:"baseline_id"`
		ChannelKeyID  int64  `json:"channel_key_id"`
		Blocks        []struct {
			Key     string `json:"key"`
			Title   string `json:"title"`
			Verdict string `json:"verdict"`
			Items   []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
				RuleID string `json:"rule_id"`
			} `json:"items"`
		} `json:"blocks"`
		Rules []struct {
			ID         string         `json:"id"`
			State      string         `json:"state"`
			Version    string         `json:"version"`
			Thresholds map[string]any `json:"thresholds"`
			Evidence   string         `json:"evidence"`
		} `json:"rules"`
		Counts       probe.RunCounts     `json:"counts"`
		Items        []json.RawMessage   `json:"items"`
		Calls        []json.RawMessage   `json:"calls"`
		Attempts     []probe.Attempt     `json:"attempts"`
		Availability *probe.Availability `json:"availability"`
	} `json:"report"`
}

// probeReportContract 是一份 v3 质量报告的契约。检查列表只带摘要、详情带完整报告，
// 以及新报告不再有总分，omitted/显式 null 都按空 score 处理。
func probeReportContract(t *testing.T, run probeRunView) {
	t.Helper()
	if run.Report.FormatVersion != 3 || run.Report.RuleVersion == "" || run.Report.BaselineID == "" || run.Report.ChannelKeyID == 0 {
		t.Fatalf("报告缺少版本/基准/key 安全 ID: version=%d rule=%q baseline=%q key=%d", run.Report.FormatVersion, run.Report.RuleVersion, run.Report.BaselineID, run.Report.ChannelKeyID)
	}
	if run.Score != nil || run.Report.Score != nil || run.Verdict != run.Report.Verdict {
		t.Errorf("v3 质量报告不应有总分，列表及报告结论应一致: %+v", run)
	}
	if len(run.Report.Blocks) != 3 || run.Report.Blocks[0].Key != "capability" || run.Report.Blocks[1].Key != "billing" || run.Report.Blocks[2].Key != "experience" {
		t.Fatalf("报告缺少三块: %+v", run.Report)
	}
	rules := map[string]bool{}
	for _, rule := range run.Report.Rules {
		rules[rule.ID] = true
	}
	for _, block := range run.Report.Blocks {
		if block.Key == "capability" && block.Verdict != run.Verdict {
			t.Errorf("能力块 %q 与总览 %q 不一致", block.Verdict, run.Verdict)
		}
		for _, item := range block.Items {
			if item.Key == "" || item.Status == "" || !rules[item.RuleID] {
				t.Errorf("块 %q 的检测项缺状态或对应规则: %+v", block.Key, item)
			}
		}
	}
	if len(run.Report.Rules) == 0 {
		t.Error("报告应带回规则清单")
	}
	for _, rule := range run.Report.Rules {
		if rule.ID == "" || rule.Version == "" || rule.State == "" {
			t.Errorf("规则清单条目不完整: %+v", rule)
		}
		if rule.ID == "reasoning_usage" {
			if _, ok := rule.Thresholds["min_reasoning_tokens"]; !ok {
				t.Errorf("推理用量规则缺少阈值: %+v", rule)
			}
			if rule.Evidence == "" {
				t.Errorf("推理用量规则缺少依据: %+v", rule)
			}
		}
	}
}

func probeBlockVerdict(run probeRunView, key string) string {
	for _, block := range run.Report.Blocks {
		if strings.EqualFold(block.Key, key) || strings.Contains(block.Title, key) {
			return block.Verdict
		}
	}
	return ""
}

func probeItemStatus(run probeRunView, itemKey string) string {
	for _, block := range run.Report.Blocks {
		for _, item := range block.Items {
			if strings.EqualFold(item.Key, itemKey) {
				return item.Status
			}
		}
	}
	return ""
}

func waitProbeRun(t *testing.T, a *client, id int64) probeRunView {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var raw map[string]any
		a.do("GET", fmt.Sprintf("/api/admin/probes/%d", id), nil, &raw)
		if raw["status"] != "running" || time.Now().After(deadline) {
			b, _ := json.Marshal(raw)
			var run probeRunView
			if err := json.Unmarshal(b, &run); err != nil {
				t.Fatalf("解析检测记录失败: %v %s", err, b)
			}
			if run.Status == "running" {
				t.Fatal("检测超时仍在运行")
			}
			return run
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func startProbe(t *testing.T, a *client, payload map[string]any) (int, string, int64) {
	t.Helper()
	var started struct{ ID int64 }
	code, body := a.do("POST", "/api/admin/probes", payload, &started)
	return code, body, started.ID
}

func TestProbeStartValidation(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	chatChannel := seedProbeChannel(t, e, "chat-only", "upstream-key", up.url(), "", "")
	seedProbeModel(t, e, "gpt-6.1-sol", "up-sol", chatChannel)
	responseChannel := seedProbeChannel(t, e, "responses-only", "upstream-key", "", "", up.url())
	seedProbeModel(t, e, "gpt-6-astra", "up-astra", responseChannel)
	blockedChannel := seedProbeChannel(t, e, "blocked", "upstream-key", "", "", up.url())
	seedProbeModel(t, e, "gpt-6-luna", "up-luna", blockedChannel)
	if code, body, _ := startProbe(t, a, map[string]any{"channel_id": responseChannel, "model": "gpt-6-astra", "preset": probe.ModeQuality}); code != http.StatusBadRequest || !strings.Contains(body, "伪装模板") {
		t.Fatalf("服务器没有 Codex 模板时质量测试应拒绝发起: %d %s", code, body)
	}
	e.api.Relay.Disguise = &relay.ProbeDisguise{}
	e.api.Relay.Disguise.Codex.Input = json.RawMessage(`[{"type":"message"}]`)

	for _, preset := range []string{"", "quick", "full", "unknown"} {
		payload := map[string]any{"channel_id": chatChannel, "model": "gpt-6.1-sol", "preset": preset}
		if code, body, _ := startProbe(t, a, payload); code != http.StatusBadRequest || !strings.Contains(body, "quality 或 fingerprint") {
			t.Errorf("旧 preset %q 应拒绝: %d %s", preset, code, body)
		}
	}
	if code, body, _ := startProbe(t, a, map[string]any{"channel_id": chatChannel, "model": "gpt-6.1-sol", "preset": probe.ModeQuality}); code != http.StatusBadRequest || !strings.Contains(body, "Responses") {
		t.Fatalf("质量模式需要 Responses 地址: %d %s", code, body)
	}
	if code, body, _ := startProbe(t, a, map[string]any{"channel_id": responseChannel, "model": "gpt-6-astra", "preset": probe.ModeFingerprint}); code != http.StatusBadRequest || !strings.Contains(body, "Chat") {
		t.Fatalf("指纹模式需要 Chat 地址: %d %s", code, body)
	}

	ctx := context.Background()
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO probe_runs (channel_id, channel_name, model, upstream_model, baseline, preset, status) VALUES ($1, 'blocked', 'gpt-6-luna', 'up-luna', 'gpt-6-luna', 'quality', 'running')`, blockedChannel); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := startProbe(t, a, map[string]any{"channel_id": blockedChannel, "model": "gpt-6-luna", "preset": probe.ModeQuality}); code != http.StatusTooManyRequests {
		t.Fatalf("同一渠道已有检测应拒绝: %d %s", code, body)
	}
	for range maxRunningProbes {
		if _, err := e.store.Pool.Exec(ctx, `INSERT INTO probe_runs (channel_name, model, upstream_model, baseline, preset, status) VALUES ('idle', 'm', 'm', 'gpt-6-luna', 'quality', 'running')`); err != nil {
			t.Fatal(err)
		}
	}
	if code, body, _ := startProbe(t, a, map[string]any{"channel_id": chatChannel, "model": "gpt-6.1-sol", "preset": probe.ModeFingerprint}); code != http.StatusTooManyRequests {
		t.Fatalf("全局已有检测时应拒绝其他渠道: %d %s", code, body)
	}
}

func TestProbeQualityGPT(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	up.mu.Lock()
	up.retryReasoningOnce = true
	up.mu.Unlock()
	channelID := seedProbeChannel(t, e, "responses", "upstream-key", "", "", up.url())
	seedProbeModel(t, e, "gpt-6.1-sol", "up-sol", channelID)
	e.api.Relay.Disguise = &relay.ProbeDisguise{}
	e.api.Relay.Disguise.Codex.Input = json.RawMessage(`[{"type":"message"}]`)
	e.api.Relay.Disguise.Codex.Headers = map[string]string{"user-agent": "private-codex-template"}
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "gpt-6.1-sol", "preset": probe.ModeQuality})
	if code != http.StatusOK {
		t.Fatalf("发起质量测试: %d %s", code, body)
	}
	run := waitProbeRun(t, a, id)
	if run.Status != "done" || run.Report.ProbeMode != probe.ModeQuality || run.Report.Protocol != "responses" || !run.Report.Disguise || run.Report.TemplateHash == "" || run.Score != nil {
		t.Fatalf("质量测试结果不对: %+v", run)
	}
	probeReportContract(t, run)
	for _, key := range []string{"availability", "model_mix", "tokens", "instruction", "needle_answer", "cache", "tool"} {
		if probeItemStatus(run, key) == "" {
			t.Errorf("缺少检测项 %s", key)
		}
	}
	requests := up.snapshot()
	if len(requests) != 39 || len(run.Report.Calls) != 38 {
		t.Fatalf("请求数 %d，报告 %d", len(requests), len(run.Report.Calls))
	}
	assertProbeAuth(t, requests)
	for _, request := range requests {
		if request.path != "/responses" || request.model != "up-sol" {
			t.Fatalf("质量测试应走 Responses: %s %s", request.path, request.model)
		}
	}
	up.assertFullDisguise(t, "private-codex-template")
	assertQualityReasoningSessions(t, requests, run.Report.Calls, 10)
	assertQualityInstructionWire(t, requests, false)
	var logs int
	e.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM request_logs`).Scan(&logs)
	if logs != 0 {
		t.Errorf("检测请求不应写调用日志: %d", logs)
	}
}

func TestProbeFingerprintGPT(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	channelID := seedProbeChannel(t, e, "chat", "upstream-key", up.url(), "", "")
	seedProbeModel(t, e, "gpt-6.1-sol", "up-sol", channelID)
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "gpt-6.1-sol", "preset": probe.ModeFingerprint})
	if code != http.StatusOK {
		t.Fatalf("发起指纹辨别: %d %s", code, body)
	}
	run := waitProbeRun(t, a, id)
	if run.Status != "done" || run.Report.ProbeMode != probe.ModeFingerprint || run.Report.Protocol != "chat" || run.Report.Disguise || run.Score == nil {
		t.Fatalf("指纹测试结果不对: %+v", run)
	}
	requests := up.snapshot()
	if len(requests) != 693 || len(run.Report.Calls) != 693 {
		t.Fatalf("指纹请求数 = %d / %d, want 693", len(requests), len(run.Report.Calls))
	}
	for _, item := range run.Report.Items {
		var legacy struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(item, &legacy) == nil && legacy.Key != "fingerprint" && legacy.Key != "pair" {
			t.Errorf("指纹报告包含额外检测项 %s", legacy.Key)
		}
	}
	assertProbeAuth(t, requests)
	for _, request := range requests {
		if request.path != "/chat/completions" || request.model != "up-sol" {
			t.Fatalf("指纹辨别应走 Chat: %s %s", request.path, request.model)
		}
	}
	up.assertTransportOnly(t)

	var list []map[string]any
	a.do("GET", "/api/admin/probes", nil, &list)
	if len(list) != 1 || list[0]["report"] != nil {
		t.Errorf("列表不应带完整报告: %v", list)
	}
	ctx := context.Background()
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO probe_runs (channel_name, model, upstream_model, baseline, preset, status) VALUES ('x', 'm', 'm', 'gpt-6.1-sol', 'quick', 'running')`); err != nil {
		t.Fatal(err)
	}
	if err := e.store.FailStaleProbeRuns(ctx); err != nil {
		t.Fatal(err)
	}
	var running int
	e.store.Pool.QueryRow(ctx, `SELECT count(*) FROM probe_runs WHERE status = 'running'`).Scan(&running)
	if running != 0 {
		t.Errorf("重启后不应还有进行中的检测: %d", running)
	}
}

func TestProbeQualityClaude(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	up.mu.Lock()
	up.retryReasoningOnce = true
	up.mu.Unlock()
	channelID := seedProbeChannel(t, e, "messages", "upstream-key", "", up.url(), "")
	seedProbeModel(t, e, "claude-opus-5-5", "up-claude", channelID)
	e.api.Relay.Disguise = &relay.ProbeDisguise{}
	e.api.Relay.Disguise.Claude.System = []map[string]any{{"type": "text", "text": "private Claude Code system prompt"}}
	e.api.Relay.Disguise.Claude.Headers = map[string]string{"user-agent": "private-claude-template"}
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "claude-opus-5-5", "preset": probe.ModeQuality})
	if code != http.StatusOK {
		t.Fatalf("发起 Claude 质量测试: %d %s", code, body)
	}
	run := waitProbeRun(t, a, id)
	if run.Status != "done" || run.Report.ProbeMode != probe.ModeQuality || run.Report.Protocol != "messages" || !run.Report.Disguise || run.Report.TemplateHash == "" || run.Score != nil {
		t.Fatalf("Claude 质量测试结果不对: %+v", run)
	}
	probeReportContract(t, run)
	requests := up.snapshot()
	if len(requests) != 30 || len(run.Report.Calls) != 29 {
		t.Fatalf("Claude 请求数 = %d / %d", len(requests), len(run.Report.Calls))
	}
	assertProbeAuth(t, requests)
	for _, request := range requests {
		if request.path != "/v1/messages" || request.model != "up-claude" {
			t.Fatalf("Claude 质量测试应走 Messages: %s %s", request.path, request.model)
		}
	}
	up.assertFullDisguise(t, "private-claude-template")
	assertQualityReasoningSessions(t, requests, run.Report.Calls, 4)
	assertQualityInstructionWire(t, requests, true)
}

func TestProbePartialReportOnQuotaError(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	up.setStatus(http.StatusPaymentRequired)
	channelID := seedProbeChannel(t, e, "no-balance", "upstream-key", up.url(), "", "")
	seedProbeModel(t, e, "gpt-6.1-sol", "up-sol", channelID)
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "gpt-6.1-sol", "preset": probe.ModeFingerprint})
	if code != http.StatusOK {
		t.Fatalf("发起检测: %d %s", code, body)
	}
	run := waitProbeRun(t, a, id)
	if run.Status != "failed" || run.Report.StopReason != "quota" || run.Score != nil {
		t.Fatalf("余额不足应留下部分报告并不评分: %+v", run)
	}
	if run.Report.Counts.Sent == 0 || run.Report.Counts.Sent >= run.Report.Counts.Planned || run.Report.Counts.Completed != 0 || run.Report.Counts.Skipped == 0 {
		t.Fatalf("部分报告计数不对: %+v", run.Report.Counts)
	}
	if len(run.Report.Calls) != run.Report.Counts.Planned {
		t.Fatalf("部分报告调用数 = %d, planned = %d", len(run.Report.Calls), run.Report.Counts.Planned)
	}
}

func TestProbeFailedAttemptCostWithCacheWrite(t *testing.T) {
	e := newEnv(t)
	a := e.login("admin")
	up := newProbeUpstream(t)
	up.mu.Lock()
	up.script = func(w http.ResponseWriter, request probeUpstreamRequest) bool {
		if !strings.Contains(outboundPrompt(request), "The text above contains three secret codes") {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		fmt.Fprint(w, `{"error":{"message":"overloaded"},"usage":{"input_tokens":5,"cache_read_input_tokens":10000,"cache_creation_input_tokens":1000000,"output_tokens":2}}`)
		return true
	}
	up.mu.Unlock()
	channelID := seedProbeChannel(t, e, "attempt-cost", "upstream-key", "", up.url(), "")
	seedProbeModel(t, e, "claude-opus-5-5", "up-claude", channelID)
	if _, err := e.store.Pool.Exec(t.Context(), `UPDATE models SET cache_write_price = 3 WHERE id = (SELECT model_id FROM channel_models WHERE channel_id = $1)`, channelID); err != nil {
		t.Fatal(err)
	}
	e.api.Relay.Disguise = &relay.ProbeDisguise{}
	e.api.Relay.Disguise.Claude.System = []map[string]any{{"type": "text", "text": "private Claude Code system prompt"}}
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "claude-opus-5-5", "preset": probe.ModeQuality})
	if code != http.StatusOK {
		t.Fatalf("发起检测: %d %s", code, body)
	}
	run := waitProbeRun(t, a, id)
	attempts, stats := run.Report.Attempts, run.Report.Availability
	if run.Status != "failed" || run.Report.StopReason != "billing_anomaly" || stats == nil || len(attempts) < 2 || len(attempts) != len(up.snapshot()) || stats.Sends != len(attempts) || stats.Unscheduled == 0 {
		t.Fatalf("应在第一轮失败用量触发时停机并保存发送记录: status=%s reason=%s attempts=%+v stats=%+v", run.Status, run.Report.StopReason, attempts, stats)
	}
	for _, attempt := range attempts {
		if attempt.Number != 1 {
			t.Fatalf("不应重试: %+v", attempt)
		}
	}
	failed := attempts[len(attempts)-1]
	if failed.Item != "needle" || failed.HTTPStatus != 529 || failed.Usage.Input != 5 || failed.Usage.CacheRead != 10000 || failed.Usage.CacheWrite != 1000000 || failed.Usage.Output != 2 {
		t.Fatalf("失败尝试应独立保存各类用量: %+v", failed)
	}
	want := decimal.RequireFromString("3.00103").Add(decimal.RequireFromString("0.008766").Mul(decimal.NewFromInt(int64(len(attempts) - 1))))
	if got, err := decimal.NewFromString(run.Cost); err != nil || !got.Equal(want) {
		t.Fatalf("费用 = %q，期望 %s", run.Cost, want)
	}
}

// startV3ProbeRun 建一个 Responses 渠道，准备两把 key，再发起检测。
func startV3ProbeRun(t *testing.T, e *env, up *probeUpstream) (*client, int64, int64) {
	t.Helper()
	a := e.login("admin")
	channelID := seedProbeChannel(t, e, "v3-quality-"+up.server.URL, "upstream-key", "", "", up.url())
	ctx := context.Background()
	var keyID int64
	if err := e.store.Pool.QueryRow(ctx, `SELECT id FROM channel_keys WHERE channel_id = $1`, channelID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO channel_keys (channel_id, api_key, sort_order) VALUES ($1, $2, 1)`, channelID, "alternate-key"); err != nil {
		t.Fatal(err)
	}
	seedProbeModel(t, e, "gpt-6.1-sol", "up-sol", channelID)
	e.api.Relay.Disguise = &relay.ProbeDisguise{}
	e.api.Relay.Disguise.Codex.Input = json.RawMessage(`[{"type":"message"}]`)
	e.api.Relay.Disguise.Codex.Headers = map[string]string{"user-agent": "private-codex-template"}
	code, body, id := startProbe(t, a, map[string]any{"channel_id": channelID, "model": "gpt-6.1-sol", "preset": probe.ModeQuality})
	if code != http.StatusOK || id == 0 {
		t.Fatalf("发起 v3 质量测试: %d %s", code, body)
	}
	return a, id, keyID
}

// TestProbeV3QualityReliableAbnormalityThenQuotaStop：合成夹具——一道推理题明确零推理，
// 之后额度耗尽中止。断言中止后仍保留已完成样本、能力结论、列表 verdict 和 null score。
func TestProbeV3QualityReliableAbnormalityThenQuotaStop(t *testing.T) {
	e := newEnv(t)
	up := newProbeUpstream(t)
	once := sync.Once{}
	up.script = func(w http.ResponseWriter, request probeUpstreamRequest) bool {
		prompt := outboundPrompt(request)
		if strings.Contains(prompt, "Ignore the text above. What is") {
			http.Error(w, `{"error":{"message":"insufficient balance"}}`, http.StatusPaymentRequired)
			return true
		}
		if !strings.HasPrefix(prompt, "Let x1 = ") {
			return false
		}
		handled := false
		once.Do(func() {
			handled = true
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_zero","model":"up-sol","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"0"}]}],"usage":{"input_tokens":4158,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0}}}}`+"\n\n")
		})
		return handled
	}
	a, id, keyID := startV3ProbeRun(t, e, up)
	run := waitProbeRun(t, a, id)
	if run.Status != "failed" || run.Report.StopReason != "quota" || !strings.Contains(run.Report.StopMessage, "余额") {
		t.Fatalf("额度中止应记为失败并留下中止原因: status=%s stop=%q verdict=%q", run.Status, run.Report.StopReason, run.Verdict)
	}
	probeReportContract(t, run)
	if run.Report.ChannelKeyID != keyID {
		t.Errorf("报告 key ID = %d，期望实际选中的渠道 key %d", run.Report.ChannelKeyID, keyID)
	}
	assertProbeAuth(t, up.snapshot())
	if expected := (probe.RunCounts{Planned: 38, Sent: 37, Completed: 34, Failed: 3, Skipped: 1}); run.Report.Counts != expected {
		t.Fatalf("中止报告应保留已完成样本和缺测计数: %+v", run.Report.Counts)
	}
	if len(run.Report.Calls) != run.Report.Counts.Planned {
		t.Errorf("报告调用数 = %d，planned = %d", len(run.Report.Calls), run.Report.Counts.Planned)
	}
	if status := probeItemStatus(run, "reasoning_usage"); status != "fail" {
		t.Errorf("明确零推理应判不通过，实际 %q", status)
	}
	if verdict := probeBlockVerdict(run, "能力"); verdict != "不通过" {
		t.Errorf("能力块应聚合为不通过，实际 %q", verdict)
	}
	if run.Verdict != "不通过" {
		t.Errorf("列表 verdict 应为简明能力结论: %q", run.Verdict)
	}
	var list []struct {
		ID      int64  `json:"id"`
		Verdict string `json:"verdict"`
		Score   *int   `json:"score"`
	}
	a.do("GET", "/api/admin/probes", nil, &list)
	if len(list) != 1 || list[0].ID != id || list[0].Verdict != run.Verdict || list[0].Score != nil {
		t.Fatalf("列表与详情 verdict/null score 不一致: list=%+v detail=%+v", list, run)
	}
}

// TestProbeV3QualityEmptyReport：合成夹具——所有请求都返回内容为空、没有推理用量的响应。
// 断言空响应在预检停机，并保存不可判定的报告和未调度计数。
func TestProbeV3QualityEmptyReport(t *testing.T) {
	e := newEnv(t)
	up := newProbeUpstream(t)
	up.script = func(w http.ResponseWriter, _ probeUpstreamRequest) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_empty","model":"up-sol","status":"completed","output":[]}}`+"\n\n")
		return true
	}
	a, id, keyID := startV3ProbeRun(t, e, up)
	run := waitProbeRun(t, a, id)
	if run.Status != "failed" || run.Report.StopReason != "preflight_failed" {
		t.Fatalf("空响应应在预检后中止: status=%s stop=%q", run.Status, run.Report.StopReason)
	}
	probeReportContract(t, run)
	if run.Report.ChannelKeyID != keyID {
		t.Errorf("报告 key ID = %d，期望实际选中的渠道 key %d", run.Report.ChannelKeyID, keyID)
	}
	if expected := (probe.RunCounts{Planned: 38, Sent: 3, Failed: 3, Skipped: 35}); run.Report.Counts != expected {
		t.Errorf("空报告计数不对: %+v", run.Report.Counts)
	}
	if len(run.Report.Calls) != run.Report.Counts.Planned {
		t.Errorf("空报告调用数 = %d，planned = %d", len(run.Report.Calls), run.Report.Counts.Planned)
	}
	if verdict := probeBlockVerdict(run, "能力"); verdict != "无法判定" {
		t.Errorf("全缺测的能力块应为无法判定，实际 %q", verdict)
	}
	if status := probeItemStatus(run, "reasoning_usage"); status != "unknown" {
		t.Errorf("推理用量未上报应记无法判定，实际 %q", status)
	}
	if run.Verdict == "" {
		t.Error("空报告也应写简明结论")
	}
	var list []struct {
		ID      int64  `json:"id"`
		Verdict string `json:"verdict"`
		Score   *int   `json:"score"`
	}
	a.do("GET", "/api/admin/probes", nil, &list)
	if len(list) != 1 || list[0].ID != id || list[0].Verdict != run.Verdict || list[0].Score != nil {
		t.Fatalf("空报告列表与详情不一致: list=%+v detail=%+v", list, run)
	}
}

// TestProbeV3ProtocolToolAdmission：合成夹具——工具请求使用 Responses 协议参数，
// 断言体验块能单独给出工具调用结论，报告仍能持久化和读出。
func TestProbeV3ProtocolToolAdmission(t *testing.T) {
	e := newEnv(t)
	a, id, _ := startV3ProbeRun(t, e, newProbeUpstream(t))
	run := waitProbeRun(t, a, id)
	probeReportContract(t, run)
	if run.Report.Protocol != "responses" {
		t.Errorf("协议 = %q，want responses", run.Report.Protocol)
	}
	if status := probeItemStatus(run, "tool"); status != "pass" {
		t.Errorf("工具调用应单独通过，实际 %q", status)
	}
	for _, key := range []string{"structured", "document", "echo"} {
		if status := probeItemStatus(run, key); status != "" {
			t.Errorf("新报告不应有 %s: %q", key, status)
		}
	}
}

// TestProbeV3ThrottleRetryAfterZero：显式 Retry-After: 0 让本轮暂停立即结束，
// 529 之后仍能把余下请求发完，不会因为缺省 20 秒暂停拖住检测。
func TestProbeV3ThrottleRetryAfterZero(t *testing.T) {
	e := newEnv(t)
	up := newProbeUpstream(t)
	var throttled atomic.Bool
	up.setScript(func(w http.ResponseWriter, request probeUpstreamRequest) bool {
		if request.header.Get("Session-Id") == "" || !throttled.CompareAndSwap(false, true) {
			return false
		}
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(529)
		fmt.Fprint(w, `{"error":{"message":"overloaded"}}`)
		return true
	})
	a, id, _ := startV3ProbeRun(t, e, up)
	run := waitProbeRun(t, a, id)
	if run.Status != "done" || run.Report.Counts.Sent != 38 || len(up.snapshot()) != 39 {
		t.Fatalf("显式 Retry-After 0 应继续发完请求: %+v", run.Report.Counts)
	}
	if !throttled.Load() {
		t.Fatal("夹具应至少触发一次 529")
	}
}
