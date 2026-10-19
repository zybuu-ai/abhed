package ocsf

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Version is the OCSF schema version Map writes.
const Version = "1.8.0"

// Versions are the schema versions an event can be written in: Version and
// the older ones Downgrade takes it to, newest first.
var Versions = []string{"1.8.0", "1.5.0", "1.3.0", "1.1.0"}

// subsetJSON is the part of each published schema the mapper uses, generated
// by scripts/ocsf-schema.py from schema.ocsf.io.
//
//go:embed schema/subset.json
var subsetJSON []byte

type attrDef struct {
	Type     string   `json:"type"`
	Object   string   `json:"object"`
	Array    bool     `json:"array"`
	Req      string   `json:"req"`
	Profiles []string `json:"profiles"`
	Enum     []string `json:"enum"`
}

type classDef struct {
	Name       string             `json:"name"`
	Caption    string             `json:"caption"`
	Profiles   []string           `json:"profiles"`
	Attributes map[string]attrDef `json:"attributes"`
}

type objectDef struct {
	Attributes map[string]attrDef `json:"attributes"`
}

type versionDef struct {
	Profiles []string             `json:"profiles"`
	Classes  map[string]classDef  `json:"classes"`
	Objects  map[string]objectDef `json:"objects"`
}

type subsetDef struct {
	Source   string                `json:"source"`
	Versions map[string]versionDef `json:"versions"`
}

var (
	subsetOnce sync.Once
	subset     subsetDef
	subsetErr  error
)

func schema(version string) (versionDef, error) {
	subsetOnce.Do(func() { subsetErr = json.Unmarshal(subsetJSON, &subset) })
	if subsetErr != nil {
		return versionDef{}, fmt.Errorf("ocsf: the embedded schema subset: %w", subsetErr)
	}
	v, ok := subset.Versions[version]
	if !ok {
		return versionDef{}, fmt.Errorf("ocsf: version %q is not one this build writes (%s)", version, strings.Join(Versions, ", "))
	}
	return v, nil
}

// CheckVersion reports whether events can be written in version.
func CheckVersion(version string) error {
	_, err := schema(version)
	return err
}

// Validate checks ev against the published schema of the version its
// metadata names: the class exists, every attribute is one the class or
// object defines, required ones are present, and enum values are defined.
// Attributes of a profile count as required only when the event names the
// profile in metadata.profiles.
func Validate(ev Event) error {
	ev, err := normal(ev)
	if err != nil {
		return err
	}
	meta, _ := ev["metadata"].(map[string]any)
	version, _ := meta["version"].(string)
	v, err := schema(version)
	if err != nil {
		return err
	}
	uid, ok := intOf(ev["class_uid"])
	if !ok {
		return fmt.Errorf("class_uid is missing")
	}
	c, ok := v.Classes[strconv.Itoa(uid)]
	if !ok {
		return fmt.Errorf("class %d is not in OCSF %s", uid, version)
	}
	var profiles []string
	if ps, ok := meta["profiles"].([]any); ok {
		for _, p := range ps {
			s, _ := p.(string)
			if !slices.Contains(c.Profiles, s) {
				return fmt.Errorf("profile %q does not apply to %s in OCSF %s", s, c.Caption, version)
			}
			profiles = append(profiles, s)
		}
	}
	var errs []string
	checkAttrs(v, c.Attributes, ev, "", profiles, &errs)
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s (OCSF %s): %s", c.Caption, version, strings.Join(errs, "; "))
	}
	return nil
}

func checkAttrs(v versionDef, defs map[string]attrDef, obj map[string]any, path string, profiles []string, errs *[]string) {
	for name, a := range defs {
		if _, ok := obj[name]; ok || a.Req != "required" {
			continue
		}
		if len(a.Profiles) > 0 && !anyOf(a.Profiles, profiles) {
			continue
		}
		*errs = append(*errs, path+name+" is required")
	}
	for name, val := range obj {
		a, ok := defs[name]
		if !ok {
			*errs = append(*errs, path+name+" is not defined")
			continue
		}
		if len(a.Profiles) > 0 && !anyOf(a.Profiles, profiles) {
			*errs = append(*errs, fmt.Sprintf("%s%s belongs to the %s profile, which metadata.profiles does not name",
				path, name, strings.Join(a.Profiles, " or ")))
		}
		if a.Array {
			list, ok := val.([]any)
			if !ok {
				*errs = append(*errs, path+name+" is not an array")
				continue
			}
			for i, item := range list {
				checkValue(v, a, item, fmt.Sprintf("%s%s[%d]", path, name, i), profiles, errs)
			}
			continue
		}
		checkValue(v, a, val, path+name, profiles, errs)
	}
}

func checkValue(v versionDef, a attrDef, val any, path string, profiles []string, errs *[]string) {
	switch a.Type {
	case "object_t":
		m, ok := val.(map[string]any)
		if !ok {
			*errs = append(*errs, path+" is not an object")
			return
		}
		if a.Object == "object" {
			return // free form, as unmapped is
		}
		o, ok := v.Objects[a.Object]
		if !ok {
			*errs = append(*errs, path+" is a "+a.Object+", which the subset does not hold")
			return
		}
		checkAttrs(v, o.Attributes, m, path+".", profiles, errs)
	case "integer_t", "long_t", "timestamp_t", "port_t":
		n, ok := intOf(val)
		if !ok {
			*errs = append(*errs, path+" is not an integer")
			return
		}
		if len(a.Enum) > 0 && !slices.Contains(a.Enum, strconv.Itoa(n)) {
			*errs = append(*errs, fmt.Sprintf("%s %d is not a defined value", path, n))
		}
	case "boolean_t":
		if _, ok := val.(bool); !ok {
			*errs = append(*errs, path+" is not a boolean")
		}
	case "json_t":
	case "float_t":
		if _, ok := val.(float64); !ok {
			*errs = append(*errs, path+" is not a number")
		}
	default:
		s, ok := val.(string)
		if !ok {
			*errs = append(*errs, path+" is not a string")
			return
		}
		if len(a.Enum) > 0 && !slices.Contains(a.Enum, s) {
			*errs = append(*errs, fmt.Sprintf("%s %q is not a defined value", path, s))
		}
	}
}

// intOf reads a whole number as Go or encoding/json holds it.
func intOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int64(n)) {
			return int(n), true
		}
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// normal is ev as encoding/json reads it back: maps, slices of any, and
// json.Number for numbers.
func normal(ev Event) (Event, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func anyOf(want, have []string) bool {
	for _, w := range want {
		if slices.Contains(have, w) {
			return true
		}
	}
	return false
}
