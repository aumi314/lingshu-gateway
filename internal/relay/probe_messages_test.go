package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/store"
)

// 事件按 2026-11-07 在 kinght claude0.8 上抓到的 claude-opus-5-5 响应构造（编号、usage 字段、thinking 签名、工具调用分片）。
const probeMessagesSSE = `event: message_start
data: {"type":"message_start","message":{"model":"claude-opus-5-5","id":"msg_011CgXIcPjXkQ1kNXWHg5Oqj","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":446,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard","inference_geo":"us"}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCkYIBxgCKkDx"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Hobart\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null,"stop_details":null},"usage":{"input_tokens":446,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":389,"output_tokens_details":{"thinking_tokens":377}}}

event: message_stop
data: {"type":"message_stop"}

`

func TestProbeMessagesDisguiseAndParse(t *testing.T) {
	var got map[string]any
	var hd http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("路径 %s", r.URL.Path)
		}
		hd = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, probeMessagesSSE)
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	ch := &store.Channel{BaseURLMessages: up.URL, APIKey: "sk-test"}
	c := h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "sess-1", probe.Request{
		Prompt: "What is the weather in Hobart right now? Use the tool.", System: "Only reply vvv", Effort: "high", Document: probe.MinimalPDF("x"),
		Tools: []map[string]any{{"name": "get_weather"}},
	})

	// 请求伪装成 Claude Code，会话编号进 metadata.user_id
	if hd.Get("x-api-key") != "sk-test" || !strings.HasPrefix(hd.Get("User-Agent"), "claude-cli/") || hd.Get("X-App") != "cli" || !strings.Contains(hd.Get("Anthropic-Beta"), "claude-code-") {
		t.Errorf("请求头 %v", hd)
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 3 || !strings.Contains(sys[1].(map[string]any)["text"].(string), "Claude Code") || sys[2].(map[string]any)["text"] != "Only reply vvv" {
		t.Errorf("system %v", got["system"])
	}
	var uid map[string]string
	json.Unmarshal([]byte(got["metadata"].(map[string]any)["user_id"].(string)), &uid)
	if uid["session_id"] != "sess-1" || len(uid["device_id"]) != 64 {
		t.Errorf("metadata.user_id %v", uid)
	}
	if got["thinking"].(map[string]any)["type"] != "adaptive" || got["output_config"].(map[string]any)["effort"] != "high" || got["max_tokens"] == nil {
		t.Errorf("推理参数 %v %v %v", got["thinking"], got["output_config"], got["max_tokens"])
	}
	msg := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if msg[0].(map[string]any)["type"] != "document" || msg[1].(map[string]any)["type"] != "text" {
		t.Errorf("PDF 应作为 document 块放在文字前面: %v", msg)
	}

	// 响应解析
	if c.Error != "" || c.ID != "msg_011CgXIcPjXkQ1kNXWHg5Oqj" || c.Model != "claude-opus-5-5" || c.FinishReason != "tool_use" || !c.Done {
		t.Errorf("基本字段 %+v", c)
	}
	if strings.Join(c.EventTypes, ",") != "message_start,content_block_start,ping,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Errorf("事件序列 %v", c.EventTypes)
	}
	if c.SignedThinking != 1 || c.ReasoningTokens != 377 || c.OutputTokens != 389 || c.InputTokens != 446 || !c.UsageReported {
		t.Errorf("推理和用量 %+v", c)
	}
	if !c.ReasoningReported {
		t.Errorf("thinking_tokens 出现时应记推理用量可核验: %+v", c)
	}
	if c.SignedThinking != 1 {
		t.Errorf("SignedThinking 应数带非空签名的 thinking 块: %+v", c)
	}
	if strings.Join(c.UsageFields, ",") != "cache_creation,cache_creation_input_tokens,cache_read_input_tokens,inference_geo,input_tokens,output_tokens,service_tier" {
		t.Errorf("usage 字段 %v", c.UsageFields)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].Name != "get_weather" || c.ToolCalls[0].Arguments != `{"city":"Hobart"}` {
		t.Errorf("工具调用 %+v", c.ToolCalls)
	}
}

// fakeProbeClient 用内存里的响应替身发请求，不开本机端口。
type fakeProbeClient struct {
	status      int
	contentType string
	body        string
	readErr     error
}

type fakeProbeBody struct {
	*strings.Reader
	tailErr error
}

func (body fakeProbeBody) Close() error { return nil }

