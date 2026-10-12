package server

import (
	"strings"
	"testing"
)

// A local sign-in goes back to the page that sent the person to sign in,
// when that is a path on this server, and to the workbench otherwise.
func TestSignInHonoursASameOriginReturn(t *testing.T) {
	node := pageTestNode(t)
	fn := jsFunction(t, landingHTML, "function returnPath(){")
	cases := map[string]string{
		"?return=%2Fadmin":                "/admin",
		"?return=%2Faccount%3Fx%3D1":      "/account?x=1",
		"":                                "/ide",
		"?return=https%3A%2F%2Fevil.test": "/ide",
		"?return=%2F%2Fevil.test%2Fx":     "/ide",
		"?return=%2F%5Cevil.test":         "/ide",
		"?return=%2Flogout":               "/ide",
		"?return=javascript%3Aalert(1)":   "/ide",
	}
	var b strings.Builder
	b.WriteString("let ok = true;\n")
	for q, want := range cases {
		b.WriteString("globalThis.location = {search: " + jsString(q) + ", origin: 'http://h.test'};\n")
		b.WriteString("if(returnPath() !== " + jsString(want) + "){ console.log('FAIL ' + " + jsString(q) + " + ' -> ' + returnPath()); ok = false; }\n")
	}
	b.WriteString("if(!ok) process.exit(1);\n")
	if out, err := runNodeScript(t, node, fn+"\n"+b.String()); err != nil {
		t.Fatalf("returnPath:\n%s", out)
	}
}

// The account page of a single sign-on account keeps its way back and its
// Sign out, replacing only the form and the line naming who is signed in.
func TestAccountPageKeepsItsNavigationForSingleSignOn(t *testing.T) {
	if strings.Contains(accountHTML, "querySelector('main').textContent") {
		t.Fatal("the account page still replaces all of main")
	}
	if !strings.Contains(accountHTML, "$('f').hidden = true") {
		t.Fatal("the account page does not hide its form for a single sign-on account")
	}
}

func jsString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
