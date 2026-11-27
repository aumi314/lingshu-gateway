package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/store"
)

// 测试用的伪装配置：结构和 references/probe-sites/build_disguise.py 生成的一样，内容是假的（真实内容不进仓库）。
const testDisguise = `{
 "claude": {"headers": {"user-agent": "claude-cli/9.9.9 (external, cli)", "anthropic-beta": "claude-code-20250219,test-beta", "x-stainless-lang": "js", "x-app": "cli"},
  "system": [{"type":"text","text":"x-anthropic-billing-header: cc_version=9.9.9;"},{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Model: {{MODEL_NAME}} ({{MODEL_ID}})"}],
  "tools": [{"name":"Bash","description":"run","input_schema":{"type":"object"}}], "max_tokens": 32000},
 "codex": {"headers": {"user-agent": "codex_exec/9.9.9", "originator": "codex_exec"},
  "input": [{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"<current_date>{{DATE}}</current_date>"}]}],
  "body": {"tool_choice":"auto","parallel_tool_calls":false,"text":{"verbosity":"low"}}, "reasoning_context": "all_turns"}
}`

func writeDisguise(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "d.json")
	os.WriteFile(p, []byte(content), 0o600)
	return p
}

func TestLoadProbeDisguise(t *testing.T) {
	d, err := LoadProbeDisguise(writeDisguise(t, testDisguise))
	if err != nil || !d.hasClaude() || !d.hasCodex() {
		t.Fatalf("合法配置应能读: %v", err)
	}
	if _, err := LoadProbeDisguise(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("文件不存在应报错（调用方退回简版伪装）")
	}
	if _, err := LoadProbeDisguise(writeDisguise(t, `{"claude":{},"codex":{}}`)); err == nil {
		t.Error("两部分都空应报错")
	}
	if got := claudeDisplayName("claude-opus-5-5"); got != "Opus 5.5" {
		t.Errorf("显示名 %q", got)
	}
}

// Claude 完整伪装：请求头、系统提示（填好模型名，检测指令接在最后）、工具（伪装工具在前、检测工具在后）、默认 max_tokens。
func TestProbeMessagesWithDisguise(t *testing.T) {
	var got map[string]any
	var hd http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, probeMessagesSSE)
	}))
	defer up.Close()
	d, _ := LoadProbeDisguise(writeDisguise(t, testDisguise))
	h := &Handler{Client: up.Client(), Disguise: d}
	h.ProbeMessages(context.Background(), &store.Channel{BaseURLMessages: up.URL, APIKey: "k"}, "claude-opus-5-5", "sess-1",
		probe.Request{Prompt: "hi", System: "Only reply vvv", Tools: []map[string]any{{"name": "get_weather_ab12"}}})
	if hd.Get("User-Agent") != "claude-cli/9.9.9 (external, cli)" || !strings.Contains(hd.Get("Anthropic-Beta"), "test-beta") ||
		hd.Get("X-Stainless-Lang") != "js" || hd.Get("X-Claude-Code-Session-Id") != "sess-1" || hd.Get("X-Client-Request-Id") == "" {
		t.Errorf("请求头 %v", hd)
	}
	sys := got["system"].([]any)
	if len(sys) != 4 || sys[2].(map[string]any)["text"] != "Model: Opus 5.5 (claude-opus-5-5)" || sys[3].(map[string]any)["text"] != "Only reply vvv" {
		t.Errorf("system %v", sys)
	}
	tools := got["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "Bash" || tools[1].(map[string]any)["name"] != "get_weather_ab12" {
		t.Errorf("tools %v", tools)
	}
	if got["max_tokens"].(float64) != 32000 {
		t.Errorf("max_tokens %v", got["max_tokens"])
	}
}

