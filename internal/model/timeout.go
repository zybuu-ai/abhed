package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Timeouts bound one request to a model: Call from sending it to the last byte
// of its reply, Stall the longest wait with no byte arriving, the first included.
type Timeouts struct {
	Call  time.Duration
	Stall time.Duration
}

// DefaultTimeouts keeps the ten minutes a request always had, and gives up on a
// reply that sends nothing for five: a slow local model's prefill fits in that.
func DefaultTimeouts() Timeouts {
	return Timeouts{Call: 10 * time.Minute, Stall: 5 * time.Minute}
}

// orDefault fills each unset bound from DefaultTimeouts.
func (t Timeouts) orDefault() Timeouts {
	d := DefaultTimeouts()
	if t.Call <= 0 {
		t.Call = d.Call
	}
	if t.Stall <= 0 {
		t.Stall = d.Stall
	}
	return t
}

// TimeoutError is a model request ended by one of its Timeouts. It is a
// condition of the moment, so the same call may be tried again.
type TimeoutError struct {
	// Stall is true when no byte arrived for After; false when the whole call took After.
	Stall bool
	After time.Duration
}

func (e *TimeoutError) Error() string {
	if e.Stall {
		return fmt.Sprintf("the model sent nothing for %s, so the call was stopped; it can be retried", e.After)
	}
	return fmt.Sprintf("the model call took longer than %s, so it was stopped; it can be retried", e.After)
}

// Timeout reports true, as net.Error does.
func (e *TimeoutError) Timeout() bool { return true }

// SetTimeouts replaces each adapter's HTTP client with one bounded by t.
func (c *Anthropic) SetTimeouts(t Timeouts)        { c.HTTP = timeoutClient(t, c.endpoints) }
func (c *OpenAICompatible) SetTimeouts(t Timeouts) { c.HTTP = timeoutClient(t, c.endpoints) }
func (g *Gemini) SetTimeouts(t Timeouts)           { g.HTTP = timeoutClient(t, g.endpoints) }
func (w *WatsonX) SetTimeouts(t Timeouts)          { w.client = timeoutClient(t, w.endpoints) }

// The endpoints each adapter reaches, which the egress allowlist allows implicitly.
func (c *Anthropic) endpoints() []string        { return []string{c.BaseURL} }
func (c *OpenAICompatible) endpoints() []string { return []string{c.BaseURL} }
func (g *Gemini) endpoints() []string           { return []string{g.BaseURL} }
func (w *WatsonX) endpoints() []string          { return []string{w.BaseURL, w.IAMURL} }

// timeoutClient bounds every request by t; under the egress allowlist, a
// request to a host other than endpoints is judged by the rules.
func timeoutClient(t Timeouts, endpoints func() []string) *http.Client {
	base := &egress.Transport{Kind: egress.KindModel, Model: endpoints}
	return &http.Client{Transport: &timeoutTransport{base: base, t: t.orDefault()}, CheckRedirect: sameHostRedirect}
}

// sameHostRedirect follows a redirect only to the same scheme and host: Go
// carries a header such as x-api-key across hosts, so a key bound to one
// host would otherwise follow a redirect to another.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if !strings.EqualFold(req.URL.Host, first.Host) || req.URL.Scheme != first.Scheme {
		return fmt.Errorf("a model endpoint redirected to another host (%s); Abhed does not follow it with the key", req.URL.Host)
	}
	return nil
}

// timeoutTransport ends a request that runs past its Timeouts with a
// TimeoutError, from the round trip or from reading the body.
type timeoutTransport struct {
	base http.RoundTripper
	t    Timeouts
}

func (tr *timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, stop := context.WithTimeoutCause(req.Context(), tr.t.Call, &TimeoutError{After: tr.t.Call})
	ctx, cancel := context.WithCancelCause(ctx)
	w := &stallWatch{d: tr.t.Stall}
	w.timer = time.AfterFunc(w.d, func() { cancel(&TimeoutError{Stall: true, After: w.d}) })
	end := func() {
		w.timer.Stop()
		cancel(nil)
		stop()
	}
	resp, err := tr.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cause := ours(ctx)
		end()
		if cause != nil {
			return nil, cause
		}
		return nil, err
	}
	w.timer.Reset(w.d)
	resp.Body = &timeoutBody{ReadCloser: resp.Body, ctx: ctx, w: w, end: end}
	return resp, nil
}

// ours is the TimeoutError that ended ctx, or nil when the caller ended it.
func ours(ctx context.Context) error {
	var te *TimeoutError
	if errors.As(context.Cause(ctx), &te) {
		return te
	}
	return nil
}

type stallWatch struct {
	d     time.Duration
	timer *time.Timer
}

// timeoutBody restarts the stall wait on every byte, and reports a read the
// timeouts cut short as their TimeoutError.
type timeoutBody struct {
	io.ReadCloser
	ctx    context.Context
	w      *stallWatch
	end    func()
	closed atomic.Bool
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.ctx.Err() == nil {
		b.w.timer.Reset(b.w.d)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if cause := ours(b.ctx); cause != nil {
			return n, cause
		}
	}
	return n, err
}

func (b *timeoutBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.closed.Swap(true) {
		b.end()
	}
	return err
}

// CallTimeout is the longest one request to the adapter may take.
func (c *Anthropic) CallTimeout() time.Duration        { return clientCallTimeout(c.HTTP) }
func (c *OpenAICompatible) CallTimeout() time.Duration { return clientCallTimeout(c.HTTP) }
func (g *Gemini) CallTimeout() time.Duration           { return clientCallTimeout(g.HTTP) }
func (w *WatsonX) CallTimeout() time.Duration          { return clientCallTimeout(w.client) }

func clientCallTimeout(c *http.Client) time.Duration {
	if c != nil {
		if tr, ok := c.Transport.(*timeoutTransport); ok {
			return tr.t.Call
		}
	}
	return DefaultTimeouts().Call
}

// CallTimeoutOf is a's call timeout, through any wrapper that passes it on,
// or the default for an adapter that does not say.
func CallTimeoutOf(a Adapter) time.Duration {
	if c, ok := a.(interface{ CallTimeout() time.Duration }); ok {
		return c.CallTimeout()
	}
	return DefaultTimeouts().Call
}
