package kubescope

import (
	"strings"
	"testing"
)

func TestDecodeManifestRefusesAmbiguousKeys(t *testing.T) {
	for _, m := range []string{
		`{"kind":"ClusterRoleBinding","Kind":"ConfigMap"}`,
		`{"kind":"a","kind":"a"}`,
		`{"KIND":"ConfigMap"}`,
		`{"apiversion":"v1"}`,
		`{"Metadata":{}}`,
		`{"metadata":{"Name":"x"}}`,
		`{"metadata":{"nameSpace":"x"}}`,
		"{\"Kind\":\"ConfigMap\"}",
		`{"spec":{"a":{"b":1,"B":2}}}`,
		`{"spec":[{"a":1,"a":2}]}`,
		`{"kind":1}`,
		`{"metadata":"x"}`,
		`{"metadata":{"name":["x"]}}`,
		`{} {}`,
		`[]`,
		` `,
	} {
		if _, err := DecodeManifest(m); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
}

func TestDecodeManifestReadsOneEncoding(t *testing.T) {
	mf, err := DecodeManifest(`{"metadata":{"namespace":"dev","name":"x","labels":{"Name":"y"}},"kind":"ConfigMap","apiVersion":"v1","data":{"n":10000000000000000001,"h":"<&>"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if mf.Kind != "ConfigMap" || mf.APIVersion != "v1" || mf.Name != "x" || mf.Namespace != "dev" {
		t.Fatalf("read %+v", mf)
	}
	want := `{"apiVersion":"v1","data":{"h":"<&>","n":10000000000000000001},"kind":"ConfigMap","metadata":{"labels":{"Name":"y"},"name":"x","namespace":"dev"}}`
	if string(mf.Canonical) != want {
		t.Fatalf("canonical %s", mf.Canonical)
	}
	again, err := DecodeManifest(string(mf.Canonical))
	if err != nil || string(again.Canonical) != want {
		t.Fatalf("the canonical form does not decode to itself: %v %s", err, again.Canonical)
	}
}

// A list of objects is named as one, so the model applies each item instead
// of reading a complaint about keys.
func TestDecodeManifestNamesAList(t *testing.T) {
	_, err := DecodeManifest(` [{"kind":"ConfigMap"},{"kind":"Secret"}]`)
	if err == nil || !strings.Contains(err.Error(), "JSON list of 2 items") || !strings.Contains(err.Error(), "each item in its own call") {
		t.Fatalf("got %v", err)
	}
	if _, err := DecodeManifest(`[{"kind":`); err == nil || !strings.Contains(err.Error(), "JSON list") {
		t.Fatalf("a broken list: %v", err)
	}
	if _, err := DecodeManifest(`null`); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("null: %v", err)
	}
}
