package probe

import (
	"fmt"
	"regexp"
	"strings"
)

var modelDateSuffix = regexp.MustCompile(`-(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{8})$`)

func (p *plan) buildMixAnchors() {
	for index, problem := range p.math {
		if problem.Kind == "recurrence_hard" {
			p.add(Request{Item: "mix_anchor", Session: Sessions + index, Prompt: strings.TrimSpace(p.suffix), MaxTokens: 16})
		}
	}
}

func modelMatches(reported, requested string, aliases []string) bool {
	normalize := func(name string) string {
		if prefix, model, ok := strings.Cut(name, "/"); ok && prefix != "" && !strings.Contains(model, "/") {
			name = model
		}
		return name
	}
	matches := func(expected string) bool {
		if expected == "" {
			return false
		}
		got, want := normalize(reported), normalize(expected)
		return reported == expected || got == want || modelDateSuffix.ReplaceAllString(got, "") == want
	}
	if matches(requested) {
		return true
	}
	for _, alias := range aliases {
		if matches(alias) {
			return true
		}
	}
	return false
}

type mixGroup struct {
	Model           string `json:"model"`
	AnchorInput     *int64 `json:"anchor_input_tokens"`
	Requests        int    `json:"requests"`
	Sessions        int    `json:"sessions"`
	RecurrenceRight int    `json:"recurrence_right"`
	RecurrenceWrong int    `json:"recurrence_wrong"`
}

func (p *plan) qualityModelMix(calls, anchors []Call) Item {
	type modelSession struct {
		model   string
		session int
	}
	type groupKey struct {
		model    string
		input    int64
		hasInput bool
	}
	inputs := map[modelSession]map[int64]bool{}
	for _, call := range anchors {
		if !call.InputReported || attemptError(call, clean(call.Text) == "") != "" {
			continue
		}
		key := modelSession{call.Model, call.Session}
		if inputs[key] == nil {
			inputs[key] = map[int64]bool{}
		}
		inputs[key][call.InputTokens] = true
	}
	type groupRow struct {
		mixGroup
		sessions map[int]bool
	}
	groups := map[groupKey]*groupRow{}
	var rows []*groupRow
	var mismatches, evidence []map[string]any
	sessions := map[int]bool{}
	missing, reported, question, right, wrong, recurrenceRight, recurrenceWrong := 0, 0, 0, 0, 0, 0, 0
	for index, call := range calls {
		var problem *MathProblem
		if call.Item == "reasoning" {
			problem = &p.math[question]
			question++
		}
		if qualityCallIssue(call) != "" || attemptError(call, clean(call.Text) == "" && len(call.ToolCalls) == 0) != "" {
			continue
		}
		sessions[call.Session] = true
		key := groupKey{model: call.Model}
		if candidates := inputs[modelSession{call.Model, call.Session}]; len(candidates) == 1 {
			for input := range candidates {
				key.input, key.hasInput = input, true
			}
		}
		// 短基准本身的用量可直接分组，即使同一会话出现多个实测值。
		if call.InputReported && (call.Item == "anchor" || call.Item == "mix_anchor" || call.Item == "tokens" && index < 2) && inputs[modelSession{call.Model, call.Session}][call.InputTokens] {
			key.input, key.hasInput = call.InputTokens, true
		}
		row := groups[key]
		if row == nil {
			row = &groupRow{mixGroup: mixGroup{Model: call.Model}, sessions: map[int]bool{}}
			if key.hasInput {
				row.AnchorInput = &key.input
			}
			groups[key] = row
			rows = append(rows, row)
		}
		row.Requests++
		row.sessions[call.Session] = true
		answer := map[string]any{"question": index + 1, "session": call.Session, "model": call.Model,
			"id_prefix": idPrefix(call.ID), "service_tier_reported": call.ServiceTier != "", "first_token_ms": call.VisibleFirstMS,
			"anchor_input_tokens": row.AnchorInput}
		if problem != nil {
			got, parsed := lastInt(clean(call.Text))
			correct := parsed && got == problem.Answer
			answer["kind"], answer["correct"] = problem.Kind, correct
			if problem.Kind == "recurrence_hard" {
				if correct {
					row.RecurrenceRight++
				} else {
					row.RecurrenceWrong++
				}
			}
		}
		evidence = append(evidence, answer)
		if call.Model == "" {
			missing++
			continue
		}
		reported++
		if p.spec.UpstreamModel != "" && !modelMatches(call.Model, p.spec.UpstreamModel, p.spec.ModelAliases) {
			mismatches = append(mismatches, answer)
			if correct, ok := answer["correct"].(bool); ok {
				if correct {
					right++
					if problem.Kind == "recurrence_hard" {
						recurrenceRight++
					}
				} else {
					wrong++
					if problem.Kind == "recurrence_hard" {
						recurrenceWrong++
					}
				}
			}
		}
	}
	features := 0
	groupDetails := make([]mixGroup, 0, len(rows))
	for _, row := range rows {
		row.Sessions = len(row.sessions)
		if row.AnchorInput != nil {
			features++
		}
		groupDetails = append(groupDetails, row.mixGroup)
	}
	item := Item{Key: "model_mix", Status: Unknown, Details: map[string]any{"requested_model": p.spec.UpstreamModel, "unreported": missing,
		"mismatch_count": len(mismatches), "mismatch_right": right, "mismatch_wrong": wrong, "mismatches": mismatches,
		"mismatch_recurrence_right": recurrenceRight, "mismatch_recurrence_wrong": recurrenceWrong,
		"successful_sessions": len(sessions), "feature_groups": features, "groups": groupDetails, "responses": evidence}}
	switch {
	case len(mismatches) > 0:
		item.Status = Fail
		item.Summary = fmt.Sprintf("型号不符 %d 次（其中推理答对 %d、答错 %d）", len(mismatches), right, wrong)
	case reported > 0 && p.spec.UpstreamModel != "":
		item.Status, item.Summary = Pass, "已上报型号均与请求一致"
	case p.spec.UpstreamModel != "" && len(sessions) > 0:
		// 不报型号按定稿记“未上报”，不判异常，也不算没测成；其余特征照常展示
		item.Status, item.Summary = Pass, "型号未上报，无法核对型号"
	default:
		item.Summary = "没测成：没有成功的响应可核对"
	}
	if features > 1 {
		item.Summary += "；观察到多组短基准响应特征"
	}
	item.Summary += fmt.Sprintf("；型号未上报 %d 次，成功会话 %d 个，可分辨特征组 %d 个；没发现不等于没有", missing, len(sessions), features)
	return item
}
