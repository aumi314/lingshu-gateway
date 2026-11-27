package probe

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestQualityReasoningPlan(t *testing.T) {
	suite, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		model, effort string
		counts        map[string]int
	}{
		{"gpt-6.1-sol", "medium", map[string]int{"recurrence_hard": 5, "subset_mod": 3, "lattice_hard": 2}},
		{"claude-opus-5-5", "high", map[string]int{"recurrence_hard": 2, "subset_mod": 1, "lattice_hard": 1}},
	} {
		t.Run(test.model, func(t *testing.T) {
			for seed := uint64(1); seed <= 40; seed++ {
				p := &plan{suite: suite, rng: rand.New(rand.NewPCG(seed, seed>>1|1)), spec: Spec{Baseline: test.model, Mode: ModeQuality}, protocol: IsProtocolModel(test.model)}
				if p.protocol {
					p.buildProtocolQuality()
				} else {
					p.buildQuality()
				}
				seen := map[string]bool{}
				counts := map[string]int{}
				for _, req := range p.reqs {
					if req.Item != "reasoning" {
						if req.Session >= Sessions || req.Item == "effort" {
							t.Fatalf("seed %d: unexpected %s request in session %d", seed, req.Item, req.Session)
						}
						continue
					}
					problem := p.math[len(seen)]
					if seen[req.Prompt] || req.Prompt != problem.Question || req.Session != Sessions+len(seen) || req.Effort != test.effort {
						t.Fatalf("seed %d: invalid reasoning request: %+v", seed, req)
					}
					seen[req.Prompt] = true
					counts[problem.Kind]++
				}
				if !reflect.DeepEqual(counts, test.counts) {
					t.Errorf("seed %d: kinds = %v, want %v", seed, counts, test.counts)
				}
			}
		})
	}
}

