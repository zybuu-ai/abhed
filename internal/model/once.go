package model

import "net/http"

// SendOnce makes a sends each request once, with no retry, and has every
// redirect its client meets pass redirect too when that is set. It reports false
// for an adapter whose client it cannot reach, so a caller can refuse it.
func SendOnce(a Adapter, redirect func(*http.Request, []*http.Request) error) bool {
	confine := func(c *http.Client) *http.Client {
		cc := &http.Client{}
		if c != nil {
			copied := *c
			cc = &copied
		}
		if redirect != nil {
			// Added to the client's own check (same host only), never in its place.
			own := cc.CheckRedirect
			cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if own != nil {
					if err := own(req, via); err != nil {
						return err
					}
				}
				return redirect(req, via)
			}
		}
		return cc
	}
	switch m := a.(type) {
	case *OpenAICompatible:
		m.HTTP, m.Retry = confine(m.HTTP), RetryPolicy{Attempts: 1}
	case *Anthropic:
		m.HTTP, m.Retry = confine(m.HTTP), RetryPolicy{Attempts: 1}
	case *Gemini:
		m.HTTP, m.Retry = confine(m.HTTP), RetryPolicy{Attempts: 1}
	case *WatsonX:
		m.client = confine(m.client)
	default:
		return false
	}
	return true
}
