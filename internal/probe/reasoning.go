package probe

import (
	"fmt"
)

func (p *plan) qualityReasoningEffort() string {
	if p.protocol {
		return "high"
	}
	return p.suite.Math.Effort
}

func (p *plan) buildQualityReasoning() {
	kinds := [3]string{"recurrence_hard", "subset_mod", "lattice_hard"}
	counts := [3]int{5, 3, 2}
	if p.protocol {
		counts = [3]int{2, 1, 1}
	}
	seen := map[string]bool{}
	for i, kind := range kinds {
		for drawn := 0; drawn < counts[i]; {
			problem := NewMathProblem(kind, p.rng)
			if seen[problem.Question] {
				continue
			}
			seen[problem.Question] = true
			p.add(Request{Item: "reasoning", Session: Sessions + len(p.math), Prompt: problem.Question, Effort: p.qualityReasoningEffort(), MaxTokens: 16000})
			p.math = append(p.math, problem)
			drawn++
		}
	}
}

func (p *plan) qualityReasoning(calls []Call, base *Baseline) []Item {
	targetEffort := p.qualityReasoningEffort()
	thresholds := qualityRules["reasoning_coverage"].Thresholds
	minValid, minRecurrence := thresholds["gpt_min_valid"], thresholds["gpt_min_recurrence"]
	if p.protocol {
		minValid, minRecurrence = thresholds["claude_min_valid"], thresholds["claude_min_recurrence"]
	}
	valid, recurrence, correct, recurrenceWrong, unreported := 0, 0, 0, 0, 0
	sessions := map[int]bool{}
	rows := make([]map[string]any, 0, len(calls))
	var zero, unsigned []int
	for i, c := range calls {
		problem := p.math[i]
		if c.Error != "检测已取消" {
			sessions[c.Session] = true
		}
		effort := c.EffortSent
		if effort == "" {
			effort = c.Effort
		}
		row := map[string]any{
			"kind": problem.Kind, "answer": problem.Answer, "session": c.Session, "effort_sent": effort,
			"reasoning_tokens": c.ReasoningTokens, "reasoning_reported": c.ReasoningReported,
		}
		rows = append(rows, row)
		if failure := qualityCallIssue(c); failure != "" {
			row["error"] = failure
			continue
		}
		if effort != targetEffort && effort != "xhigh" && (p.protocol || effort != "high") {
			row["error"] = "实发推理强度未达到 " + targetEffort
			continue
		}
		valid++
		if problem.Kind == "recurrence_hard" {
			recurrence++
		}
		text := clean(c.Text)
		got, ok := lastInt(text)
		right := ok && got == problem.Answer
		row["got"], row["correct"] = truncate(text, 40), right
		if right {
			correct++
		} else if problem.Kind == "recurrence_hard" {
			recurrenceWrong++
		}
		if base != nil {
			ref := base.Math[problem.Kind]
			if ref.Runs > 0 {
				row["baseline_reasoning"], row["baseline_correct"] = ref.MedianReasoning, fmt.Sprintf("%d/%d", ref.Correct, ref.Runs)
			}
		}
		if !c.ReasoningReported {
			unreported++
		} else if c.ReasoningTokens < qualityRules["reasoning_usage"].Thresholds["min_reasoning_tokens"] {
			zero = append(zero, i+1)
		}
		if p.protocol {
			row["signed_thinking"] = c.SignedThinking
			if int64(c.SignedThinking) < qualityRules["reasoning_signature"].Thresholds["min_signed_blocks"] {
				unsigned = append(unsigned, i+1)
			}
		}
	}
	coverage := Item{Key: "reasoning_coverage", Status: Pass, Summary: fmt.Sprintf("%s 强度有效 %d/%d 题，其中递推 %d 题，实发覆盖 %d 个会话", targetEffort, valid, len(p.math), recurrence, len(sessions))}
	if int64(valid) < minValid || int64(recurrence) < minRecurrence {
		coverage.Status = Unknown
		coverage.Summary += fmt.Sprintf("；至少需要 %d 道有效题，其中递推至少 %d 道", minValid, minRecurrence)
	}
	usage := Item{Key: "reasoning_usage", Status: Pass, Summary: fmt.Sprintf("%d 题上报正数推理 token", valid)}
	usage.Details = map[string]any{"zero_questions": zero, "unreported": unreported, "valid": valid}
	switch {
	case len(zero) > 0:
		usage.Status, usage.Summary = Fail, fmt.Sprintf("%d 题明确上报 0 个推理 token", len(zero))
	case valid == 0:
		usage.Status, usage.Summary = Unknown, "没有可核对的完整作答"
	case unreported > 0:
		usage.Status, usage.Summary = Unknown, fmt.Sprintf("%d/%d 题未上报推理用量", unreported, valid)
	}
	if len(zero) > 0 && unreported > 0 {
		usage.Summary += fmt.Sprintf("；另有 %d 题未上报推理用量", unreported)
	}
	items := []Item{coverage, usage}
	if p.protocol {
		signature := Item{Key: "reasoning_signature", Status: Pass, Summary: fmt.Sprintf("%d 题带非空 thinking 签名", valid)}
		if len(unsigned) > 0 {
			signature.Status, signature.Summary = Fail, fmt.Sprintf("%d 题缺少非空 thinking 签名", len(unsigned))
			signature.Details = map[string]any{"unsigned_questions": unsigned}
		} else if valid == 0 {
			signature.Status, signature.Summary = Unknown, "没有可核对的完整作答"
		}
		items = append(items, signature)
	}
	answers := "没有有效作答；判定规则待校准"
	if valid > 0 {
		answers = fmt.Sprintf("递推错 %d/%d，总答对 %d/%d；判定规则待校准", recurrenceWrong, recurrence, correct, valid)
	}
	return append(items, Item{Key: "reasoning_answers", Status: Info, Summary: answers, Details: map[string]any{"problems": rows}})
}
