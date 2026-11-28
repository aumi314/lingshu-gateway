package relay

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/store"
)

func TestProbeCallRecordVisible(t *testing.T) {
	var call probe.Call
	call.RecordVisible("  \n\t", time.Millisecond)
	call.RecordVisible(" ", 10*time.Millisecond)
	if call.VisibleFirstMS != 0 || call.VisibleLastMS != 0 || call.VisibleEvents != 0 {
		t.Fatalf("空白不应记可见正文: %+v", call)
	}
	call.RecordVisible("hello", 250*time.Microsecond)
	call.RecordVisible("world", 20*time.Millisecond)
	call.RecordVisible("\nnext", 30*time.Millisecond)
	if call.VisibleFirstMS != 1 || call.VisibleLastMS != 30 || call.VisibleEvents != 3 {
		t.Fatalf("可见正文时间应取毫秒、首个不为零: %+v", call)
	}
}

func TestProbeResponsesVisibleTiming(t *testing.T) {
	body := strings.Join([]string{
		`event: response.created`, `data: {"type":"response.created"}`, ``,
		`event: response.reasoning_text.delta`, `data: {"type":"response.reasoning_text.delta","delta":"thinking"}`, ``,
		`event: response.function_call_arguments.delta`, `data: {"type":"response.function_call_arguments.delta","delta":"{\"city\":"}`, ``,
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":" "}`, ``,
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":"Hello"}`, ``,
		`event: response.output_text.delta`, `data: {"type":"response.output_text.delta","delta":" there"}`, ``,
		`event: response.completed`, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":" Hello there"}]}]}}`, ``,
		``,
	}, "\n")
	h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
	call := h.ProbeResponses(context.Background(), &store.Channel{BaseURLResponses: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi"})
	if call.Error != "" || !call.Done || call.Text != " Hello there" {
		t.Fatalf("流应正常结束并保留原样正文: %+v", call)
	}
	if call.FirstTokenMS == 0 || call.VisibleFirstMS == 0 || call.VisibleLastMS < call.VisibleFirstMS || call.VisibleEvents != 2 {
		t.Fatalf("只数非空正文增量，工具/推理/空白和 aggregated 输出不计: %+v", call)
	}
}

func TestProbeMessagesVisibleTiming(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start"}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret"}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"First "}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" "}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"get_weather"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
		``,
	}, "\n")
	h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
	call := h.ProbeMessages(context.Background(), &store.Channel{BaseURLMessages: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi"})
	if call.Error != "" || !call.Done || call.Text != "First  answer" || len(call.ToolCalls) != 1 {
		t.Fatalf("正文和工具应照常保留: %+v", call)
	}
	if call.FirstTokenMS == 0 || call.VisibleFirstMS == 0 || call.VisibleLastMS < call.VisibleFirstMS || call.VisibleEvents != 2 {
		t.Fatalf("只数 text 块起始和非空 text_delta，thinking/工具/空白不计: %+v", call)
	}
}

func TestProbeChatVisibleTiming(t *testing.T) {
	for _, test := range []struct {
		name  string
		body  string
		text  string
		count int
	}{
		{"stream", strings.Join([]string{
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":""}}]}`, ``,
			`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"get_weather","arguments":"{"}}]}}]}`, ``,
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":" "}}]}`, ``,
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":"stop"}]}`, ``,
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":" there"}}]}`, ``,
			`data: [DONE]`, ``, ``,
		}, "\n"), " Hello there", 2},
		{"json", `{"id":"c","choices":[{"finish_reason":"stop","message":{"content":"Hello"}}]}`, "Hello", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			contentType := "application/json"
			if test.name == "stream" {
				contentType = "text/event-stream"
			}
			h := fakeProbeHandler(fakeProbeClient{contentType: contentType, body: test.body})
			call := h.ProbeChat(context.Background(), &store.Channel{BaseURLChat: "http://up.invalid", APIKey: "k"}, "m", "s", probe.Request{Prompt: "hi"})
			if call.Error != "" || call.Text != test.text {
				t.Fatalf("正文应照常保留: %+v", call)
			}
			if call.VisibleFirstMS == 0 || call.VisibleLastMS < call.VisibleFirstMS || call.VisibleEvents != test.count {
				t.Fatalf("只数非空正文，工具/空白不计: %+v", call)
			}
		})
	}
}