// 按 Codex Responses 流构造：正文增量、工具调用（参数分片，以 completed 里的完整输出为准）、用量含推理 token。
const probeResponsesSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_0abc","model":"gpt-6.1-sol","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":" there"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_0abc","model":"gpt-6.1-sol","status":"completed","service_tier":"default","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"Hello there"}]},{"type":"function_call","name":"get_weather_ab12","arguments":"{\"city\":\"Hobart\"}"}],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":100},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":30}}}}

`

func TestProbeResponsesShapeAndParse(t *testing.T) {
	var got map[string]any
	var hd http.Header
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, hd = r.URL.Path, r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, probeResponsesSSE)
	}))
	defer up.Close()
	d, _ := LoadProbeDisguise(writeDisguise(t, testDisguise))
	h := &Handler{Client: up.Client(), Disguise: d}
	req := probe.Request{Prompt: "weather?", System: "Only reply kiwi", Effort: "high", MaxTokens: 64,
		Tools:          []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather_ab12", "description": "d", "parameters": map[string]any{"type": "object"}}}},
		ResponseFormat: map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "strict": true, "schema": map[string]any{"type": "object"}}}}
	c := h.ProbeResponses(context.Background(), &store.Channel{BaseURLResponses: up.URL + "/v1", APIKey: "k"}, "gpt-6.1-sol", "sess-9", req)

	// 请求形态和真实 Codex 一致
	if path != "/v1/responses" || hd.Get("User-Agent") != "codex_exec/9.9.9" || hd.Get("Session-Id") != "sess-9" || hd.Get("Authorization") != "Bearer k" ||
		!strings.Contains(hd.Get("X-Codex-Turn-Metadata"), `"session_id":"sess-9"`) {
		t.Errorf("路径或请求头 %s %v", path, hd)
	}
	if _, ok := got["temperature"]; ok {
		t.Error("不能传 temperature（真 Codex 不传，ztest 因此露馅）")
	}
	if _, ok := got["max_output_tokens"]; ok {
		t.Error("完整伪装下不传 max_output_tokens（真 Codex 不传）")
	}
	input := got["input"].([]any)
	if len(input) != 4 || !strings.Contains(mustJSON(input[1]), `current_date\u003e20`) || strings.Contains(mustJSON(input[1]), "{{DATE}}") ||
		input[2].(map[string]any)["role"] != "developer" || input[3].(map[string]any)["role"] != "user" {
		t.Errorf("input %s", mustJSON(input))
	}
	r := got["reasoning"].(map[string]any)
	if r["effort"] != "high" || r["context"] != "all_turns" || got["prompt_cache_key"] != "sess-9" || got["tool_choice"] != "auto" {
		t.Errorf("请求体 %v", got)
	}
	if tf := got["text"].(map[string]any); tf["verbosity"] != "low" || tf["format"].(map[string]any)["type"] != "json_schema" {
		t.Errorf("text %v", tf)
	}
	if tl := got["tools"].([]any); tl[0].(map[string]any)["name"] != "get_weather_ab12" || tl[0].(map[string]any)["type"] != "function" {
		t.Errorf("tools %v", tl)
	}
	// 解析
	if c.Error != "" || !c.Done || c.ID != "resp_0abc" || c.Text != "Hello there" || c.ServiceTier != "default" || c.FirstTokenMS == 0 {
		t.Errorf("解析 %+v", c)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Arguments != `{"city":"Hobart"}` {
		t.Errorf("工具 %+v", c.ToolCalls)
	}
	if c.InputTokens != 120 || c.CachedTokens != 100 || c.OutputTokens != 50 || c.ReasoningTokens != 30 {
		t.Errorf("用量 %+v", c)
	}
	if !c.ReasoningReported {
		t.Errorf("response.completed 带 reasoning_tokens 时应记推理用量可核验: %+v", c)
	}
}

// 推理用量字段的四种情况：缺失、null、0、正值。测试用内存响应替身，不开端口。
func TestProbeResponsesReasoningFieldPresence(t *testing.T) {
	usageJSON := func(field string) string {
		if field == "" {
			return `{"input_tokens":10,"output_tokens":5}`
		}
		return `{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":` + field + `}}`
	}
	cases := []struct {
		name   string
		field  string
		report bool
		value  int64
	}{
		{"json-missing", "", false, 0},
		{"json-null", "null", false, 0},
		{"json-zero", "0", true, 0},
		{"json-positive", "7", true, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"r","model":"m","status":"completed","output":[],"usage":%s}`, usageJSON(c.field))
			h := fakeProbeHandler(fakeProbeClient{contentType: "application/json", body: body})
			call := h.ProbeResponses(context.Background(), &store.Channel{BaseURLResponses: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi"})
			if call.ReasoningReported != c.report || call.ReasoningTokens != c.value {
				t.Errorf("%s => %+v，期望 report=%v value=%d", c.name, call, c.report, c.value)
			}
		})
	}

	sseCases := []struct {
		name   string
		field  string
		report bool
		value  int64
	}{
		{"sse-missing", "", false, 0},
		{"sse-null", "null", false, 0},
		{"sse-zero", "0", true, 0},
		{"sse-positive", "7", true, 7},
	}
	for _, c := range sseCases {
		t.Run(c.name, func(t *testing.T) {
			body := fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":%s}}\n\n", usageJSON(c.field))
			h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
			call := h.ProbeResponses(context.Background(), &store.Channel{BaseURLResponses: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi"})
			if call.Error != "" || !call.Done || call.ReasoningReported != c.report || call.ReasoningTokens != c.value {
				t.Errorf("%s => %+v，期望 report=%v value=%d", c.name, call, c.report, c.value)
			}
		})
	}
}

// 上游发 response.incomplete 时即便带半截文本，FinishReason 也固定为 incomplete；判定侧按终止状态算没测成。
func TestProbeResponsesIncompleteIsUnmeasured(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"42\"}\n\n" +
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"42\"}]}]}}\n\n"
	h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
	call := h.ProbeResponses(context.Background(), &store.Channel{BaseURLResponses: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi", Item: "reasoning", DisguiseSet: true, FullDisguise: true})
	if call.Done || call.FinishReason != "incomplete" || call.Text != "42" {
		t.Errorf("response.incomplete 应算没测成: %+v", call)
	}
}

// 失败场景：流中 error、断流、none 被 400 拒后改 low 重试；没有伪装配置时用简版并按 MaxTokens 传上限。
func TestProbeResponsesFailures(t *testing.T) {
	var efforts []string
	var lastBody map[string]any
	mode := ""
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&lastBody)
		e := ""
		if rr, ok := lastBody["reasoning"].(map[string]any); ok {
			e, _ = rr["effort"].(string)
		}
		efforts = append(efforts, e)
		if e == "none" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported value: 'none' is not supported with the 'gpt-6-astra' model."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch mode {
		case "error":
			io.WriteString(w, "data: {\"type\":\"error\",\"message\":\"overloaded\"}\n\n")
		case "hung":
			io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		default:
			io.WriteString(w, probeResponsesSSE)
		}
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	ch := &store.Channel{BaseURLResponses: up.URL, APIKey: "k"}
	mode = "error"
	if c := h.ProbeResponses(context.Background(), ch, "m", "s", probe.Request{Prompt: "x"}); !strings.Contains(c.Error, "overloaded") {
		t.Errorf("流中报错: %+v", c)
	}
	mode = "hung"
	if c := h.ProbeResponses(context.Background(), ch, "m", "s", probe.Request{Prompt: "x"}); !strings.Contains(c.Error, "没有正常结束") {
		t.Errorf("断流: %+v", c)
	}
	mode = ""
	c := h.ProbeResponses(context.Background(), ch, "gpt-6-astra", "s", probe.Request{Prompt: "x", Effort: "none", MaxTokens: 8})
	if c.Error != "" || c.EffortSent != "low" || strings.Join(efforts[len(efforts)-2:], ",") != "none,low" {
		t.Errorf("none 应改 low 重试: %+v %v", c, efforts)
	}
	if lastBody["max_output_tokens"].(float64) != 16 {
		t.Errorf("简版伪装按 MaxTokens 传上限（不低于 16）: %v", lastBody["max_output_tokens"])
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 完整伪装下不需要工具的题加“不要用工具”；工具题不加；custom_tool_call 识别成工具调用。
func TestDisguiseNoToolsSuffixAndCustomTool(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"custom_tool_call\",\"name\":\"exec\",\"input\":\"1+1\"}]}}\n\n")
	}))
	defer up.Close()
	d, _ := LoadProbeDisguise(writeDisguise(t, testDisguise))
	h := &Handler{Client: up.Client(), Disguise: d}
	ch := &store.Channel{BaseURLResponses: up.URL, APIKey: "k"}
	c := h.ProbeResponses(context.Background(), ch, "m", "s", probe.Request{Prompt: "What is 2+2?"})
	input := got["input"].([]any)
	if last := mustJSON(input[len(input)-1]); !strings.Contains(last, "do not use any tools") {
		t.Errorf("应加不用工具: %s", last)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "exec" || c.ToolCalls[0].Arguments != "1+1" {
		t.Errorf("custom_tool_call: %+v", c.ToolCalls)
	}
	h.ProbeResponses(context.Background(), ch, "m", "s", probe.Request{Prompt: "Use the tool.", Tools: []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather_ab12"}}}})
	input = got["input"].([]any)
	if last := mustJSON(input[len(input)-1]); strings.Contains(last, "do not use any tools") {
		t.Errorf("工具题不应加: %s", last)
	}
}

// 多轮缓存：第二轮的请求开头必须和第一轮发出去的一模一样（含加在题面后的“不要用工具”），上游缓存才能接上。
func TestProbeChainKeepsPrefix(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			io.WriteString(w, probeMessagesSSE)
		} else {
			io.WriteString(w, probeResponsesSSE)
		}
	}))
	defer up.Close()
	d, _ := LoadProbeDisguise(writeDisguise(t, testDisguise))
	h := &Handler{Client: up.Client(), Disguise: d}
	first := probe.Request{Item: "chain", Prompt: "lots of text. What is 1+2?", DisguiseSet: true, FullDisguise: true}
	second := probe.Request{Item: "chain", Prompt: "more text. What is 3+4?", History: []probe.Turn{{User: first.Prompt, Assistant: "3"}}, DisguiseSet: true, FullDisguise: true}

	ch := &store.Channel{BaseURLResponses: up.URL + "/v1", BaseURLMessages: up.URL, APIKey: "k"}
	h.ProbeResponses(context.Background(), ch, "gpt-6.1-sol", "s", first)
	h.ProbeResponses(context.Background(), ch, "gpt-6.1-sol", "s", second)
	a, b := bodies[0]["input"].([]any), bodies[1]["input"].([]any)
	if len(b) != len(a)+2 || mustJSON(b[:len(a)]) != mustJSON(a) || b[len(a)].(map[string]any)["role"] != "assistant" {
		t.Errorf("Responses 第二轮开头和第一轮不一致:\n%s\n%s", mustJSON(a), mustJSON(b))
	}

	bodies = nil
	h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "s", first)
	h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "s", second)
	m1, m2 := bodies[0]["messages"].([]any), bodies[1]["messages"].([]any)
	text := func(m any) string { return m.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string) }
	if len(m2) != 3 || text(m2[0]) != text(m1[0]) || m2[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("Messages 第二轮开头和第一轮不一致:\n%s\n%s", mustJSON(m1), mustJSON(m2))
	}
	if !strings.Contains(mustJSON(m2[2]), `"cache_control":{"type":"ephemeral"}`) || strings.Contains(mustJSON(m2[0]), "cache_control") {
		t.Errorf("缓存断点应只在最后一条消息上（和 Claude Code 一样）: %s", mustJSON(m2))
	}
}
