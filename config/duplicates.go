package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// DuplicateKey is a key written more than once in one object of a
// configuration file. encoding/json keeps the last one without a word, so a
// second "gemini" provider silently replaces the first; this says so.
type DuplicateKey struct {
	File string `json:"file"`
	Path string `json:"path"` // the object that holds the key, "" for the top level
	Key  string `json:"key"`
	// Other is the earlier spelling when the two differ only in letter case.
	Other string `json:"other,omitempty"`
}

func (d DuplicateKey) String() string {
	where := Printable(orRoot(d.Path))
	if d.Other != "" {
		return fmt.Sprintf("%s: the keys %q and %q in %s differ only in letter case; a setting reads them as one, so only the last is used",
			Printable(d.File), Printable(d.Other), Printable(d.Key), where)
	}
	return fmt.Sprintf("%s: the key %q appears more than once in %s; only the last one is used", Printable(d.File), Printable(d.Key), where)
}

// duplicateKeys lists every key written twice in an object of data, at any
// depth; nil for a file that does not parse (its parse error says why). Keys
// that differ only in case count only for a setting (a struct field, which
// encoding/json matches without case), not for names such as providers.
func duplicateKeys(file string, data []byte) []DuplicateKey {
	if !json.Valid(data) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out []DuplicateKey
	collectDups(dec, file, "", reflect.TypeFor[Config](), &out)
	return out
}

// childType is the type a key of an object of type t decodes into, or nil when it is not known.
func childType(t reflect.Type, key string) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == nil:
		return nil
	case t.Kind() == reflect.Struct:
		if f, ok := fieldFor(jsonFields(t), key); ok {
			return f.Type
		}
	case t.Kind() == reflect.Map, t.Kind() == reflect.Slice, t.Kind() == reflect.Array:
		return t.Elem()
	}
	return nil
}

func isStruct(t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t != nil && t.Kind() == reflect.Struct
}

func collectDups(dec *json.Decoder, file, path string, t reflect.Type, out *[]DuplicateKey) {
	tok, err := dec.Token()
	if err != nil {
		return
	}
	switch tok {
	case json.Delim('{'):
		exact := map[string]bool{}
		folded := map[string]string{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return
			}
			key, _ := k.(string)
			switch first, seen := folded[foldKey(key)]; {
			case exact[key]:
				*out = append(*out, DuplicateKey{File: file, Path: path, Key: key})
			case seen && isStruct(t):
				*out = append(*out, DuplicateKey{File: file, Path: path, Key: key, Other: first})
			default:
				folded[foldKey(key)] = key
			}
			exact[key] = true
			collectDups(dec, file, join(path, key), childType(t, key), out)
		}
		_, _ = dec.Token()
	case json.Delim('['):
		var elem reflect.Type
		if t != nil {
			elem = childType(t, "")
		}
		for dec.More() {
			collectDups(dec, file, path, elem, out)
		}
		_, _ = dec.Token()
	}
}

// warnDuplicates warns once of each repeated key.
func warnDuplicates(w warnSink, dups []DuplicateKey) {
	for _, d := range dups {
		w.emit("dup\x00"+d.File+"\x00"+d.Path+"\x00"+d.Key, d.String())
	}
}

// Warnings are what is wrong with the configuration files, one sentence each:
// repeated keys and keys no part of Abhed reads. Editors show them beside the
// model list (studio-acp-contract.md §3.2).
func (c Config) Warnings() []string {
	out := make([]string, 0, len(c.Duplicates)+len(c.Unknown))
	for _, d := range c.Duplicates {
		out = append(out, d.String())
	}
	for _, u := range c.Unknown {
		out = append(out, u.String())
	}
	return out
}
