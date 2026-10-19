package tools

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type deepSecrets struct{ Read }

func (deepSecrets) SecretPaths() [][]string { return [][]string{{"from", "data", "*", "secret"}} }

// Names deeper in the arguments are put to their rules, and a value where a
// name belongs is withheld, at every key "*" reaches.
func TestSecretPathsAreReadAndWithheld(t *testing.T) {
	args := json.RawMessage(`{"from":{"data":{"a":{"secret":"KEY_A"},"b":{"file":"/x"},"c":{"secret":"not a name!"}}}}`)
	got := SecretNames(deepSecrets{}, args)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"KEY_A", "not a name!"}) {
		t.Fatalf("names %v", got)
	}
	out := string(WithholdSecretValues(deepSecrets{}, args))
	if strings.Contains(out, "not a name!") || !strings.Contains(out, `"KEY_A"`) || !strings.Contains(out, WithheldSecretArg) {
		t.Fatalf("withheld %s", out)
	}
	if again := WithholdSecretValues(deepSecrets{}, json.RawMessage(out)); string(again) != out {
		t.Fatalf("withholding twice changed %s to %s", out, again)
	}
}
