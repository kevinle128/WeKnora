package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The body a gateway returns when it wraps an upstream 429 in a 503.
const proxiedRateLimitBody = `{"error":{"message":"[codex/gpt-5.6-terra] [429]: {\"detail\":\"Rate limit exceeded\"} (reset after 2s)"}}`

func TestIsRateLimitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &HTTPError{StatusCode: 429, Body: "slow down"}, true},
		{"503 wrapping 429", &HTTPError{StatusCode: 503, Body: proxiedRateLimitBody}, true},
		{"503 with Retry-After", &HTTPError{StatusCode: 503, Header: http.Header{"Retry-After": {"3"}}}, true},
		{"plain 503", &HTTPError{StatusCode: 503, Body: "service unavailable"}, false},
		{"400", &HTTPError{StatusCode: 400, Body: "rate limit"}, false},
		{"transport", &TransportError{Op: "send request", Err: io.EOF}, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsRateLimitError(tc.err), tc.name)
	}
}

func TestParseResetHint(t *testing.T) {
	assert.Equal(t, 2*time.Second, parseResetHint(proxiedRateLimitBody))
	assert.Equal(t, 1500*time.Millisecond, parseResetHint("Please retry in 1.5 seconds"))
	assert.Equal(t, 20*time.Second, parseResetHint("try again in 20s."))
	assert.Equal(t, 300*time.Millisecond, parseResetHint("reset after 300ms"))
	assert.Equal(t, time.Duration(0), parseResetHint("no hint here"))
}

func TestRateLimitWaitPrefersHintAndCaps(t *testing.T) {
	p := RateLimitRetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	hinted := &HTTPError{StatusCode: 503, Body: proxiedRateLimitBody}
	assert.Equal(t, 2250*time.Millisecond, p.rateLimitWait(hinted, 1))

	header := &HTTPError{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}}
	assert.Equal(t, 5*time.Second, p.rateLimitWait(header, 1))

	noHint := &HTTPError{StatusCode: 429}
	assert.Equal(t, 2250*time.Millisecond, p.rateLimitWait(noHint, 2))
}

func TestDoWithRateLimitRetryRecoversAndReplaysBody(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"[429] Rate limit exceeded (reset after 10ms)"}}`))
			return
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer server.Close()

	ep := Endpoint{}
	req, _, err := ep.NewRequest(context.Background(), server.URL, map[string]any{"q": "hi"}, true)
	require.NoError(t, err)
	resp, err := ep.DoWithRateLimitRetry(req, RateLimitRetryPolicy{MaxRetries: 2, MaxDelay: time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, 2, attempts)
	assert.Equal(t, bodies[0], bodies[1])
	assert.Equal(t, `{"q":"hi"}`, bodies[1])
}

func TestDoWithRateLimitRetryStopsOnOtherErrorsAndBudget(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	attempts := 0
	status := http.StatusBadRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`reset after 1ms`))
	}))
	defer server.Close()

	ep := Endpoint{}
	policy := RateLimitRetryPolicy{MaxRetries: 2, MaxDelay: time.Second}

	req, _, err := ep.NewRequest(context.Background(), server.URL, map[string]any{}, false)
	require.NoError(t, err)
	_, err = ep.DoWithRateLimitRetry(req, policy)
	require.Error(t, err)
	assert.Equal(t, 1, attempts, "a 400 must not be retried")

	attempts = 0
	status = http.StatusTooManyRequests
	req, _, err = ep.NewRequest(context.Background(), server.URL, map[string]any{}, false)
	require.NoError(t, err)
	_, err = ep.DoWithRateLimitRetry(req, policy)
	require.Error(t, err)
	assert.True(t, IsRateLimitError(err))
	assert.Equal(t, 3, attempts, "first attempt plus two retries")
}
