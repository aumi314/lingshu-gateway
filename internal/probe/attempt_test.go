package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestAttemptAvailability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		success := Call{HTTPStatus: http.StatusOK, Done: true, Text: "answer"}
		fixtures := [][]Call{
			{success},
			{{HTTPStatus: 529, Error: "overloaded"}, success},
			{{HTTPStatus: http.StatusForbidden, Error: `{"error":{"code":"policy_blocked"}}`}},
		}
		byQuestion := make([][]Attempt, len(fixtures))
		for i, replies := range fixtures {
			var next int
			req := Request{Item: "anchor", Session: i}
			callWithRetry(t.Context(), func(context.Context, Request) Call {
				result := replies[next]
				next++
				return result
			}, req, func(c Call, reason string) {
				byQuestion[i] = append(byQuestion[i], newAttempt(req, i+1, len(byQuestion[i])+1, reason, c))
			})
		}
		attempts, stats := summarizeAttempts(byQuestion, []bool{true, true, true}, make([]bool, 3))
		if *stats != (Availability{SentQuestions: 3, FirstUsable: 1, FinalUsable: 2, Sends: 4}) {
			t.Fatalf("可用率统计不符: %+v", stats)
		}
		if attempts[2].Question != 2 || attempts[2].Session != 1 || attempts[2].Number != 2 || attempts[2].RetryReason != "overloaded" || attempts[3].ErrorType != "blocked" {
			t.Fatalf("逐次记录不符: %+v", attempts)
		}
	})
}

func TestFailedAttemptStopsBeforeRetry(t *testing.T) {
	var sends atomic.Int32
	report, err := Run(t.Context(), func(ctx context.Context, req Request) Call {
		sends.Add(1)
		input := int64(70000)
		failed := Call{HTTPStatus: 529, Error: "overloaded",
			WireUsage: AttemptUsage{Fields: map[string]*int64{"input_tokens": &input}, Input: input}}
		req.OnAttempt(failed, req.RetryReason)
		if ctx.Err() == nil {
			t.Error("失败尝试记录完后应立即触发停机")
		}
		return failed
	}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.StopReason != "billing_anomaly" || report.StopCallIndex == nil || report.Availability.Sends != int(sends.Load()) || len(report.Attempts) < 1 || len(report.Attempts) > 3 || report.Availability.Unscheduled == 0 {
		t.Fatalf("费用保护应保留已发尝试并拦住重试: stop=%s availability=%+v attempts=%+v", report.StopReason, report.Availability, report.Attempts)
	}
	for _, attempt := range report.Attempts {
		if attempt.Number != 1 || attempt.Usage.Input != 70000 {
			t.Fatalf("不应重试，也不应丢失失败用量: %+v", attempt)
		}
	}
}

func TestAttemptSeparatesContentAndStreamEnd(t *testing.T) {
	tools := []map[string]any{{"name": "weather"}}
	for _, tc := range []struct {
		name          string
		request       Request
		call          Call
		content, done bool
		kind          string
	}{
		{"partial text", Request{}, Call{HTTPStatus: 200, Text: "partial"}, true, false, "stream_interrupted"},
		{"usage only", Request{}, Call{HTTPStatus: 200, Done: true, UsageReported: true, InputTokens: 20}, false, true, "missing_content"},
		{"valid tool", Request{Tools: tools}, Call{HTTPStatus: 200, Done: true, ToolCalls: []ToolCall{{Name: "weather", Arguments: `{"city":"Oslo"}`}}}, true, true, ""},
		{"wrong tool", Request{Tools: tools}, Call{HTTPStatus: 200, Done: true, ToolCalls: []ToolCall{{Name: "other", Arguments: `{}`}}}, false, true, "missing_content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newAttempt(tc.request, 1, 1, "", tc.call)
			if got.HasContent != tc.content || got.Done != tc.done || got.ErrorType != tc.kind || got.Usable != (tc.content && tc.kind == "") {
				t.Fatalf("尝试状态不符: %+v", got)
			}
		})
	}
}

func TestAttemptCancellationAndUnscheduled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	report, err := Run(ctx, func(_ context.Context, _ Request) Call {
		cancel()
		return Call{AttemptError: "cancelled", Error: "连接失败"}
	}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats := report.Availability
	if report.StopReason != "cancelled" || stats.Sends < 1 || stats.Sends > stats.Cancelled || stats.Cancelled+stats.Unscheduled != report.Counts.Planned || stats.FinalUsable != 0 {
		t.Fatalf("取消与未调度应分别统计: stop=%s stats=%+v", report.StopReason, stats)
	}
	if !slices.ContainsFunc(report.Attempts, func(a Attempt) bool { return a.ErrorType == "cancelled" }) {
		t.Fatalf("应保留被取消的实际发送: %+v", report.Attempts)
	}
}

func TestLegacyReportWithoutAttempts(t *testing.T) {
	var report Report
	if err := json.Unmarshal([]byte(`{"verdict":"通过","calls":[{"text":"answer"}]}`), &report); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil || report.Verdict != "通过" || len(report.Calls) != 1 || report.Calls[0].Text != "answer" || strings.Contains(string(raw), `"availability"`) || strings.Contains(string(raw), `"attempts"`) {
		t.Fatalf("旧报告应保留原答卷和结论: %s (%v)", raw, err)
	}
}

func TestLegacyQualityRuleSnapshotSurvives(t *testing.T) {
	var report Report
	old := `{"format_version":3,"rule_version":"quality-v3.1","blocks":[{"key":"experience","items":[{"key":"structured","rule_id":"structured","status":"pass"},{"key":"document","rule_id":"document","status":"pass"},{"key":"echo","rule_id":"echo","status":"fail"}]}],"rules":[{"id":"structured","version":"quality-v3.1"},{"id":"document","version":"quality-v3.1"},{"id":"echo","version":"quality-v3.1"}]}`
	if err := json.Unmarshal([]byte(old), &report); err != nil {
		t.Fatal(err)
	}
	if report.RuleVersion != "quality-v3.1" || len(report.Blocks) != 1 || len(report.Blocks[0].Items) != 3 || len(report.Rules) != 3 {
		t.Fatalf("旧报告未保留规则快照: %+v", report)
	}
	for _, rule := range report.Rules {
		if item := findQualityItem(report.Blocks[0].Items, rule.ID); item.RuleID != rule.ID {
			t.Errorf("旧规则 %q 找不到对应结果", rule.ID)
		}
	}
}
