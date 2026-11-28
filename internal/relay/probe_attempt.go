package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/relay/usage"
)

func probeRetryAfter(header http.Header) *time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil && seconds <= uint64((1<<63-1)/time.Second) {
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := max(0, time.Until(date))
		return &delay
	}
	return nil
}

type probeUsage struct {
	*usage.Tracker
	fields map[string]*int64
}

func newProbeUsage(protocol usage.Protocol) *probeUsage {
	return &probeUsage{Tracker: usage.NewTracker(protocol), fields: map[string]*int64{}}
}

func (u *probeUsage) ObserveEvent(data []byte) {
	u.Tracker.ObserveEvent(data)
	u.capture(data)
}

func (u *probeUsage) ObserveBody(data []byte) {
	u.Tracker.ObserveBody(data)
	u.capture(data)
}

func (u *probeUsage) capture(data []byte) {
	type envelope struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	var event struct {
		envelope
		Message  envelope `json:"message"`
		Response envelope `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	for _, fields := range []map[string]json.RawMessage{event.Message.Usage, event.Response.Usage, event.Usage} {
		copyUsageFields(u.fields, "", fields)
	}
}

func copyUsageFields(dst map[string]*int64, prefix string, fields map[string]json.RawMessage) {
	for key, raw := range fields {
		switch key {
		case "input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens",
			"cache_read_input_tokens", "cache_creation_input_tokens", "cached_tokens", "cache_write_tokens",
			"reasoning_tokens", "thinking_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens",
			"audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens":
			var n *int64
			if json.Unmarshal(raw, &n) == nil {
				dst[prefix+key] = n
			}
		case "cache_creation", "input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details":
			if prefix == "" {
				var details map[string]json.RawMessage
				if json.Unmarshal(raw, &details) == nil {
					copyUsageFields(dst, key+".", details)
				}
			}
		}
	}
}

func (u *probeUsage) finish(c *probe.Call, req probe.Request, effort string, start time.Time) {
	normalized := u.Result()
	c.WireUsage = probe.AttemptUsage{Fields: u.fields, Input: normalized.Input,
		CacheRead: normalized.CacheRead, CacheWrite: normalized.CacheWrite, Output: normalized.Output}
	c.TotalMS = max(1, int(time.Since(start).Milliseconds()))
	if req.OnAttempt != nil {
		result := *c
		result.EffortSent = effort
		req.OnAttempt(result, req.RetryReason)
	}
}

func probeReadError(ctx context.Context, err error, fallback string) string {
	var timeout net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	default:
		return fallback
	}
}

func captureProbeError(resp *http.Response, body []byte, tracker *probeUsage) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		reader := usage.NewSSEReader(strings.NewReader(string(body)))
		for {
			event, err := reader.Next()
			if err != nil {
				break
			}
			tracker.ObserveEvent(event.Data)
		}
		return
	}
	tracker.ObserveBody(body)
	tracker.ObserveEvent(body)
}
