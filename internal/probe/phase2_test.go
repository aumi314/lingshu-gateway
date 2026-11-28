package probe

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestQualityThrottlePausesWholeRun(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		retryAfter *time.Duration
		pause      time.Duration
	}{
		{"429 with header", 429, new(7 * time.Second), 7 * time.Second},
		{"529 without header", 529, nil, 20 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var started, active, peak, afterPeak atomic.Int32
				startBody := time.Now().Add(time.Second)
				resume := startBody.Add(time.Second + test.pause)
				report, err := Run(t.Context(), func(_ context.Context, req Request) Call {
					seq := started.Add(1)
					count := active.Add(1)
					defer active.Add(-1)
					if seq > 6 {
						if time.Now().Before(resume) {
							t.Errorf("request %d before cooldown ended", seq)
						}
						updatePeak(&afterPeak, count)
					} else if seq > 3 {
						updatePeak(&peak, count)
					}
					time.Sleep(time.Second)
					if seq >= 4 && seq <= 6 {
						return Call{HTTPStatus: test.status, RetryAfter: test.retryAfter}
					}
					call := Call{HTTPStatus: 200, Done: true, Text: "1"}
					if len(req.Tools) != 0 {
						fn := req.Tools[0]["function"].(map[string]any)
						call.ToolCalls = []ToolCall{{Name: fn["name"].(string), Arguments: `{}`}}
					}
					return call
				}, Spec{Baseline: "gpt-6.1-sol", Mode: ModeQuality, Seed: 1}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if peak.Load() != 3 || afterPeak.Load() != 2 {
					t.Errorf("concurrency before/after throttle: %d/%d", peak.Load(), afterPeak.Load())
				}
				if report.StopReason != "" || *report.Availability != (Availability{SentQuestions: 38, FirstUsable: 35, FinalUsable: 38, Sends: 41}) {
					t.Fatalf("stop=%q availability=%+v", report.StopReason, report.Availability)
				}
			})
		})
	}
}

func updatePeak(peak *atomic.Int32, count int32) {
	for current := peak.Load(); count > current; current = peak.Load() {
		if peak.CompareAndSwap(current, count) {
			return
		}
	}
}

func TestQualityAvailabilityCutoffAndFailures(t *testing.T) {
	for _, test := range []struct {
		done, usable int
		want         Status
	}{
		{9, 0, Unknown}, {10, 7, Fail}, {10, 8, Pass},
	} {
		item := qualityAvailabilityItem(nil, &Availability{SentQuestions: 13}, availabilityProgress{completed: test.done, usable: test.usable})
		if item.Status != test.want {
			t.Errorf("%d/%d: %+v", test.usable, test.done, item)
		}
	}
	for i, test := range []struct {
		call Call
		kind string
	}{
		{Call{HTTPStatus: 402}, "quota"}, {Call{HTTPStatus: 429}, "rate_limit"}, {Call{HTTPStatus: 529}, "overloaded"},
		{Call{HTTPStatus: 403, Error: `HTTP 403 {"code":"policy_blocked"}`}, "blocked"}, {Call{HTTPStatus: 403}, "refused"},
		{Call{HTTPStatus: 200, Done: true}, "missing_content"}, {Call{HTTPStatus: 504}, "timeout"}, {Call{HTTPStatus: 200}, "stream_interrupted"},
	} {
		attempt := newAttempt(Request{}, i+1, 1, "", test.call)
		item := qualityAvailabilityItem([]Attempt{attempt}, &Availability{SentQuestions: 1}, availabilityProgress{})
		if counts := item.Details["failure_counts"].(map[string]int); counts[test.kind] != 1 {
			t.Errorf("%+v: %v", test.call, counts)
		}
	}
}

