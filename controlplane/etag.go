package controlplane

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrPreconditionFailed is an If-Match no current version satisfies (412).
var ErrPreconditionFailed = errors.New("precondition failed")

// ETag is the entity tag for a resource version.
func ETag(rv int64) string { return `"` + strconv.FormatInt(rv, 10) + `"` }

// CheckIfMatch compares an If-Match header with the current resource
// version. An empty header passes; "*" passes for any existing object. A
// weak or malformed tag is ErrInvalid (400): If-Match compares strongly.
func CheckIfMatch(header string, current int64) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	if header == "*" {
		return nil
	}
	matched := false
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if strings.HasPrefix(tag, "W/") {
			return fmt.Errorf("%w: If-Match needs a strong entity tag, not %s", ErrInvalid, tag)
		}
		if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
			return fmt.Errorf("%w: If-Match entity tag %q", ErrInvalid, tag)
		}
		rv, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
		if err != nil || rv < 1 {
			return fmt.Errorf("%w: If-Match entity tag %q", ErrInvalid, tag)
		}
		if rv == current {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("%w: the sandbox is at %s", ErrPreconditionFailed, ETag(current))
	}
	return nil
}