func (body fakeProbeBody) Read(p []byte) (int, error) {
	n, err := body.Reader.Read(p)
	if err == io.EOF && body.tailErr != nil {
		return n, body.tailErr
	}
	return n, err
}

type fakeProbeRoundTripper struct{ response fakeProbeClient }

func (rt fakeProbeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	status := rt.response.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{rt.response.contentType}},
		Body:       fakeProbeBody{Reader: strings.NewReader(rt.response.body), tailErr: rt.response.readErr},
		Request:    req,
	}, nil
}

func fakeProbeHandler(response fakeProbeClient) *Handler {
	return &Handler{Client: &http.Client{Transport: fakeProbeRoundTripper{response: response}}}
}

// 推理用量四种情况（缺失、null、0、正值）在 SSE 的 message_delta 和整段 JSON 里都要区分；null 不算报告。
func TestProbeMessagesReasoningFieldPresence(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"m\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"
	delta := func(usage string) string {
		return "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":" + usage + "}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	qualityReq := probe.Request{Prompt: "hi", Item: "reasoning", DisguiseSet: true, FullDisguise: true}
	cases := []struct {
		name   string
		wire   string
		usage  string
		report bool
		value  int64
	}{
		{"sse-missing", "sse", `{"output_tokens":5}`, false, 0},
		{"sse-null", "sse", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":null}}`, false, 0},
		{"sse-zero", "sse", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":0}}`, true, 0},
		{"sse-positive", "sse", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":9}}`, true, 9},
		{"json-missing", "json", `{"output_tokens":5}`, false, 0},
		{"json-null", "json", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":null}}`, false, 0},
		{"json-zero", "json", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":0}}`, true, 0},
		{"json-positive", "json", `{"output_tokens":5,"output_tokens_details":{"thinking_tokens":9}}`, true, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			contentType, body := "application/json", `{"id":"m","model":"m","stop_reason":"end_turn","content":[{"type":"thinking","signature":"sig"},{"type":"text","text":"42"}],"usage":`+c.usage+`}`
			if c.wire == "sse" {
				contentType, body = "text/event-stream", start+delta(c.usage)
			}
			h := fakeProbeHandler(fakeProbeClient{contentType: contentType, body: body})
			call := h.ProbeMessages(context.Background(), &store.Channel{BaseURLMessages: "http://up.invalid", APIKey: "k"}, "m", "s", qualityReq)
			if call.ReasoningReported != c.report || call.ReasoningTokens != c.value {
				t.Errorf("%s => %+v，期望 report=%v value=%d", c.name, call, c.report, c.value)
			}
			if c.wire == "sse" && (call.Error != "" || !call.Done) {
				t.Errorf("%s: SSE 应正常结束: %+v", c.name, call)
			}
			if c.wire == "json" && (call.Error != "" || call.Done || call.FinishReason != "end_turn" || call.Text != "42" || call.SignedThinking != 1) {
				t.Errorf("%s: 完整 JSON 应保留回答和签名: %+v", c.name, call)
			}
		})
	}
}

func TestProbeChatReasoningFieldPresence(t *testing.T) {
	for _, test := range []struct {
		field    string
		reported bool
		tokens   int64
	}{{"", false, 0}, {"null", false, 0}, {"0", true, 0}, {"7", true, 7}} {
		for _, stream := range []bool{false, true} {
			stats := `"completion_tokens":5`
			if test.field != "" {
				stats += `,"completion_tokens_details":{"reasoning_tokens":` + test.field + `}`
			}
			body := `{"choices":[{"finish_reason":"stop","message":{"content":"42"}}],"usage":{` + stats + `}}`
			contentType := "application/json"
			if stream {
				body, contentType = "data: "+body+"\n\ndata: [DONE]\n\n", "text/event-stream"
			}
			handler := fakeProbeHandler(fakeProbeClient{contentType: contentType, body: body})
			call := handler.ProbeChat(context.Background(), &store.Channel{BaseURLChat: "http://up.invalid"}, "m", "s", probe.Request{Prompt: "hi"})
			if call.Error != "" || call.ReasoningReported != test.reported || call.ReasoningTokens != test.tokens {
				t.Errorf("stream %v, field %q, call %+v", stream, test.field, call)
			}
		}
	}
}

// Chat 遇到 wire 读取失败时给 Call 记错，即使响应看起来有结束标记和用量。
func TestProbeChatPartialReadIsError(t *testing.T) {
	h := fakeProbeHandler(fakeProbeClient{
		contentType: "text/event-stream",
		body:        "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		readErr:     errors.New("unexpected EOF"),
	})
	call := h.ProbeChat(context.Background(), &store.Channel{BaseURLChat: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi", Item: "reasoning", DisguiseSet: true, FullDisguise: true})
	if call.Error == "" || !strings.Contains(call.Error, "unexpected EOF") || !call.Done || call.FinishReason != "stop" || call.Text != "42" {
		t.Errorf("wire 读取失败应记错、保留原始结束原因和文本: %+v", call)
	}
}

// quality reasoning 的流即使带 stop_reason，没收到 [DONE] 也算没测成；整段 JSON 不走这条规则。
func TestProbeChatMissingDoneIsErrorForQualityReasoning(t *testing.T) {
	body := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":\"stop\"}]}\n\n"
	for _, c := range []struct {
		name    string
		req     probe.Request
		wantErr bool
	}{
		{"quality", probe.Request{Prompt: "hi", Item: "reasoning", DisguiseSet: true, FullDisguise: true}, true},
		{"legacy", probe.Request{Prompt: "hi"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
			call := h.ProbeChat(context.Background(), &store.Channel{BaseURLChat: "http://up.invalid", APIKey: "k"}, "m", "s", c.req)
			if (call.Error != "") != c.wantErr {
				t.Errorf("%s: error = %q, wantErr = %v", c.name, call.Error, c.wantErr)
			}
			if call.FinishReason != "stop" || call.Text != "42" {
				t.Errorf("原始结束原因和文本应保留: %+v", call)
			}
		})
	}
}

// 上游先回 200 再在流里报错（2026-11-07 Claude 大海捞针 20 万 token 时实际发生）：记成请求失败。
func TestProbeMessagesStreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01x\",\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":201223,\"output_tokens\":1}}}\n\n"+
			"event: ping\ndata: {\"type\":\"ping\"}\n\n"+
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"prompt is too long\"}}\n\n")
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	c := h.ProbeMessages(context.Background(), &store.Channel{BaseURLMessages: up.URL, APIKey: "k"}, "claude-opus-5-5", "s", probe.Request{Prompt: "hi"})
	if !strings.Contains(c.Error, "prompt is too long") || c.Done {
		t.Errorf("流中报错应记成请求失败: %+v", c)
	}
}

// 上游回了 200、开了流，却一个字不回就断开（2026-11-07 kinght 实际发生）：两种检测请求都记成失败。
func TestProbeHungStreamIsError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01x\",\"model\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n")
			return
		}
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	ch := &store.Channel{BaseURLChat: up.URL, BaseURLMessages: up.URL, APIKey: "k"}
	if c := h.ProbeChat(context.Background(), ch, "m", "s", probe.Request{Prompt: "hi"}); !strings.Contains(c.Error, "没有正常结束") {
		t.Errorf("Chat 断流应记成失败: %+v", c)
	}
	if c := h.ProbeMessages(context.Background(), ch, "m", "s", probe.Request{Prompt: "hi"}); !strings.Contains(c.Error, "没有正常结束") {
		t.Errorf("Messages 断流应记成失败: %+v", c)
	}
}

// 官方 API 环境的 astra 不支持推理强度 none，报错里没有 reasoning 字样（2026-11-08 检测 59）：换成 low 重试。
func TestProbeChatRetriesUnsupportedNone(t *testing.T) {
	var efforts []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		e, _ := body["reasoning_effort"].(string)
		efforts = append(efforts, e)
		if e == "none" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported value: 'none' is not supported with the 'gpt-6-astra' model. Supported values are: 'low', 'medium', 'high' and 'xhigh'."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"mango\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	c := h.ProbeChat(context.Background(), &store.Channel{BaseURLChat: up.URL, APIKey: "k"}, "gpt-6-astra", "s", probe.Request{Prompt: "fruit?", Effort: "none"})
	if c.Error != "" || c.Text != "mango" || strings.Join(efforts, ",") != "none,low" || c.EffortSent != "low" || c.NoneRejectedBy != "400" {
		t.Errorf("应换成 low 重试并成功: %+v，推理强度依次 %v", c, efforts)
	}
}

// 有的中转把 Chat 转成 Responses 后，上游不支持 none 时什么都不回（aresai，2026-11-08 检测 60：0 token、流没有结束标记）：也换 low 重试。
func TestProbeChatRetriesNoneOnEmptyStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		if body["reasoning_effort"] == "none" {
			io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			return
		}
		io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"mango\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	h := &Handler{Client: up.Client()}
	c := h.ProbeChat(context.Background(), &store.Channel{BaseURLChat: up.URL, APIKey: "k"}, "gpt-6-astra", "s", probe.Request{Prompt: "fruit?", Effort: "none"})
	if c.Error != "" || c.Text != "mango" || c.EffortSent != "low" || c.NoneRejectedBy != "empty" {
		t.Errorf("空流应换成 low 重试并成功: %+v", c)
	}
}

