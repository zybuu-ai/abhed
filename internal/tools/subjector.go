package tools

import (
	"encoding/json"
	"sync"
)

// Subjector is a tool that names its own policy subject, for a tool whose
// most security-relevant argument is not the first of SubjectKeys: ssh's
// host as well as its command. ok is false when the arguments name none.
type Subjector interface {
	PolicySubject(args json.RawMessage) (subject string, ok bool)
}

var subjectors sync.Map // tool name -> Subjector

// RegisterSubjector makes s name the policy subject of the tool called name.
func RegisterSubjector(name string, s Subjector) { subjectors.Store(name, s) }

// SubjectorFor is the Subjector registered for the tool called name, or nil.
func SubjectorFor(name string) Subjector {
	if s, ok := subjectors.Load(name); ok {
		return s.(Subjector)
	}
	return nil
}