func TestQualityModelMatchAndMix(t *testing.T) {
	const model = "gpt-6.2-sol"
	for _, name := range []string{model, "sol-mapped", "openai/" + model, model + "-2026-11-28", "openai/" + model + "-2026-11-28"} {
		if !modelMatches(name, model, []string{"sol-mapped"}) {
			t.Errorf("expected match for %q", name)
		}
	}
	for _, name := range []string{"gpt-5.6-luna", model + "-preview", "", model + "-2026-xx"} {
		if modelMatches(name, model, nil) {
			t.Errorf("unexpected match for %q", name)
		}
	}
	const luna = "gpt-5.6-luna"
	p := &plan{spec: Spec{UpstreamModel: model}, math: []MathProblem{{Kind: "recurrence_hard", Answer: 42}, {Kind: "recurrence_hard", Answer: 42}, {Kind: "recurrence_hard", Answer: 42}, {Kind: "recurrence_hard", Answer: 42}}}
	short := func(name string, session int, tokens int64) Call {
		return Call{Item: "mix_anchor", Model: name, Session: session, InputTokens: tokens, InputReported: true, Done: true, Text: "1"}
	}
	math := func(name string, session int, text string) Call {
		return Call{Item: "reasoning", Model: name, Session: session, InputTokens: 48000, InputReported: true, Done: true, Text: text}
	}
	anchors := []Call{short(model, 4, 9800), short(luna, 5, 17000), short(luna, 6, 17000)}
	calls := append([]Call{math(model, 4, "42"), math(luna, 5, "0"), math(luna, 6, "42"), math("", 7, "42")}, anchors...)
	item := p.qualityModelMix(calls, anchors)
	if item.Status != Fail || item.Details["mismatch_count"] != 4 || item.Details["mismatch_recurrence_right"] != 1 || item.Details["mismatch_recurrence_wrong"] != 1 || item.Details["unreported"] != 1 || item.Details["successful_sessions"] != 4 || item.Details["feature_groups"] != 2 {
		t.Fatalf("unexpected model mix: %+v", item)
	}
	groups := item.Details["groups"].([]mixGroup)
	if group := groups[1]; group.Model != luna || group.AnchorInput == nil || *group.AnchorInput != 17000 || group.Requests != 4 || group.Sessions != 2 || group.RecurrenceRight != 1 || group.RecurrenceWrong != 1 {
		t.Fatalf("unexpected Luna group: %+v", group)
	}
	item = p.qualityModelMix([]Call{{Item: "anchor", Done: true, Text: "1"}}, nil)
	if item.Status != Pass || !strings.Contains(item.Summary, "型号未上报") || item.Details["unreported"] != 1 {
		t.Fatalf("unreported model: %+v", item)
	}
}

func TestQualityInstructionOutcomes(t *testing.T) {
	p := &plan{onlyWord: "bcdfgh", userWord: "jkmnpq", marker: "[[rstvwxz]]"}
	answer := func(text string) Call { return Call{HTTPStatus: 200, Done: true, Text: text} }
	control := answer("Yellow.\n" + p.marker)
	for _, test := range []struct {
		name              string
		conflict, control Call
		want              Status
		summary           string
	}{
		{"system", answer(p.onlyWord), control, Pass, "听系统"},
		{"user", answer(p.userWord), control, Fail, "听用户"},
		{"neither", answer("other"), answer("Yellow."), Fail, "都没听"},
		{"control omitted", answer(p.onlyWord), answer("Yellow."), Fail, "未加结尾标记"},
		{"empty", answer(" "), control, Unknown, "没测成"},
		{"403", Call{HTTPStatus: http.StatusForbidden}, control, Unknown, "没测成"},
		{"interrupted", Call{HTTPStatus: 200, Text: p.onlyWord}, control, Unknown, "没测成"},
		{"missing control", answer(p.userWord), Call{HTTPStatus: 200, Done: true, FinishReason: "max_tokens"}, Unknown, "没测成"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if item := p.qualityInstruction([]Call{test.conflict, test.control}); item.Status != test.want || !strings.Contains(item.Summary, test.summary) {
				t.Fatalf("%+v", item)
			}
		})
	}
	variants := map[string]bool{}
	for seed := uint64(1); seed <= 40; seed++ {
		p := &plan{rng: rand.New(rand.NewPCG(seed, 1))}
		p.buildInstructions()
		conflict, control := p.reqs[0], p.reqs[1]
		if p.onlyWord == p.userWord || !strings.Contains(conflict.System, p.onlyWord) || !strings.Contains(conflict.Prompt, p.userWord) || !strings.Contains(control.System, p.marker) || strings.Contains(strings.ToLower(conflict.Prompt), "ignore earlier rules") {
			t.Fatalf("conflict=%+v control=%+v", conflict, control)
		}
		variants[strings.Replace(conflict.Prompt, p.userWord, "%s", 1)] = true
		for _, request := range p.reqs {
			if request.Effort != "" || request.MaxTokens < 1024 {
				t.Fatalf("invalid instruction budget: %+v", request)
			}
		}
	}
	if len(variants) < 4 {
		t.Fatalf("only %d prompt variants", len(variants))
	}
}
