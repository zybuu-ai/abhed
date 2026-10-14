package model

import (
	"errors"
	"net/url"
)

// scrubURLError strips the query and any user information from the URL a
// failed request reports: a base_url may carry a key there (?api_key=,
// user:key@host), and the error reaches the terminal, the record and the log.
func scrubURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	u, perr := url.Parse(ue.URL)
	if perr != nil {
		return &url.Error{Op: ue.Op, URL: "(unparsable URL)", Err: ue.Err}
	}
	if u.RawQuery == "" && u.User == nil && u.Fragment == "" {
		return err
	}
	u.RawQuery, u.User, u.Fragment, u.RawFragment, u.ForceQuery = "", nil, "", "", false
	return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
}
