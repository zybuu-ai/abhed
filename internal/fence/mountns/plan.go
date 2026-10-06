package mountns

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Plan is what one command's mount namespace holds read-only or hides. Every
// path is relative to Root, local and clean.
type Plan struct {
	Root     string   `json:"root"`
	Pin      []string `json:"pin,omitempty"`
	ReadOnly []string `json:"read_only,omitempty"`
	Empty    []string `json:"empty,omitempty"`
	Null     []string `json:"null,omitempty"`
}

// ErrPlan is a plan that cannot be applied as written.
var ErrPlan = errors.New("mountns: invalid plan")

// Validate refuses a relative root and any path that is not local to it.
func (p Plan) Validate() error {
	if p.Root == "" || !filepath.IsAbs(p.Root) {
		return fmt.Errorf("%w: the root %q is not absolute", ErrPlan, p.Root)
	}
	for _, list := range [][]string{p.Pin, p.ReadOnly, p.Empty, p.Null} {
		for _, rel := range list {
			if rel == "" || rel == "." || !filepath.IsLocal(rel) || filepath.Clean(rel) != rel {
				return fmt.Errorf("%w: %q is not a clean path inside %s", ErrPlan, rel, p.Root)
			}
		}
	}
	return nil
}

// Describe words the plan for the record.
func (p Plan) Describe() string {
	return fmt.Sprintf("%d read-only, %d pinned, %d hidden", len(p.ReadOnly), len(p.Pin), len(p.Empty)+len(p.Null))
}
