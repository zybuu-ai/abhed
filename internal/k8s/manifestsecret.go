package k8s

import (
	"encoding/json"
	"strings"

	"github.com/zybuu-ai/abhed/internal/kubescope"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A Secret written into k8s_apply's manifest carries its values in the call,
// where policy, the approver and the record would read them. Such a call is
// refused, pointing to secret_from, and its values are withheld before
// anything reads the call.

// secretFromHint says how to make a Secret without its values in the call.
const secretFromHint = "Use secret_from instead: name each key's stored secret (the user stores it " +
	"with `abhed secret set NAME`) or workspace file, e.g. {\"action\":\"apply\",\"secret_from\":" +
	"{\"name\":\"site-tls\",\"type\":\"kubernetes.io/tls\",\"data\":{\"tls.crt\":{\"file\":" +
	"\"/abs/site.crt\"},\"tls.key\":{\"secret\":\"SITE_TLS_KEY\"}}}}"

// secretInManifest is the refusal, for the model.
const secretInManifest = "a Secret's values do not go in a manifest, where the rules, the approval and the " +
	"record would hold them, so this call was refused and its values withheld. " + secretFromHint

// withheldManifest stands in for a manifest that could not be read and may
// hold a Secret's values.
const withheldManifest = "[withheld: a manifest that could not be read and may hold a Secret's values]"

// WithholdSecrets withholds the values of any Secret in the call's
// manifest, before policy, the approver or the record read the call.
func (ApplyTool) WithholdSecrets(raw json.RawMessage) json.RawMessage {
	args, err := tools.DecodeArgs(raw)
	if err != nil {
		return raw
	}
	v, _ := tools.Lookup(args, "manifest")
	manifest, _ := v.(string)
	if strings.TrimSpace(manifest) == "" {
		return raw
	}
	var obj any
	if mf, err := kubescope.DecodeManifest(manifest); err == nil {
		obj = mf.Object
	} else if json.Unmarshal([]byte(manifest), &obj) != nil {
		if !mayHoldSecret(manifest) {
			return raw
		}
		args["manifest"] = withheldManifest
		return encodeArgs(args)
	}
	if !maskSecrets(obj) {
		return raw
	}
	b, err := json.Marshal(obj)
	if err != nil {
		args["manifest"] = withheldManifest
	} else {
		args["manifest"] = string(b)
	}
	return encodeArgs(args)
}

// mayHoldSecret is a guess at text that is not JSON: whether it names a
// Secret and its data.
func mayHoldSecret(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "secret") && strings.Contains(l, "data")
}

func encodeArgs(args map[string]any) json.RawMessage {
	b, err := json.Marshal(args)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
