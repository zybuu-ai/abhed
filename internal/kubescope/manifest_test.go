package kubescope

import "testing"

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
