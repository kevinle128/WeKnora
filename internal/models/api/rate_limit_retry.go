package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// RateLimitRetryPolicy controls how a chat request waits and tries again when
// the vendor (or a proxy in front of it) says the caller is sending too fast.
type RateLimitRetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// BaseDelay is the wait when the reply gives no hint; it doubles each time.
	BaseDelay time.Duration
	// MaxDelay caps every single wait, including a vendor hint, so a hint of
	// "retry in 10 minutes" fails fast instead of holding the user's request.
	MaxDelay time.Duration
}

// DefaultRateLimitRetryPolicy is the policy used by the chat protocols.
var DefaultRateLimitRetryPolicy = RateLimitRetryPolicy{
	MaxRetries: 3,
	BaseDelay:  2 * time.Second,
	MaxDelay:   30 * time.Second,
}

// resetHintPattern matches the wait hints that vendors and LLM proxies put in
// the reply body, for example "(reset after 2s)", "retry after 1.5 seconds",
// "try again in 20s" or "Please retry in 3.2s".
var resetHintPattern = regexp.MustCompile(
	`(?i)(?:reset|retry|try again)\s+(?:after|in)\s+([0-9]+(?:\.[0-9]+)?)\s*(ms|milliseconds?|s|secs?|seconds?|m|mins?|minutes?)\b`,
)

// IsRateLimitError reports whether err is a vendor reply that asks the caller
// to slow down. A 429 always qualifies. A 503 qualifies only when it carries a
// Retry-After header or its body names a rate limit, because some gateways
// wrap an upstream 429 in a 503 while a plain 503 can also be an outage.
func IsRateLimitError(err error) bool {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusServiceUnavailable:
		if httpErr.RetryAfter() > 0 {
			return true
		}
		body := strings.ToLower(httpErr.Body)
		return strings.Contains(body, "rate limit") ||
			strings.Contains(body, "rate_limit") ||
			strings.Contains(body, "too many requests") ||
			strings.Contains(body, "[429]")
	}
	return false
}

// rateLimitWait is how long to wait before retry number attempt (1-based).
// The Retry-After header wins, then a hint in the body, then backoff.
func (p RateLimitRetryPolicy) rateLimitWait(err error, attempt int) time.Duration {
	wait := time.Duration(0)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		wait = httpErr.RetryAfter()
		if wait <= 0 {
			wait = parseResetHint(httpErr.Body)
		}
	}
	if wait <= 0 && p.BaseDelay > 0 {
		wait = p.BaseDelay << uint(attempt-1)
	}
	// A small margin so the request lands after the window resets, not on it.
	wait += 250 * time.Millisecond
	if p.MaxDelay > 0 && wait > p.MaxDelay {
		wait = p.MaxDelay
	}
	return wait
}

func parseResetHint(body string) time.Duration {
	m := resetHintPattern.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0
	}
	unit := strings.ToLower(m[2])
	switch {
	case strings.HasPrefix(unit, "ms"), strings.HasPrefix(unit, "milli"):
		return time.Duration(n * float64(time.Millisecond))
	case strings.HasPrefix(unit, "m"):
		return time.Duration(n * float64(time.Minute))
	default:
		return time.Duration(n * float64(time.Second))
	}
}

// DoWithRateLimitRetry sends req like Do, and when the reply is a rate-limit
// error it waits as the vendor asked and sends the same request again. Other
// errors return at once. The request body is replayed through req.GetBody,
// which http.NewRequestWithContext sets for the in-memory bodies built by
// NewRequest.
func (e Endpoint) DoWithRateLimitRetry(req *http.Request, policy RateLimitRetryPolicy) (*http.Response, error) {
	ctx := req.Context()
	attemptReq := req
	for attempt := 0; ; attempt++ {
		resp, err := e.Do(attemptReq)
		if err == nil || attempt >= policy.MaxRetries || !IsRateLimitError(err) || req.GetBody == nil {
			return resp, err
		}
		wait := policy.rateLimitWait(err, attempt+1)
		logger.Warnf(ctx, "[LLM Request] rate limited, retrying (%d/%d) after %v: %v",
			attempt+1, policy.MaxRetries, wait, err)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w (gave up waiting for rate limit: %v)", err, ctx.Err())
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, err
		}
		attemptReq = req.Clone(ctx)
		attemptReq.Body = body
	}
}