func TestQualityReasoningJudgement(t *testing.T) {
	suite, _, _ := Load()
	base := &Baseline{Math: map[string]MathBaseline{
		"recurrence_hard": {Runs: 3, Correct: 3, MedianReasoning: 8000},
		"subset_mod":      {Runs: 3, Correct: 2},
	}}
	for _, test := range []struct {
		name     string
		protocol bool
		alter    func([]Call)
		status   Status
		contains []string
		excludes []string
		unscored []int
	}{
		{name: "GPT full", status: Pass, contains: []string{"仅覆盖 medium", "有效 10 / 10 题（递推 5）", "实际使用 10 个会话", "答对 10 / 10"}},
		{name: "all explicitly zero", alter: func(calls []Call) {
			for i := range calls {
				calls[i].ReasoningTokens = 0
			}
			calls[1].Text = "0"
		}, status: Fail, contains: []string{"推理被跳过", "会话 4，答对", "会话 5，答错", "会话 13，答对"}},
		{name: "mixed positive and missing", alter: func(calls []Call) {
			calls[0].ReasoningReported, calls[0].ReasoningTokens = false, 0
		}, status: Pass, contains: []string{"1 题推理用量不可核验"}, excludes: []string{"推理被跳过"}},
		{name: "zero before coverage", alter: func(calls []Call) {
			for i := 1; i < 8; i++ {
				calls[i].Error = "超时"
			}
			calls[0].ReasoningTokens = 0
		}, status: Fail, contains: []string{"有效 3 / 10", "会话 4，答对"}, excludes: []string{"推理样本不足"}, unscored: []int{1, 2}},
		{name: "partial text ignored and recurrence coverage required", alter: func(calls []Call) {
			calls[0].FinishReason, calls[1].FinishReason = "incomplete", "length"
			calls[0].ReasoningTokens, calls[1].ReasoningTokens = 0, 0
			calls[1].Text = "0"
		}, status: Info, contains: []string{"推理样本不足", "有效 8 / 10 题（递推 3）", "答对 8 / 8"}, excludes: []string{"推理被跳过", "题型答错"}, unscored: []int{0, 1}},
		{name: "GPT total coverage required", alter: func(calls []Call) {
			for i := 5; i < 9; i++ {
				calls[i].Error = "HTTP 402"
			}
			calls[5].Error = "检测已取消"
		}, status: Info, contains: []string{"有效 6 / 10 题（递推 5）", "实际使用 9 个会话"}},
		{name: "other kinds observed only", alter: func(calls []Call) {
			calls[5].Text, calls[8].Text = "0", "0"
		}, status: Pass, contains: []string{"答对 8 / 10"}, excludes: []string{"建议换个时段复测", "题型答错"}},
		{name: "one expected wrong", alter: func(calls []Call) {
			calls[0].Text = "0"
		}, status: Warn, contains: []string{"官方样本全对的题型答错 1 题"}, excludes: []string{"建议换个时段复测"}},
		{name: "two expected wrong", alter: func(calls []Call) {
			calls[0].Text, calls[1].Text = "0", "0"
		}, status: Warn, contains: []string{"建议换个时段复测"}},
		{name: "actual default after fallback", alter: func(calls []Call) {
			calls[0].EffortSent, calls[0].ReasoningTokens = "default", 0
		}, status: Pass, excludes: []string{"推理被跳过"}},
		{name: "actual high counts for GPT", alter: func(calls []Call) {
			calls[0].EffortSent, calls[0].ReasoningTokens = "high", 0
		}, status: Fail, contains: []string{"推理被跳过"}},
		{name: "Claude low positive is allowed", protocol: true, status: Pass, contains: []string{"仅覆盖 high", "有效 4 / 4 题（递推 2）", "实际使用 4 个会话"}},
		{name: "Claude two wrong only warn", protocol: true, alter: func(calls []Call) {
			calls[0].Text, calls[1].Text = "0", "0"
		}, status: Warn, contains: []string{"建议换个时段复测"}},
		{name: "Claude missing usage and signed thinking independent", protocol: true, alter: func(calls []Call) {
			calls[0].ReasoningReported, calls[0].ReasoningTokens, calls[1].SignedThinking = false, 0, 0
		}, status: Fail, contains: []string{"1 题推理用量不可核验", "缺少带签名的 thinking", "会话 5，答对"}, excludes: []string{"推理被跳过"}},
		{name: "Claude explicit zero with signature", protocol: true, alter: func(calls []Call) {
			calls[0].ReasoningTokens = 0
		}, status: Fail, contains: []string{"推理被跳过"}, excludes: []string{"缺少带签名的 thinking"}},
		{name: "Claude recurrence coverage", protocol: true, alter: func(calls []Call) {
			calls[0].FinishReason, calls[0].SignedThinking = "max_tokens", 0
		}, status: Info, contains: []string{"推理样本不足", "有效 3 / 4 题（递推 1）"}, excludes: []string{"缺少带签名的 thinking"}, unscored: []int{0}},
		{name: "Claude total coverage", protocol: true, alter: func(calls []Call) {
			calls[2].Error, calls[3].Error = "HTTP 500", "超时"
		}, status: Info, contains: []string{"有效 2 / 4 题（递推 2）"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &plan{suite: suite, rng: rand.New(rand.NewPCG(9, 5)), protocol: test.protocol}
			p.buildQualityReasoning()
			calls := make([]Call, len(p.math))
			for i, problem := range p.math {
				calls[i] = Call{Session: p.reqs[i].Session, Effort: p.reqs[i].Effort, Text: fmt.Sprint(problem.Answer), ReasoningReported: true, ReasoningTokens: 1, SignedThinking: 1, Done: true}
			}
			if test.alter != nil {
				test.alter(calls)
			}
			item := p.judgeQualityReasoning(calls, base)
			if item.Status != test.status {
				t.Errorf("status = %s, want %s: %s", item.Status, test.status, item.Summary)
			}
			for _, text := range test.contains {
				if !strings.Contains(item.Summary, text) {
					t.Errorf("summary %q missing %q", item.Summary, text)
				}
			}
			for _, text := range test.excludes {
				if strings.Contains(item.Summary, text) {
					t.Errorf("summary %q contains %q", item.Summary, text)
				}
			}
			rows := item.Details["problems"].([]map[string]any)
			for _, index := range test.unscored {
				if rows[index]["error"] == nil || rows[index]["got"] != nil || rows[index]["correct"] != nil {
					t.Errorf("row %d is scored: %v", index, rows[index])
				}
			}
			if test.status == Info {
				report := &Report{Items: []Item{item, {Key: "tokens", Status: Pass}}}
				finalize(report, "")
				if report.Score == nil || *report.Score != 100 || !strings.Contains(report.Verdict, "推理未完成") {
					t.Errorf("incomplete reasoning must not affect score: %+v", report)
				}
			}
		})
	}
}

func TestQualityReasoningTruncationCountsAsFailed(t *testing.T) {
	report, err := Run(context.Background(), func(_ context.Context, request Request) Call {
		if request.Item == "reasoning" {
			return Call{Text: "partial 123", FinishReason: "incomplete", ReasoningReported: true}
		}
		return Call{Text: "ok"}
	}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (RunCounts{Planned: 34, Sent: 34, Completed: 24, Failed: 10}); report.Counts != want {
		t.Errorf("counts = %+v, want %+v", report.Counts, want)
	}
	if item := itemOf(report, "reasoning"); item.Status != Info || !strings.Contains(report.Verdict, "推理未完成") {
		t.Errorf("reasoning item = %+v, verdict = %q", item, report.Verdict)
	}
}
