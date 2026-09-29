package tools

import "encoding/json"

// ExecutedSubject decodes arguments the way the named native tool does and
// returns the argument policy reads for it.
func ExecutedSubject(tool string, raw json.RawMessage) (string, error) {
	switch tool {
	case "bash":
		var a bashArgs
		err := json.Unmarshal(raw, &a)
		return a.Command, err
	case "write":
		var a writeArgs
		err := json.Unmarshal(raw, &a)
		return a.Path, err
	case "edit":
		var a editArgs
		err := json.Unmarshal(raw, &a)
		return a.Path, err
	}
	var a readArgs
	err := json.Unmarshal(raw, &a)
	return a.Path, err
}
