package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aumi314/lingshu/internal/probe"
	"github.com/aumi314/lingshu/internal/store"
)

type probeRetryTransport struct {
	status int
	header http.Header
}

func (t probeRetryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{"error": "rate limited"})
	return &http.Response{StatusCode: t.status, Header: t.header, Body: io.NopCloser(bytes.NewReader(body)), Request: &http.Request{}}, nil
}

func TestProbeCallersCaptureRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Handler, *store.Channel) probe.Call
	}{
		{
			name: "responses",
			call: func(h *Handler, ch *store.Channel) probe.Call {
				return h.ProbeResponses(context.Background(), ch, "gpt-6.2-sol", "sess", probe.Request{Prompt: "hi"})
			},
		},
		{
			name: "messages",
			call: func(h *Handler, ch *store.Channel) probe.Call {
				return h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "sess", probe.Request{Prompt: "hi"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("Retry-After", "17")
			h := &Handler{Client: &http.Client{Transport: probeRetryTransport{status: http.StatusTooManyRequests, header: header}}}
			ch := &store.Channel{BaseURLResponses: "http://up.invalid", BaseURLMessages: "http://up.invalid", APIKey: "k"}
			c := tc.call(h, ch)
			if c.HTTPStatus != http.StatusTooManyRequests || c.RetryAfter == nil || *c.RetryAfter != 17*time.Second {
				t.Fatalf("RetryAfter = %v, status = %d, err = %s", c.RetryAfter, c.HTTPStatus, c.Error)
			}
		})
	}
}

func TestProbeCallersLeaveMissingRetryAfterNil(t *testing.T) {
	h := &Handler{Client: &http.Client{Transport: probeRetryTransport{status: http.StatusTooManyRequests, header: http.Header{}}}}
	ch := &store.Channel{BaseURLResponses: "http://up.invalid", BaseURLMessages: "http://up.invalid", APIKey: "k"}
	for _, c := range []probe.Call{
		h.ProbeResponses(context.Background(), ch, "gpt-6.2-sol", "sess", probe.Request{Prompt: "hi"}),
		h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "sess", probe.Request{Prompt: "hi"}),
	} {
		if c.HTTPStatus != http.StatusTooManyRequests || c.RetryAfter != nil {
			t.Fatalf("missing Retry-After should stay nil: %+v", c)
		}
	}
}

func TestProbeCallersCaptureExplicitZeroRetryAfter(t *testing.T) {
	header := make(http.Header)
	header.Set("Retry-After", "0")
	h := &Handler{Client: &http.Client{Transport: probeRetryTransport{status: 529, header: header}}}
	ch := &store.Channel{BaseURLResponses: "http://up.invalid", BaseURLMessages: "http://up.invalid", APIKey: "k"}
	for _, c := range []probe.Call{
		h.ProbeResponses(context.Background(), ch, "gpt-6.2-sol", "sess", probe.Request{Prompt: "hi"}),
		h.ProbeMessages(context.Background(), ch, "claude-opus-5-5", "sess", probe.Request{Prompt: "hi"}),
	} {
		if c.RetryAfter == nil || *c.RetryAfter != 0 {
			t.Fatalf("explicit zero Retry-After should be non-nil: %+v", c)
		}
	}
}
