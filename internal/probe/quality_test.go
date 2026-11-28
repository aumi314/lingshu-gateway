package probe

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQualityVerdictPriority(t *testing.T) {
	for _, test := range []struct {
		items []Item
		want  string
	}{
		{[]Item{{RuleID: "reasoning_usage", Status: Unknown}, {RuleID: "needle_answer", Status: Fail}}, "不通过"},
		{[]Item{{RuleID: "reasoning_usage", Status: Unknown}, {RuleID: "reasoning_answers", Status: Fail}}, "无法判定"},
		{[]Item{{RuleID: "reasoning_usage", Status: Pass}, {RuleID: "injection", Status: Warn}, {RuleID: "tool", Status: Fail}}, "通过"},
		{[]Item{{RuleID: "reasoning_answers", Status: Info}}, "无法判定"},
		{nil, "无法判定"},
	} {
		if got := qualityVerdict(test.items); got != test.want {
			t.Errorf("%+v verdict=%s, want %s", test.items, got, test.want)
		}
	}
}

func TestQualityNeedleEvidence(t *testing.T) {
	p := &plan{needleCodes: []string{"AAA", "BBB", "CCC"}}
	for _, test := range []struct {
		name   string
		call   Call
		answer Status
		usage  Status
	}{
		{"漏答与低报各自记录", Call{Text: "AAA CCC", InputReported: true, InputTokens: 59999}, Fail, Fail},
		{"输入下限", Call{Text: "AAA BBB CCC", InputReported: true, InputTokens: 60000}, Pass, Pass},
		{"仅上报输出", Call{Text: "AAA BBB CCC", UsageReported: true}, Pass, Unknown},
		{"拦截", Call{HTTPStatus: http.StatusForbidden}, Unknown, Unknown},
		{"额度", Call{HTTPStatus: http.StatusPaymentRequired}, Unknown, Unknown},
		{"取消", Call{Error: "检测已取消"}, Unknown, Unknown},
		{"截断", Call{Text: "AAA", FinishReason: "max_tokens"}, Unknown, Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.call.Item = "needle"
			answer, usage := p.qualityNeedle([]Call{test.call})
			if answer.Status != test.answer || usage.Status != test.usage {
				t.Errorf("answer=%+v usage=%+v", answer, usage)
			}
		})
	}
}

func TestQualityStructuredAndToolSeparately(t *testing.T) {
	p := &plan{city: "Reykjavik", toolName: "get_weather_12ab", nonce: "LS-example", nonceValue: 21}
	for _, test := range []struct {
		name, arguments string
		want            Status
	}{
		{p.toolName, `{"city":"  reYKjaVíK  "}`, Pass},
		{p.toolName, `{"city":"Reykjavík, Iceland"}`, Pass},
		{p.toolName, `{"city":"Reykjavík，Iceland"}`, Pass},
		{p.toolName, `{"city":"Reykjavik, Norway"}`, Fail},
		{p.toolName, `{"city":"Reykjavik, Iceland, Europe"}`, Fail},
		{p.toolName, `{"city":"Hobart"}`, Fail},
		{p.toolName, `{"city":42}`, Fail},
		{p.toolName, `{"City":"Reykjavik"}`, Fail},
		{"get_weather_wrong", `{"city":"Reykjavik"}`, Fail},
	} {
		tool := p.qualityTool([]Call{{ToolCalls: []ToolCall{{Name: test.name, Arguments: test.arguments}}}})
		if tool.Status != test.want {
			t.Errorf("%s %s: tool=%+v, want %s", test.name, test.arguments, tool, test.want)
		}
	}
	good := p.qualityStructured([]Call{{Text: `{"value":21,"nonce":"LS-example"}`}})
	bad := p.qualityStructured([]Call{{Text: `{"value":"21","nonce":"LS-example"}`}})
	if good.Status != Pass || bad.Status != Fail {
		t.Errorf("structured: good=%+v bad=%+v", good, bad)
	}
}

func TestQualityMetricsUsesOnlyShortVisibleSamples(t *testing.T) {
	long := Call{Item: "reasoning", Text: "1", VisibleFirstMS: 9000, VisibleLastMS: 10000, VisibleEvents: 8, OutputTokens: 90, UsageReported: true, ReasoningReported: true}
	short := long
	short.Item, short.VisibleFirstMS, short.VisibleLastMS, short.OutputTokens, short.ReasoningTokens = "instruction", 300, 900, 20, 2
	lateTokenProbe := short
	lateTokenProbe.Item, lateTokenProbe.VisibleFirstMS, lateTokenProbe.OutputTokens = "tokens", 8000, 100
	calls := []Call{{Item: "tokens", Error: "HTTP 403"}, {Item: "tokens", Error: "HTTP 403"}, {Item: "tokens", Error: "HTTP 403"}, lateTokenProbe, long, short}
	metrics, _ := qualityMetrics(calls, false)
	if metrics["ttft_samples"] != 1 || metrics["output_tps_samples"] != 0 || metrics["first_token_ms"] != 300 || metrics["output_tps_missing_reason"] == nil {
		t.Fatalf("short sample metrics: %+v", metrics)
	}
	short.OutputTokens = 32
	metrics, _ = qualityMetrics([]Call{short, long}, false)
	if metrics["output_tps_samples"] != 1 || metrics["tokens_per_sec"] != 50.0 {
		t.Fatalf("output speed: %+v", metrics)
	}
	metrics, _ = qualityMetrics([]Call{long}, false)
	if metrics["ttft_samples"] != 0 || metrics["ttft_missing_reason"] == nil || metrics["output_tps_missing_reason"] == nil {
		t.Fatalf("missing reasons: %+v", metrics)
	}
}

