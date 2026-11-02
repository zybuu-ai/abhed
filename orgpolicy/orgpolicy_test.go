package orgpolicy

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func stmt(epoch int64, policy string, at time.Time, keys ...ed25519.PublicKey) Statement {
	s := Statement{V: 1, Tenant: "acme", Subject: "alice", Name: "default", Version: epoch, Epoch: epoch,
		IssuedAt: at, MaxAgeSeconds: 3600, Policy: json.RawMessage(policy)}
	for _, k := range keys {
		s.Keys = append(s.Keys, EncodeKey(k))
	}
	return s
}

func sign(t *testing.T, s Statement, priv ed25519.PrivateKey) Envelope {
	t.Helper()
	e, err := Sign(s, priv)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestOpenChecksTheSignatureAndTheStatement(t *testing.T) {
	pub, priv := newKey(t)
	other, otherPriv := newKey(t)
	now := time.Now().UTC()
	env := sign(t, stmt(3, `{"permissions":{"mode":"plan"}}`, now, pub), priv)
	got, kid, err := Open(env, []ed25519.PublicKey{other, pub})
	if err != nil || kid != KeyID(pub) || got.Epoch != 3 || string(got.Policy) != `{"permissions":{"mode":"plan"}}` {
		t.Fatalf("open: %v %q %+v", err, kid, got)
	}
	if _, _, err := Open(env, []ed25519.PublicKey{other}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("an unpinned signer was taken: %v", err)
	}
	if _, _, err := Open(env, nil); err == nil {
		t.Error("no pinned keys verified")
	}
	// A payload changed after signing.
	tampered := env
	tampered.Payload = base64.StdEncoding.EncodeToString([]byte(strings.Replace(mustDecode(t, env.Payload), "plan", "bypass", 1)))
	if _, _, err := Open(tampered, []ed25519.PublicKey{pub}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("a changed payload verified: %v", err)
	}
	// The same payload under another type.
	retyped := env
	retyped.PayloadType = "application/vnd.abhed.evidence+json"
	if _, _, err := Open(retyped, []ed25519.PublicKey{pub}); err == nil {
		t.Error("another payload type was taken as a policy")
	}
	// A signature by another key claiming the pinned key's id.
	forged := sign(t, stmt(3, `{}`, now), otherPriv)
	forged.Signatures[0].KeyID = KeyID(pub)
	if _, _, err := Open(forged, []ed25519.PublicKey{pub}); !errors.Is(err, ErrUntrusted) {
		t.Errorf("a forged key id verified: %v", err)
	}
	for name, s := range map[string]Statement{
		"version":    {V: 2, IssuedAt: now, Policy: json.RawMessage(`{}`)},
		"no time":    {V: 1, Policy: json.RawMessage(`{}`)},
		"not object": {V: 1, IssuedAt: now, Policy: json.RawMessage(`[1]`)},
		"bad key":    {V: 1, IssuedAt: now, Policy: json.RawMessage(`{}`), Keys: []string{"nope"}},
		"negative":   {V: 1, IssuedAt: now, Policy: json.RawMessage(`{}`), Epoch: -1},
	} {
		if _, _, err := Open(sign(t, s, priv), []ed25519.PublicKey{pub}); err == nil {
			t.Errorf("%s: a bad statement was taken", name)
		}
	}
	// Fields the format does not have are refused, not ignored.
	payload := `{"v":1,"tenant":"t","name":"n","version":1,"epoch":1,"issued_at":"` + now.Format(time.RFC3339) + `","policy":{},"extra":1}`
	sig := ed25519.Sign(priv, PAE(PayloadType, []byte(payload)))
	extra := Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signatures: []Signature{{KeyID: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(sig)}}}
	if _, _, err := Open(extra, []ed25519.PublicKey{pub}); err == nil {
		t.Error("an unknown field was accepted")
	}
}

