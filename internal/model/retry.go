package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retryable reports whether a response should be tried again.
//
// A rate limit and a server error are conditions of the moment, not of the
// request: the same call a few seconds later usually succeeds. Treating them as
// fatal ends a session and discards everything it had established, which is a
// far worse outcome than waiting — and it is the outcome a user sees as "the
// agent crashed" when nothing was actually wrong with what they asked.
//
// A 4xx that is not 429 is not retried. A malformed request, a bad credential
// or a model that does not exist will fail identically however many times it is
// sent, and retrying only delays the error the user needs to read.
func Retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// RetryPolicy bounds how long a caller waits for a transient failure to clear.
type RetryPolicy struct {
	// Attempts is the total number of tries, including the first.
	Attempts int
	// Base is the first backoff interval; each retry doubles it.
	Base time.Duration
	// Max caps a single wait, so a long Retry-After does not strand a session.
	Max time.Duration
}

// DefaultRetry is deliberately short. An agent turn is interactive: a user
// watching a prompt will accept a few seconds of silence and not a minute of
// it, and a rate limit that has not cleared in half a minute usually needs a
// different model or a smaller request rather than more patience.
func DefaultRetry() RetryPolicy {
	return RetryPolicy{Attempts: 4, Base: time.Second, Max: 20 * time.Second}
}

// Wait sleeps before attempt n (1-based), honouring the server's own advice.
//
// Retry-After is preferred over the computed backoff because the server knows
// when its limit resets and a client guessing shorter simply burns another
// request against the same limit. Jitter is added to the computed case so that
// several agents failing together do not retry in lockstep.
func (p RetryPolicy) Wait(ctx context.Context, attempt int, header http.Header) error {
	delay := p.Base * time.Duration(1<<uint(attempt-1))
	if advised, ok := retryAfter(header); ok {
		delay = advised
	} else {
		delay += time.Duration(rand.Int63n(int64(p.Base / 2)))
	}
	if p.Max > 0 && delay > p.Max {
		delay = p.Max
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryAfter reads the header, in either of the two forms the spec allows.
func retryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		return 0, true // a date in the past means retry now
	}
	return 0, false
}

// retryNote explains to the user what is happening, since a silent pause in an
// interactive session is indistinguishable from a hang.
func retryNote(status, attempt, total int, wait time.Duration) string {
	what := "rate limited"
	if status >= 500 {
		what = fmt.Sprintf("upstream error %d", status)
	}
	return fmt.Sprintf("%s — retrying in %s (attempt %d of %d)",
		what, wait.Round(100*time.Millisecond), attempt, total)
}

// send performs a request with retries, rebuilding it on each attempt.
//
// The body has to be rebuilt rather than reused: an http.Request carries a
// reader, and by the time a response comes back that reader is drained. Reusing
// it retries with an empty body, which the server rejects for a reason that has
// nothing to do with the original failure — an easy bug to introduce and a
// confusing one to find.
//
// notify is called before each wait so an interactive caller can say why it has
// gone quiet. A silent pause is indistinguishable from a hang.
func send(ctx context.Context, client *http.Client, p RetryPolicy,
	build func() (*http.Request, error), notify func(string),
	fatal func(*StatusError) bool) (*http.Response, error) {

	attempts := p.Attempts
	if attempts < 1 {
		attempts = 1
	}

	var lastStatus int
	var lastBody string
	var lastRetryAfter, lastRateLimitHeaders bool
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if te := (*TimeoutError)(nil); errors.As(err, &te) {
			// A bound already waited out: more tries would multiply it unseen.
			return nil, te
		}
		if err != nil {
			// A connection that failed to establish is worth one more try for
			// the same reason a 503 is: the network moment has passed.
			if attempt < attempts && ctx.Err() == nil {
				if notify != nil {
					notify(fmt.Sprintf("connection failed — retrying (attempt %d of %d)",
						attempt+1, attempts))
				}
				if werr := p.Wait(ctx, attempt, nil); werr != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		if resp.StatusCode == http.StatusOK || !Retryable(resp.StatusCode) {
			return resp, nil
		}
		// Fatal lets a caller stop early on a status that is retryable in
		// general but hopeless in its particular case — a refusal that arrives
		// wearing a 429, for instance. Deciding that here would be wrong:
		// plenty of real rate limiters omit Retry-After, and treating its
		// absence as fatal would stop retrying the very failures retrying
		// exists for.
		if fatal != nil {
			h := resp.Header
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			resp.Body.Close()
			se := &StatusError{
				Status: resp.StatusCode, Body: strings.TrimSpace(string(body)),
				Attempts:            attempt,
				RetryAfter:          h.Get("Retry-After") != "",
				HasRateLimitHeaders: hasRateLimitHeaders(h),
			}
			if fatal(se) {
				return nil, se
			}
			// Not fatal after all: carry on with what was already read.
			lastStatus, lastBody = se.Status, se.Body
			lastRetryAfter, lastRateLimitHeaders = se.RetryAfter, se.HasRateLimitHeaders
			if attempt == attempts {
				break
			}
			if notify != nil {
				notify(retryNote(lastStatus, attempt+1, attempts, p.Base))
			}
			if err := p.Wait(ctx, attempt, h); err != nil {
				return nil, err
			}
			continue
		}

		header := resp.Header
		lastStatus = resp.StatusCode
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		lastBody = strings.TrimSpace(string(body))
		lastRetryAfter = header.Get("Retry-After") != ""
		lastRateLimitHeaders = hasRateLimitHeaders(header)
		resp.Body.Close()

		if attempt == attempts {
			break
		}
		if notify != nil {
			wait, _ := retryAfter(header)
			if wait == 0 {
				wait = p.Base * time.Duration(1<<uint(attempt-1))
			}
			if p.Max > 0 && wait > p.Max {
				wait = p.Max
			}
			notify(retryNote(lastStatus, attempt+1, attempts, wait))
		}
		if err := p.Wait(ctx, attempt, header); err != nil {
			return nil, err
		}
	}

	return nil, &StatusError{
		Status: lastStatus, Body: lastBody, Attempts: attempts,
		RetryAfter: lastRetryAfter, HasRateLimitHeaders: lastRateLimitHeaders,
	}
}

// StatusError is a request that kept failing. It names the attempts so the
// message says "after 4 attempts" rather than implying a single try.
type StatusError struct {
	Status   int
	Body     string
	Attempts int
	// RetryAfter and HasRateLimitHeaders record whether the server said when
	// its limit resets. A 429 that says neither is not behaving like a rate
	// limit, whatever its body claims.
	RetryAfter          bool
	HasRateLimitHeaders bool
}

func (e *StatusError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("%d %s after %d attempts: %s",
			e.Status, http.StatusText(e.Status), e.Attempts, e.Body)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

// SetNotify installs a callback for retry notices. It exists as a method on
// each adapter rather than a constructor argument so a caller can attach one
// without knowing which provider it is holding.
func (c *Anthropic) SetNotify(f func(string))        { c.Notify = f }
func (c *OpenAICompatible) SetNotify(f func(string)) { c.Notify = f }
func (g *Gemini) SetNotify(f func(string))           { g.Notify = f }

// hasRateLimitHeaders reports whether the server described its own limit. A
// real rate limit says when it resets; a refusal wearing a 429 does not.
func hasRateLimitHeaders(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit") ||
			strings.HasPrefix(strings.ToLower(k), "x-ratelimit") {
			return true
		}
	}
	return false
}
