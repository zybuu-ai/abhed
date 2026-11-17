package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/profiles"
)

// Sign-in over ACP (docs/architecture/sign-in.md §2.3): the engine never
// prompts; it lists the ways in, in initialize's authMethods, and the
// client (Studio) shows the choice and calls authenticate. Until someone
// signs in where sign-in is required, everything else is refused with
// -32000 and data.authRequired.

// ACP method ids for the ways in.
const (
	acpAuthAccount  = "abhed.account"
	acpAuthRegister = "abhed.register"
	acpAuthServer   = "abhed.server"
	acpAuthGuest    = "abhed.guest"
)

// acpBeforeSignIn are the methods answered while a sign-in is pending.
var acpBeforeSignIn = map[string]bool{"initialize": true, "authenticate": true}

// acpAuthMethods are the ways in the rules allow, as ACP lists them.
func acpAuthMethods() []any {
	rules, err := clientRules()
	if err != nil {
		return []any{}
	}
	m := func(id, name, desc string) map[string]any {
		return map[string]any{"id": id, "name": name, "description": desc}
	}
	var out []any
	if rules.SignIn != "server" {
		out = append(out, m(acpAuthAccount, "Sign in", "An account on this machine"),
			m(acpAuthRegister, "Create an account", "On this machine; nothing leaves it"))
	}
	server := "Your own or your organisation's Abhed server"
	if rules.SignIn == "server" {
		server = rules.Server
	}
	out = append(out, m(acpAuthServer, "Sign in to a server", server))
	if rules.GuestAllowed {
		out = append(out, m(acpAuthGuest, "Guest", "Nothing is kept when the session ends"))
	}
	return out
}

// acpIdentity is who is working, for initialize's _meta: chosen is false
// when no one chose (a guest by default, or a sign-in pending), so the
// client should show the choice.
//
// owner names whose history this is, so the client can keep each person's
// apart (Studio keys its chat history by it); adopted is set for the account
// that took over the data from before sign-in, whose history is that data's.
func acpIdentity() map[string]any {
	r := principalReport()
	out := map[string]any{"kind": r.Kind, "display": r.Display, "chosen": r.Pending == "" && !guestByDefault()}
	if r.Pending != "" {
		out["pending"] = r.Pending
		return out
	}
	p := currentPrincipal()
	if p.Kind != identity.Legacy {
		out["owner"] = cliUser()
	}
	if p.Entry.LegacyUser != "" {
		out["adopted"] = true
	}
	return out
}

// signOut is _abhed/auth/signout: the machine forgets the remembered
// sign-in, so the engine's next start asks who is working. The engine
// keeps working as who it is until it is restarted.
func (c *acpConn) signOut(msg rpcMessage) {
	switch currentPrincipal().Kind {
	case identity.Account, identity.Server:
		s, err := identity.Open()
		if err == nil {
			err = s.Forget()
		}
		if err != nil {
			c.reply(msg.ID, nil, refusal(errRefused, "%s", err.Error()))
			return
		}
	}
	c.reply(msg.ID, map[string]any{"restart": true}, nil)
}

func init() {
	handle(map[string]func(*acpConn, rpcMessage){"_abhed/auth/signout": (*acpConn).signOut})
	liveFeatures = append(liveFeatures, "auth")
}

type acpAuthParams struct {
	MethodID string                        `json:"methodId"`
	Meta     map[string]acpAuthCredentials `json:"_meta"`
}

// acpAuthCredentials are authenticate's, under _meta's acpMetaKey.
type acpAuthCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	URL      string `json:"url"`
}

// authenticate signs the person in, registers them, or turns guest, before
// any session has opened.
func (c *acpConn) authenticate(msg rpcMessage) {
	var p acpAuthParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		c.reply(msg.ID, nil, badParams("authenticate", err))
		return
	}
	c.sessMu.Lock()
	opened := len(c.sessions) > 0
	c.sessMu.Unlock()
	if opened || c.rec != nil || c.recErr != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "who is working is fixed once a session has opened; restart the engine to change it"))
		return
	}
	a := p.Meta[acpMetaKey]
	var code string
	var err error
	switch p.MethodID {
	case acpAuthAccount:
		code, err = switchPrincipal("auth.signin", a.Username, a.Password, "")
	case acpAuthRegister:
		code, err = switchPrincipal("auth.register", a.Username, a.Password, a.Email)
	case acpAuthGuest:
		code, err = switchPrincipal("auth.guest", "", "", "")
	case acpAuthServer:
		err = c.serverSignIn(a.URL)
	default:
		c.reply(msg.ID, nil, refusal(errParams, "unknown methodId %q", p.MethodID))
		return
	}
	if err != nil {
		c.reply(msg.ID, nil, &rpcError{Code: errRefused, Message: err.Error(),
			Data: map[string]any{"authRequired": errors.Is(err, identity.ErrSignInRequired)}})
		return
	}
	// The record follows the person now working.
	c.useRecord(c.base, c.trust)
	res := map[string]any{"_meta": map[string]any{acpMetaKey: map[string]any{"identity": acpIdentity()}}}
	if code != "" {
		res["_meta"].(map[string]any)[acpMetaKey].(map[string]any)["recoveryCode"] = code
	}
	c.reply(msg.ID, res, nil)
}

// serverSignIn runs abhed login for a server sign-in, asking the client to
// open the browser: the engine's stdout is the protocol.
func (c *acpConn) serverSignIn(url string) error {
	rules, err := clientRules()
	if err != nil {
		return err
	}
	if rules.SignIn == "server" && strings.TrimSpace(url) == "" {
		url = rules.Server
	}
	base, err := profiles.NormalizeURL(strings.TrimSpace(url), false)
	if err != nil {
		return err
	}
	s, err := identity.Open()
	if err != nil {
		return err
	}
	old := openBrowser
	openBrowser = func(u string) error {
		c.notification("_abhed/auth/openUrl", map[string]any{"url": u})
		return nil
	}
	defer func() { openBrowser = old }()
	pr, err := signInViaLogin(s, rules, []string{"-browser", base}, os.Stderr, os.Stderr)
	if err != nil {
		return fmt.Errorf("the server sign-in did not complete: %w", err)
	}
	endGuest()
	if err := usePrincipal(pr); err != nil {
		return err
	}
	setPendingSignIn(nil)
	return nil
}
