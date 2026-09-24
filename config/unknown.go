package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// UnknownKey is a key in a config file that no setting reads. It is ignored,
// as it always was, but it is reported: a misspelt key silently changes what runs.
type UnknownKey struct {
	File    string `json:"file"`
	Path    string `json:"path"`              // e.g. model.provider
	Suggest string `json:"suggest,omitempty"` // a known key it is close to
}

func (u UnknownKey) String() string {
	s := fmt.Sprintf("%s: unknown key %s is ignored", u.File, u.Path)
	if u.Suggest != "" {
		s += fmt.Sprintf(" (did you mean %s?)", u.Suggest)
	}
	return s
}

// aliases are keys written for another where the spelling is not close.
var aliases = map[string]string{
	"model.provider": "model.default",
	"model.name":     "model.default",
}

var (
	warnOut  io.Writer = os.Stderr
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// warnUnknown writes each unknown key once per process, however often the
// configuration is loaded.
func warnUnknown(keys []UnknownKey) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	for _, k := range keys {
		if id := k.File + "\x00" + k.Path; !warned[id] {
			warned[id] = true
			fmt.Fprintf(warnOut, "abhed: warning: %s\n", k)
		}
	}
}

// unknownKeys lists the keys in data that t has no field for.
func unknownKeys(file string, data []byte, t reflect.Type) []UnknownKey {
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []UnknownKey
	walkUnknown(file, "", raw, t, &out)
	return out
}

func walkUnknown(file, path string, v any, t reflect.Type, out *[]UnknownKey) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// A type that reads its own JSON decides what it accepts.
	if reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return
	}
	switch val := v.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := jsonFields(t)
			keys := make([]string, 0, len(val))
			for k := range val {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := join(path, k)
				// encoding/json matches a key to a field regardless of case.
				f, ok := fields[strings.ToLower(k)]
				if !ok {
					*out = append(*out, UnknownKey{File: file, Path: p, Suggest: suggest(path, k, val[k], fields)})
					continue
				}
				walkUnknown(file, p, val[k], f.Type, out)
			}
		case reflect.Map:
			for k, e := range val {
				walkUnknown(file, join(path, k), e, t.Elem(), out)
			}
		}
	case []any:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			for i, e := range val {
				walkUnknown(file, fmt.Sprintf("%s[%d]", path, i), e, t.Elem(), out)
			}
		}
	}
}

// jsonFields maps each field's JSON name, lower-cased, to the field,
// following embedded structs as encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					if _, ok := fields[k]; !ok {
						fields[k] = v
					}
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		fields[strings.ToLower(name)] = f
	}
	return fields
}

// suggest names the known key an unknown one was probably meant to be: an
// alias, or the closest name at the same level that takes the same kind of value.
func suggest(path, key string, v any, fields map[string]reflect.StructField) string {
	if a, ok := aliases[join(path, strings.ToLower(key))]; ok {
		return a
	}
	best, bestD := "", len(key)/3+1
	if bestD < 2 {
		bestD = 2
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if d := distance(strings.ToLower(key), n); d <= bestD && fits(v, fields[n].Type) {
			if d < bestD || best == "" {
				best, bestD = n, d
			}
		}
	}
	if best == "" {
		return ""
	}
	return join(path, best)
}

// fits reports whether a JSON value could be decoded into t.
func fits(v any, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch v.(type) {
	case map[string]any:
		return t.Kind() == reflect.Struct || t.Kind() == reflect.Map || t.Kind() == reflect.Interface
	case []any:
		return t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Interface
	case string:
		return t.Kind() == reflect.String || t.Kind() == reflect.Interface
	case bool:
		return t.Kind() == reflect.Bool || t.Kind() == reflect.Interface
	case float64:
		switch t.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64, reflect.Interface:
			return true
		}
		return false
	}
	return true
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