func mustDecode(t *testing.T, s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseKeyReadsBase64AndPEM(t *testing.T) {
	pub, _ := newKey(t)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	for _, s := range []string{EncodeKey(pub), pemKey, " " + EncodeKey(pub) + "\n"} {
		k, err := ParseKey(s)
		if err != nil || !k.Equal(pub) {
			t.Errorf("ParseKey(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "AAAA", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) took a bad key", s)
		}
	}
}

func TestEvaluateDecidesWhatApplies(t *testing.T) {
	pub, priv := newKey(t)
	now := time.Now().UTC()
	fresh := sign(t, stmt(5, `{"web_search":{"enabled":true}}`, now.Add(-time.Minute), pub), priv)
	stale := sign(t, stmt(5, `{"web_search":{"enabled":true}}`, now.Add(-2*time.Hour), pub), priv)
	future := sign(t, stmt(5, `{}`, now.Add(time.Hour), pub), priv)
	pins := []string{EncodeKey(pub)}
	cases := []struct {
		name   string
		st     *State
		m      Machine
		status string
		doc    bool
		why    string
	}{
		{"not enrolled", nil, Machine{}, StatusNone, false, "not signed in"},
		{"required, not enrolled", nil, Machine{Required: true, URL: "https://org"}, StatusFallback, false, "requires an org policy"},
		{"applied", &State{URL: "https://org", Keys: pins, Envelope: &fresh, FetchedAt: now}, Machine{}, StatusApplied, true, ""},
		{"stale", &State{URL: "https://org", Keys: pins, Envelope: &stale, FetchedAt: now}, Machine{}, StatusFallback, true, "past its limit"},
		{"machine max age", &State{URL: "https://org", Keys: pins, Envelope: &fresh}, Machine{MaxAge: time.Second}, StatusFallback, true, "past its limit"},
		{"future", &State{URL: "https://org", Keys: pins, Envelope: &future}, Machine{}, StatusFallback, true, "in the future"},
		{"never fetched", &State{URL: "https://org", Keys: pins}, Machine{}, StatusFallback, false, "no policy has been fetched"},
		{"refused fetch", &State{URL: "https://org", Keys: pins, Envelope: &fresh, FetchedAt: now.Add(-time.Minute),
			Failure: &Failure{At: now, Reason: "bad"}}, Machine{}, StatusFallback, true, "was refused"},
		{"older failure", &State{URL: "https://org", Keys: pins, Envelope: &fresh, FetchedAt: now,
			Failure: &Failure{At: now.Add(-time.Hour), Reason: "bad"}}, Machine{}, StatusApplied, true, ""},
		{"other server", &State{URL: "https://evil", Keys: pins, Envelope: &fresh}, Machine{URL: "https://org"}, StatusNone, false, "is not taken"},
		{"other server, required", &State{URL: "https://evil", Keys: pins, Envelope: &fresh}, Machine{URL: "https://org", Required: true}, StatusFallback, false, "only from"},
		{"tampered pins", &State{URL: "https://org", Keys: []string{"x"}, Envelope: &fresh}, Machine{}, StatusFallback, false, "do not read"},
	}
	for _, c := range cases {
		e := Evaluate(c.st, c.m, now)
		if e.Status != c.status || (e.Doc != nil) != c.doc || !strings.Contains(e.Reason, c.why) {
			t.Errorf("%s: status %s doc %v reason %q", c.name, e.Status, e.Doc != nil, e.Reason)
		}
		if c.status == StatusFallback && !e.Fallback {
			t.Errorf("%s: fallback status without Fallback", c.name)
		}
	}
}

// The machine's own keys are the only ones trusted when it sets any: a key
// the state file pins, which its owner can write, is not.
func TestMachineKeysOverrideTheStatesPins(t *testing.T) {
	orgPub, _ := newKey(t)
	ownPub, ownPriv := newKey(t)
	env := sign(t, stmt(9, `{"permissions":{"mode":"bypass"}}`, time.Now(), ownPub), ownPriv)
	st := &State{URL: "https://org", Keys: []string{EncodeKey(ownPub)}, Envelope: &env}
	e := Evaluate(st, Machine{Keys: []string{EncodeKey(orgPub)}, Required: true}, time.Now())
	if e.Status != StatusFallback || !strings.Contains(e.Reason, "does not verify") {
		t.Fatalf("a policy signed by a key only the state pins applied: %+v", e)
	}
}

func TestAcceptVerifiesRotatesAndRefusesRollback(t *testing.T) {
	k1, p1 := newKey(t)
	k2, p2 := newKey(t)
	now := time.Now().UTC()
	st := &State{URL: "https://org", Keys: []string{EncodeKey(k1)}}
	// Signed by k1, announcing k1 and k2: both are pinned after.
	if _, err := Accept(st, Machine{}, sign(t, stmt(1, `{}`, now, k1, k2), p1), "alice", nil, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Keys) != 2 || st.Epoch != 1 || st.Envelope == nil || st.Failure != nil {
		t.Fatalf("state after accept: %+v", st)
	}
	// Then signed by k2 alone, announcing only k2: k1 is let go.
	if _, err := Accept(st, Machine{}, sign(t, stmt(2, `{}`, now, k2), p2), "alice", nil, now); err != nil {
		t.Fatal(err)
	}
	if len(st.Keys) != 1 || st.Keys[0] != EncodeKey(k2) {
		t.Fatalf("pins after rotation: %v", st.Keys)
	}
	good := st.Envelope
	// k1 no longer verifies.
	if _, err := Accept(st, Machine{}, sign(t, stmt(3, `{}`, now, k1), p1), "alice", nil, now.Add(time.Second)); err == nil {
		t.Fatal("a statement from a let-go key was accepted")
	}
	if st.Failure == nil || st.Envelope != good {
		t.Fatalf("a refusal must be kept and the good policy left: %+v", st)
	}
	if e := Evaluate(st, Machine{}, now.Add(2*time.Second)); e.Status != StatusFallback {
		t.Errorf("after a refused fetch the fallback must apply, got %s", e.Status)
	}
	// An earlier epoch is a rollback, even signed by the current key.
	if _, err := Accept(st, Machine{}, sign(t, stmt(1, `{}`, now, k2), p2), "alice", nil, now); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("rollback: %v", err)
	}
	// Another person's statement.
	if _, err := Accept(st, Machine{}, sign(t, stmt(4, `{}`, now, k2), p2), "bob", nil, now); err == nil {
		t.Fatal("a statement issued for another subject was accepted")
	}
	// A document the configuration does not load.
	if _, err := Accept(st, Machine{}, sign(t, stmt(4, `{}`, now, k2), p2), "alice", func([]byte) error { return errors.New("no") }, now); err == nil {
		t.Fatal("a policy that does not load was accepted")
	}
	// Accepted again: the failure clears.
	if _, err := Accept(st, Machine{}, sign(t, stmt(4, `{}`, now, k2), p2), "alice", nil, now.Add(time.Minute)); err != nil || st.Failure != nil {
		t.Fatalf("accept: %v %+v", err, st.Failure)
	}
}

func TestMachinePinnedKeysAreNotRotated(t *testing.T) {
	k1, p1 := newKey(t)
	k2, _ := newKey(t)
	st := &State{URL: "https://org"}
	m := Machine{Keys: []string{EncodeKey(k1)}}
	if _, err := Accept(st, m, sign(t, stmt(1, `{}`, time.Now(), k2), p1), "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(st.Keys) != 0 {
		t.Errorf("a statement rotated machine-pinned keys into the state: %v", st.Keys)
	}
}

func TestStateFileIsPrivateAndRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "org-policy.json")
	if st, err := LoadState(path); st != nil || err != nil {
		t.Fatalf("missing: %v %v", st, err)
	}
	if err := SaveState(path, &State{URL: "https://org", Keys: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	st, err := LoadState(path)
	if err != nil || st.URL != "https://org" {
		t.Fatalf("load: %v %+v", err, st)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if _, err := LoadState(link); err == nil {
		t.Error("a link was read as the state")
	}
	if err := SaveState(link, st); err == nil {
		t.Error("the state was written through a link")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Error("a state others may write was read")
	}
}
