package probe

import (
	"encoding/json"
	"net/http"
	"strings"
)

type AttemptUsage struct {
	Fields     map[string]*int64 `json:"fields"`
	Input      int64             `json:"input_tokens"`
	CacheRead  int64             `json:"cache_read_tokens"`
	CacheWrite int64             `json:"cache_write_tokens"`
	Output     int64             `json:"output_tokens"`
}

type Attempt struct {
	Question     int          `json:"question"`
	Item         string       `json:"item"`
	Session      int          `json:"session"`
	Number       int          `json:"number"`
	RetryReason  string       `json:"retry_reason,omitempty"`
	Effort       string       `json:"effort_sent"`
	HTTPStatus   int          `json:"http_status"`
	ErrorType    string       `json:"error_type,omitempty"`
	Done         bool         `json:"done"`
	HasContent   bool         `json:"has_content"`
	Usable       bool         `json:"usable"`
	FirstTokenMS int          `json:"first_token_ms"`
	TotalMS      int          `json:"total_ms"`
	Usage        AttemptUsage `json:"usage"`
}

type Availability struct {
	SentQuestions int `json:"sent_questions"`
	FirstUsable   int `json:"first_usable"`
	FinalUsable   int `json:"final_usable"`
	Sends         int `json:"sends"`
	Cancelled     int `json:"cancelled"`
	Unscheduled   int `json:"unscheduled"`
}

func newAttempt(req Request, question, number int, retry string, c Call) Attempt {
	content := hasAnswerContent(req, c)
	kind := attemptError(c, !content)
	effort := c.EffortSent
	if effort == "" {
		effort = "default"
	}
	usage := c.WireUsage
	if usage.Fields == nil && c.UsageReported {
		usage.Input = max(0, c.InputTokens-c.CachedTokens)
		usage.CacheRead, usage.Output = c.CachedTokens, c.OutputTokens
	}
	return Attempt{
		Question: question, Item: req.Item, Session: req.Session, Number: number,
		RetryReason: retry, Effort: effort, HTTPStatus: c.HTTPStatus, ErrorType: kind,
		Done: c.Done, HasContent: content, Usable: kind == "" && content,
		FirstTokenMS: c.VisibleFirstMS, TotalMS: c.TotalMS, Usage: usage,
	}
}

func hasAnswerContent(req Request, c Call) bool {
	if len(req.Tools) == 0 {
		return clean(c.Text) != ""
	}
	for _, tool := range req.Tools {
		name, _ := tool["name"].(string)
		if fn, ok := tool["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
		}
		for _, tc := range c.ToolCalls {
			var args map[string]json.RawMessage
			if name != "" && tc.Name == name && json.Unmarshal([]byte(tc.Arguments), &args) == nil && args != nil {
				return true
			}
		}
	}
	return false
}

func attemptError(c Call, missingContent bool) string {
	msg := strings.ToLower(c.Error)
	switch {
	case quotaError(c, msg):
		return "quota"
	case c.HTTPStatus == 529 || c.HTTPStatus == http.StatusServiceUnavailable || strings.Contains(msg, "overloaded"):
		return "overloaded"
	case c.HTTPStatus == http.StatusTooManyRequests || strings.Contains(msg, "rate_limit") || strings.Contains(msg, "too many requests"):
		return "rate_limit"
	case gatewayError(c):
		return "gateway_error"
	case c.HTTPStatus == http.StatusForbidden && strings.Contains(msg, "policy_blocked"):
		return "blocked"
	case c.HTTPStatus >= 300:
		return "http_error"
	case c.AttemptError != "":
		return c.AttemptError
	case qualityCallIssue(Call{FinishReason: c.FinishReason}) != "":
		return "incomplete"
	case c.Error != "" && !strings.Contains(msg, "流没有正常结束") && !strings.Contains(msg, "流在正常结束前断开"):
		return "request_error"
	case !c.Done && !c.BodyComplete:
		return "stream_interrupted"
	case missingContent:
		return "missing_content"
	case c.Error != "":
		return "request_error"
	default:
		return ""
	}
}

func summarizeAttempts(byQuestion [][]Attempt, scheduled, cancelled []bool) ([]Attempt, *Availability) {
	var attempts []Attempt
	var summary Availability
	for i, question := range byQuestion {
		if !scheduled[i] {
			summary.Unscheduled++
		}
		if cancelled[i] {
			summary.Cancelled++
		}
		if len(question) == 0 {
			continue
		}
		attempts = append(attempts, question...)
		summary.SentQuestions++
		if question[0].Usable {
			summary.FirstUsable++
		}
		for _, attempt := range question {
			if attempt.Usable {
				summary.FinalUsable++
				break
			}
		}
	}
	summary.Sends = len(attempts)
	return attempts, &summary
}
