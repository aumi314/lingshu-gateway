package probe

import (
	"fmt"
	"net/http"
	"strings"
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

func qualityReasoningFailure(c Call) string {
	if c.Error != "" {
		return c.Error
	}
	if c.HTTPStatus >= http.StatusBadRequest {
		return fmt.Sprintf("HTTP %d", c.HTTPStatus)
	}
	switch c.FinishReason {
	case "incomplete", "length", "max_tokens", "model_context_window_exceeded", "content_filter", "failed", "error", "in_progress", "queued", "cancelled":
		return c.FinishReason
	}
	return ""
}

func (p *plan) judgeQualityReasoning(calls []Call, base *Baseline) Item {
	targetEffort := p.qualityReasoningEffort()
	it := Item{Key: "reasoning", Title: "推理能力", Status: Pass}
	minValid, minRecurrence := 7, 4
	if p.protocol {
		it.Title = "推理"
		minValid, minRecurrence = 3, 2
	}
	valid, recurrence, correct, unknown, wrongExpected := 0, 0, 0, 0, 0
	sessions := map[int]bool{}
	rows := make([]map[string]any, 0, len(calls))
	var zero, unsigned []string
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
		if failure := qualityReasoningFailure(c); failure != "" {
			row["error"] = failure
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
		answerStatus := "答错"
		if right {
			correct++
			answerStatus = "答对"
		}
		if base != nil {
			ref := base.Math[problem.Kind]
			if ref.Runs > 0 {
				row["baseline_reasoning"], row["baseline_correct"] = ref.MedianReasoning, fmt.Sprintf("%d/%d", ref.Correct, ref.Runs)
			}
			if !right && ref.Runs > 0 && ref.Correct == ref.Runs {
				wrongExpected++
			}
		}
		label := fmt.Sprintf("第 %d 题 %s（会话 %d，%s）", i+1, problem.Kind, c.Session, answerStatus)
		if !c.ReasoningReported {
			unknown++
		} else if c.ReasoningTokens == 0 && (effort == "high" || effort == "xhigh" || !p.protocol && effort == "medium") {
			zero = append(zero, label)
		}
		if p.protocol {
			row["signed_thinking"] = c.SignedThinking
			if c.SignedThinking == 0 {
				unsigned = append(unsigned, label)
			}
		}
	}
	coverage := fmt.Sprintf("仅覆盖 %s；有效 %d / %d 题（递推 %d），实际使用 %d 个会话，答对 %d / %d", targetEffort, valid, len(p.math), recurrence, len(sessions), correct, valid)
	var notes []string
	if len(zero) > 0 {
		notes = append(notes, "推理被跳过（推理 token 为 0）："+strings.Join(zero, "、"))
	}
	if len(unsigned) > 0 {
		notes = append(notes, "缺少带签名的 thinking："+strings.Join(unsigned, "、"))
	}
	switch {
	case len(zero) > 0 || len(unsigned) > 0:
		it.Status = Fail
	case valid < minValid || recurrence < minRecurrence:
		it.Status = Info
		notes = append(notes, fmt.Sprintf("推理样本不足：至少需要 %d 道有效题，其中递推至少 %d 道", minValid, minRecurrence))
	case wrongExpected > 0:
		it.Status = Warn
	}
	notes = append(notes, coverage)
	if unknown > 0 {
		notes = append(notes, fmt.Sprintf("%d 题推理用量不可核验", unknown))
	}
	if wrongExpected > 0 {
		notes = append(notes, fmt.Sprintf("官方样本全对的题型答错 %d 题", wrongExpected))
	}
	if wrongExpected >= 2 {
		notes = append(notes, "建议换个时段复测")
	}
	it.Summary = strings.Join(notes, "；")
	it.Details = map[string]any{"problems": rows, "zero_reasoning": len(zero)}
	return it
}
