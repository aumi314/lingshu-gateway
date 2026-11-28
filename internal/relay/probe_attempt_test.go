package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/store"
)

type probeAttemptTransport func(*http.Request) (*http.Response, error)

func (transport probeAttemptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestProbeAttemptEffortFallback(t *testing.T) {
	for _, stop := range []bool{false, true} {
		label := "retry"
		if stop {
			label = "stop before fallback"
		}
		t.Run(label, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var wire []string
			h := &Handler{Client: &http.Client{Transport: probeAttemptTransport(func(req *http.Request) (*http.Response, error) {
				var body struct {
					Effort string `json:"reasoning_effort"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				wire = append(wire, body.Effort)
				response := fakeProbeClient{contentType: "text/event-stream", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"}
				if body.Effort == "none" {
					response = fakeProbeClient{status: 400, contentType: "application/json", body: `{"error":{"message":"Unsupported value: 'none'"},"usage":{"prompt_tokens":11,"completion_tokens":0}}`}
				}
				return (fakeProbeRoundTripper{response: response}).RoundTrip(req)
			})}}
			var attempts []probe.Call
			var reasons []string
			call := h.ProbeChat(ctx, &store.Channel{BaseURLChat: "http://up.invalid"}, "model", "session", probe.Request{
				Effort: "none", OnAttempt: func(call probe.Call, reason string) {
					attempts = append(attempts, call)
					reasons = append(reasons, reason)
					if stop {
						cancel()
					}
				},
			})
			want := []string{"none"}
			if !stop {
				want = append(want, "low")
			}
			if !reflect.DeepEqual(wire, want) || len(attempts) != len(want) {
				t.Fatalf("发送和记录应一一对应: wire=%v attempts=%+v", wire, attempts)
			}
			if first := attempts[0]; first.HTTPStatus != 400 || first.EffortSent != "none" || first.WireUsage.Input != 11 || first.WireUsage.Fields["completion_tokens"] == nil || *first.WireUsage.Fields["completion_tokens"] != 0 || first.TotalMS == 0 {
				t.Fatalf("应保留失败尝试的设置、显式零用量和耗时: %+v", first)
			}
			if !stop && (reasons[1] != "effort_none_rejected_400" || attempts[1].EffortSent != "low" || !attempts[1].Done || !call.Done) {
				t.Fatalf("应保留 low 回退和最终答卷: %+v %v", attempts, reasons)
			}
		})
	}
}

func TestProbeAttemptMessagesCacheWrite(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":8,\"cache_creation_input_tokens\":20,\"cache_creation\":{\"ephemeral_5m_input_tokens\":20},\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6,\"output_tokens_details\":{\"thinking_tokens\":null}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	h := fakeProbeHandler(fakeProbeClient{contentType: "text/event-stream", body: body})
	call := h.ProbeMessages(t.Context(), &store.Channel{BaseURLMessages: "http://up.invalid"}, "model", "session", probe.Request{})
	counts := call.WireUsage
	if !call.Done || counts.Input != 10 || counts.CacheRead != 8 || counts.CacheWrite != 20 || counts.Output != 6 || call.InputTokens != 38 {
		t.Fatalf("缓存写应独立保留，旧答卷仍显示总输入: %+v", call)
	}
	if tokens := counts.Fields["cache_creation.ephemeral_5m_input_tokens"]; tokens == nil || *tokens != 20 {
		t.Fatalf("缺少原始缓存写字段: %+v", counts.Fields)
	}
	if tokens, present := counts.Fields["output_tokens_details.thinking_tokens"]; !present || tokens != nil {
		t.Fatalf("显式 null 应和缺字段区分: %+v", counts.Fields)
	}
}
