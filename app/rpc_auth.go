package app

import (
	"errors"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/identity"
)

// signInNotice is what the last sign-in must tell the person, such as a
// password reset since their previous one; the surface that signed them in
// shows it and takes it.
var signInNotice string

func setSignInNotice(s string) {
	principalMu.Lock()
	signInNotice = s
	principalMu.Unlock()
}

func takeSignInNotice() string {
	principalMu.Lock()
	defer principalMu.Unlock()
	n := signInNotice
	signInNotice = ""
	return n
}

// rpcPrincipal is who is working, as rpc and acp report it.
type rpcPrincipal struct {
	Kind    string `json:"kind"`
	Display string `json:"display"`
	// Pending, when set, is why nothing can start until someone signs in.
	Pending string `json:"pending,omitempty"`
}

func principalReport() *rpcPrincipal {
	p := currentPrincipal()
	r := &rpcPrincipal{Kind: string(p.Kind), Display: p.Display()}
	if err := pendingSignIn(); err != nil {
		r.Kind, r.Display, r.Pending = "", "", err.Error()
	}
	return r
}

// rpcAuth answers the auth methods. Who is working can change only before
// the first start: the person's folder is fixed once a session has opened.
func rpcAuth(req rpcRequest, started bool) rpcResponse {
	fail := func(err error) rpcResponse { return rpcResponse{ID: req.ID, Type: "error", Error: err.Error()} }
	if req.Method == "auth.status" {
		return rpcResponse{ID: req.ID, Type: "auth", Principal: principalReport()}
	}
	if started {
		return fail(errors.New("who is working is fixed once a session has started; run another abhed rpc to change it"))
	}
	code, err := switchPrincipal(req.Method, req.Username, req.Password, req.Email)
	if err != nil {
		return fail(err)
	}
	return rpcResponse{ID: req.ID, Type: "auth", Principal: principalReport(), RecoveryCode: code, Notice: takeSignInNotice()}
}

// switchPrincipal signs in, registers or turns guest for a client that asks
// on behalf of the person (rpc, acp), before anything has started. It
// returns the recovery code of a new account.
func switchPrincipal(method, username, password, email string) (string, error) {
	// Who is working is the person's to say, never the agent's (as selfAdmin).
	if why := inAgentCommand(); why != "" {
		return "", fmt.Errorf("signing in is refused inside an agent's command (%s)", why)
	}
	rules, err := clientRules()
	if err != nil {
		return "", err
	}
	s, err := identity.Open()
	if err != nil {
		return "", err
	}
	var next identity.Principal
	code := ""
	switch method {
	case "auth.guest":
		if !rules.GuestAllowed {
			why := rules.GuestOff
			if why == "" {
				why = "your organisation has turned guest off"
			}
			return "", &identity.RequiredError{Why: why}
		}
		next = identity.GuestPrincipal()
	case "auth.signin":
		if rules.SignIn == "server" {
			return "", &identity.RequiredError{Why: "your organisation requires a sign-in to " + rules.Server,
				Next: "run abhed login " + rules.Server + " in a terminal"}
		}
		e, err := s.Authenticate(username, password)
		if err != nil {
			return "", err
		}
		if err := s.Remember(e.ID); err != nil {
			return "", err
		}
		if r, _ := s.TakeReset(e.ID); r != nil {
			setSignInNotice(resetNote(r))
		}
		next = s.Principal(e)
	case "auth.register":
		if rules.SignIn == "server" {
			return "", &identity.RequiredError{Why: "your organisation requires a sign-in to " + rules.Server}
		}
		e, c, err := s.Create(username, password, email, "")
		if err != nil {
			return "", err
		}
		if err := s.Remember(e.ID); err != nil {
			return "", err
		}
		next, code = s.Principal(e), c
	default:
		return "", errors.New("unknown auth method")
	}
	endGuest()
	if err := usePrincipal(next); err != nil {
		return "", err
	}
	setPendingSignIn(nil)
	return code, nil
}