// 指纹用的传输层伪装要过号池的 Claude Code 校验（docs/research/claude-probe.md 第九节，kirisame 二分实测）：
// 整套 x-stainless-* 头、anthropic-version、会话头等于 metadata 的 session_id、UA 版本等于计费头 cc_version 去掉最后一段；
// 系统提示恰好两行、不带工具和 thinking、max_tokens 1024（和 scripts/probe/probe.py 采官方样本的请求一致）。服务器有完整模板也不用。
func TestProbeMessagesTransportDisguise(t *testing.T) {
	var got map[string]any
	var hd http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, probeMessagesSSE)
	}))
	defer up.Close()
	d := &ProbeDisguise{}
	d.Claude.System = []map[string]any{{"type": "text", "text": "full template"}}
	d.Claude.Headers = map[string]string{"user-agent": "claude-cli/2.1.281 (external, cli)"}
	h := &Handler{Client: up.Client(), Disguise: d}
	ch := &store.Channel{BaseURLMessages: up.URL, APIKey: "sk-test"}
	h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "3f1c2b8e-0000-4000-8000-000000000001", probe.Request{Prompt: "Name a random gemstone.", DisguiseSet: true})

	for _, k := range []string{"X-Stainless-Arch", "X-Stainless-Lang", "X-Stainless-Os", "X-Stainless-Package-Version", "X-Stainless-Retry-Count", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout", "Anthropic-Version"} {
		if hd.Get(k) == "" {
			t.Errorf("缺少请求头 %s", k)
		}
	}
	var uid map[string]string
	json.Unmarshal([]byte(got["metadata"].(map[string]any)["user_id"].(string)), &uid)
	if hd.Get("X-Claude-Code-Session-Id") == "" || hd.Get("X-Claude-Code-Session-Id") != uid["session_id"] {
		t.Errorf("会话头 %q 应等于 metadata 的 session_id %q", hd.Get("X-Claude-Code-Session-Id"), uid["session_id"])
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("系统提示应恰好两行（不用完整模板）: %v", got["system"])
	}
	billing := sys[0].(map[string]any)["text"].(string)
	ua := strings.TrimSuffix(strings.TrimPrefix(hd.Get("User-Agent"), "claude-cli/"), " (external, cli)")
	if !strings.Contains(billing, "cc_version="+ua+".") {
		t.Errorf("UA 版本 %q 要等于计费头 cc_version 去掉最后一段: %q", ua, billing)
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("指纹请求不应带工具")
	}
	if _, ok := got["thinking"]; ok {
		t.Errorf("指纹请求不开 thinking")
	}
	if got["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v, want 1024", got["max_tokens"])
	}
}

// message_start 里的 thinking_tokens 是占位的 0，最终值在 message_delta；只在开头出现 0 时不能当成“明确报了 0”。
func TestProbeMessagesThinkingZeroOnlyInStart(t *testing.T) {
	sse := strings.Replace(probeMessagesSSE, `"output_tokens":1,`, `"output_tokens":1,"output_tokens_details":{"thinking_tokens":0},`, 1)
	sse = strings.Replace(sse, `,"output_tokens_details":{"thinking_tokens":377}`, ``, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer up.Close()
	d, _ := LoadProbeDisguise(writeDisguise(t, testDisguise))
	h := &Handler{Client: up.Client(), Disguise: d}
	c := h.ProbeMessages(context.Background(), &store.Channel{BaseURLMessages: up.URL, APIKey: "k"}, "claude-opus-5-5", "s",
		probe.Request{Item: "reasoning", Prompt: "x", Effort: "high", DisguiseSet: true, FullDisguise: true})
	if c.ReasoningReported {
		t.Errorf("只有开头占位的 0，不应算报了推理用量: %+v", c)
	}
}