func TestQualityReportTimeoutAndRuleSnapshot(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	report, err := Run(ctx, func(context.Context, Request) Call {
		t.Fatal("过期上下文不应发送请求")
		return Call{}
	}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.StopReason != "timeout" || report.StopMessage == "" || report.Verdict != "无法判定" || report.Score != nil ||
		report.FormatVersion != 3 || report.RuleVersion == "" || report.BaselineID == "" || report.Counts != (RunCounts{Planned: 34, Skipped: 34}) {
		t.Fatalf("timeout report: stop=%s verdict=%s counts=%+v", report.StopReason, report.Verdict, report.Counts)
	}
	rules := map[string]ReportRule{}
	for _, rule := range report.Rules {
		rules[rule.ID] = rule
		if rule.Version != report.RuleVersion || rule.Evidence == "" || rule.Source == "" {
			t.Errorf("missing audit metadata: %+v", rule)
		}
	}
	for _, block := range report.Blocks {
		for _, item := range block.Items {
			if rules[item.RuleID].Block != block.Key {
				t.Errorf("rule for %+v is missing or in the wrong block", item)
			}
		}
	}
}

func TestQualityQuotaKeepsCompletedEvidence(t *testing.T) {
	report, err := Run(context.Background(), func(_ context.Context, req Request) Call {
		if req.Item == "chain" {
			return Call{HTTPStatus: http.StatusPaymentRequired}
		}
		return Call{Text: "1", Done: true, ReasoningReported: req.Item == "reasoning", ReasoningTokens: 0}
	}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.StopReason != "quota" || report.Verdict != "不通过" || report.Score != nil ||
		report.Counts != (RunCounts{Planned: 34, Sent: 33, Completed: 30, Failed: 3, Skipped: 1}) {
		t.Fatalf("quota report: stop=%s verdict=%s counts=%+v", report.StopReason, report.Verdict, report.Counts)
	}
	if item := findQualityItem(report.Blocks[0].Items, "reasoning_usage"); item.Status != Fail {
		t.Errorf("missing completed zero reasoning samples: %+v", item)
	}
}

// 这些夹具按案例摘要合成，只验证摘要涉及的项目，不重建历史报告。
func TestSyntheticQualityCases(t *testing.T) {
	suite, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	reasoning := func(claude bool, recurrenceWrong int) []Item {
		p := &plan{suite: suite, rng: rand.New(rand.NewPCG(27, 1)), protocol: claude}
		p.buildQualityReasoning()
		var calls []Call
		for index, problem := range p.math {
			answer := fmt.Sprint(problem.Answer)
			if index < recurrenceWrong {
				answer = "错误"
			}
			calls = append(calls, Call{Item: "reasoning", Text: answer, Effort: p.qualityReasoningEffort(), ReasoningReported: true, ReasoningTokens: 1, SignedThinking: 1, Done: true})
		}
		return p.qualityReasoning(calls, nil)
	}
	t.Run("超超", func(t *testing.T) {
		answers := findQualityItem(reasoning(false, 3), "reasoning_answers")
		injection := qualityInjection([]Call{qualityAnchorCall(9845 + 18800)}, Spec{Disguise: true, TemplateHash: "4468e298e601"})
		if answers.Status != Info || !strings.Contains(answers.Summary, "递推错 3/5") || injection.Status != Warn || injection.Details["extra_tokens"] != int64(18800) {
			t.Errorf("answers=%+v injection=%+v", answers, injection)
		}
	})
	t.Run("aresai", func(t *testing.T) {
		item := qualityCache([]Call{qualityChainCall(21000, 0), qualityChainCall(22000, 19360), qualityChainCall(23000, 20240), qualityChainCall(24000, 21120)}, false)
		if item.Status != Warn || item.Details["rates_suppressed"] != true || strings.Contains(item.Summary, "88%") {
			t.Errorf("cache=%+v", item)
		}
	})
	t.Run("pikaqiu", func(t *testing.T) {
		answers := findQualityItem(reasoning(false, 1), "reasoning_answers")
		item := qualityCache([]Call{qualityChainCall(21400, 0), qualityChainCall(22927, 20864), qualityChainCall(24475, 22272), qualityChainCall(26303, 23936)}, false)
		if answers.Status != Info || !strings.Contains(answers.Summary, "递推错 1/5") || item.Status != Pass || !strings.Contains(item.Summary, "91.0%") {
			t.Errorf("answers=%+v cache=%+v", answers, item)
		}
	})
	t.Run("kirisame", func(t *testing.T) {
		items := reasoning(true, 0)
		signature, answers := findQualityItem(items, "reasoning_signature"), findQualityItem(items, "reasoning_answers")
		if signature.Status != Pass || answers.Status != Info || !strings.Contains(answers.Summary, "总答对 4/4") {
			t.Errorf("signature=%+v answers=%+v", signature, answers)
		}
	})
}
