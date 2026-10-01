package kubescope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Manifest is an apply's object decoded once, strictly. The resolver, the
// policy subject and the tool all read this one value.
type Manifest struct {
	Object     map[string]any
	Kind       string
	APIVersion string
	Name       string
	Namespace  string
	// Canonical is the one encoding that is judged, approved and sent.
	Canonical []byte
}

// canonicalTop and canonicalMeta are the keys read by name. A Go struct reads
// any case of them and the API server only this one, so no other is accepted.
var canonicalTop = []string{"apiVersion", "kind", "metadata"}
var canonicalMeta = []string{"name", "namespace"}

// DecodeManifest decodes a manifest as one JSON object with each key once,
// ignoring case, at every depth, and kind, apiVersion, metadata, name and
// namespace spelled only as the API server reads them.
func DecodeManifest(s string) (*Manifest, error) {
	if len(bytes.TrimSpace([]byte(s))) == 0 {
		return nil, errors.New("the manifest is empty")
	}
	obj, err := tools.DecodeArgs(json.RawMessage(s))
	if err != nil {
		return nil, fmt.Errorf("the manifest must be one JSON object with each key once, in any case: %s",
			strings.TrimPrefix(err.Error(), tools.ErrMalformedArgs.Error()+": "))
	}
	if err := exactKeys(obj, canonicalTop, ""); err != nil {
		return nil, err
	}
	m := &Manifest{Object: obj}
	if m.Kind, err = stringField(obj, "kind", "kind"); err != nil {
		return nil, err
	}
	if m.APIVersion, err = stringField(obj, "apiVersion", "apiVersion"); err != nil {
		return nil, err
	}
	if v, ok := obj["metadata"]; ok && v != nil {
		meta, isObj := v.(map[string]any)
		if !isObj {
			return nil, errors.New("the manifest's metadata must be an object")
		}
		if err := exactKeys(meta, canonicalMeta, "metadata."); err != nil {
			return nil, err
		}
		if m.Name, err = stringField(meta, "name", "metadata.name"); err != nil {
			return nil, err
		}
		if m.Namespace, err = stringField(meta, "namespace", "metadata.namespace"); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, fmt.Errorf("the manifest cannot be encoded: %w", err)
	}
	m.Canonical = bytes.TrimRight(buf.Bytes(), "\n")
	return m, nil
}

// exactKeys refuses a key that folds to one of names but is spelled otherwise.
func exactKeys(obj map[string]any, names []string, prefix string) error {
	for k := range obj {
		for _, n := range names {
			if k != n && tools.FoldKey(k) == tools.FoldKey(n) {
				return fmt.Errorf("the manifest's %s%q must be spelled %q", prefix, k, n)
			}
		}
	}
	return nil
}

func stringField(obj map[string]any, key, label string) (string, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("the manifest's %s must be a string", label)
	}
	return s, nil
}
