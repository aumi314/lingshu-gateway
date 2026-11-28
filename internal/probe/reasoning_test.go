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
	for _, test := range []struct {
		name          string
		claude        bool
		alter         func([]Call)
		verdict       string
		item, summary string
	}{
		{"三道递推答错只观察", false, func(calls []Call) { calls[0].Text, calls[1].Text, calls[2].Text = "错误", "错误", "错误" }, "通过", "reasoning_answers", "递推错 3/5"},
		{"明确零推理优先于缺测", false, func(calls []Call) {
			calls[0].ReasoningTokens = 0
			for index := 1; index < len(calls); index++ {
				calls[index].Error = "检测已取消"
			}
		}, "不通过", "reasoning_usage", "明确上报 0"},
		{"缺失用量和零推理分开", false, func(calls []Call) { calls[0].ReasoningReported, calls[0].ReasoningTokens = false, 0 }, "无法判定", "reasoning_usage", "未上报"},
		{"截断不取答案和用量", false, func(calls []Call) {
			calls[0].FinishReason, calls[1].FinishReason, calls[0].ReasoningTokens = "incomplete", "length", 0
		}, "无法判定", "reasoning_coverage", "有效 8/10"},
		{"实发强度不符不判零推理", false, func(calls []Call) { calls[0].EffortSent, calls[0].ReasoningTokens = "default", 0 }, "通过", "reasoning_coverage", "有效 9/10"},
		{"Claude 签名独立于用量", true, func(calls []Call) { calls[0].SignedThinking, calls[0].ReasoningReported = 0, false }, "不通过", "reasoning_signature", "缺少非空 thinking 签名"},
		{"Claude 递推覆盖不足", true, func(calls []Call) { calls[0].FinishReason, calls[0].SignedThinking = "max_tokens", 0 }, "无法判定", "reasoning_coverage", "其中递推 1 题"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &plan{suite: suite, rng: rand.New(rand.NewPCG(9, 5)), protocol: test.claude}
			p.buildQualityReasoning()
			calls := make([]Call, len(p.math))
			for index, problem := range p.math {
				calls[index] = Call{Item: "reasoning", Session: p.reqs[index].Session, Effort: p.reqs[index].Effort, Text: fmt.Sprint(problem.Answer), ReasoningReported: true, ReasoningTokens: 1, SignedThinking: 1, Done: true}
			}
			test.alter(calls)
			items := p.qualityReasoning(calls, nil)
			for index, item := range items {
				items[index].RuleID = item.Key
			}
			if got := qualityVerdict(items); got != test.verdict {
				t.Errorf("verdict = %q, want %q: %+v", got, test.verdict, items)
			}
			if got := findQualityItem(items, test.item); !strings.Contains(got.Summary, test.summary) {
				t.Errorf("item = %+v, want %q", got, test.summary)
			}
		})
	}
}

func findQualityItem(items []Item, key string) Item {
	for _, item := range items {
		if item.Key == key {
			return item
		}
	}
	return Item{}
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
	if item := findQualityItem(report.Blocks[0].Items, "reasoning_coverage"); item.Status != Unknown {
		t.Errorf("reasoning item = %+v, verdict = %q", item, report.Verdict)
	}
}
