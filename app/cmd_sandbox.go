package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/wsconn"
)

// sandboxCmd is `abhed sandbox`, a server's sandboxes from this command
// line. connect --stdio joins stdin and stdout to a sandbox's SSH server
// through the server's relay, for OpenSSH's ProxyCommand.
func sandboxCmd(workspace string, args []string, in io.Reader, out, errOut io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errOut, "usage: abhed sandbox connect --stdio [-profile NAME] SANDBOX_ID")
		return 2
	}
	if len(args) == 0 || args[0] != "connect" {
		return usage()
	}
	fs := flag.NewFlagSet("sandbox connect", flag.ContinueOnError)
	fs.SetOutput(errOut)
	stdio := fs.Bool("stdio", false, "join stdin and stdout to the sandbox's SSH server (for ProxyCommand)")
	name := fs.String("profile", "", "the server's profile (default: the current one)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*stdio || fs.NArg() != 1 {
		return usage()
	}
	id := fs.Arg(0)
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	if !controlplane.ValidID(id) {
		return fail(fmt.Errorf("%q is not a sandbox id", id))
	}
	path, err := profilesPath(workspace)
	if err != nil {
		return fail(err)
	}
	reg, err := profiles.Load(path)
	if err != nil {
		return fail(err)
	}
	_, p, err := reg.Get(*name)
	if err != nil {
		return fail(err)
	}
	if !p.SignedIn(time.Now()) && p.RefreshToken != "" {
		if client, err := p.HTTPClient(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if profiles.Refresh(ctx, client, p) == nil {
				_ = profiles.Save(path, reg)
			}
			cancel()
		}
	}
	if !p.SignedIn(time.Now()) {
		return fail(fmt.Errorf("the profile for %s is not signed in; run abhed login %s", p.URL, p.URL))
	}
	if err := connectStdio(context.Background(), p, id, in, out); err != nil {
		return fail(err)
	}
	return 0
}

// connectStdio relays in and out over the sandbox's SSH WebSocket until
// either side ends.
func connectStdio(ctx context.Context, p *profiles.Profile, id string, in io.Reader, out io.Writer) error {
	tc, err := p.TLSConfig()
	if err != nil {
		return err
	}
	h := http.Header{}
	if p.Token != "" {
		typ := p.TokenType
		if typ == "" || strings.EqualFold(typ, "bearer") {
			typ = "Bearer"
		}
		h.Set("Authorization", typ+" "+p.Token)
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, err := wsconn.Dial(dctx, p.URL+"/v1/sandboxes/"+id+":ssh", wsconn.DialOptions{
		Header: h, Protocols: []string{server.SSHRelayProtocol}, TLSConfig: tc})
	cancel()
	if err != nil {
		return relayRefusal(err)
	}
	stream := wsconn.NewStream(ws)
	inDone, outDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, in)
		inDone <- err
	}()
	go func() {
		_, err := io.Copy(out, stream)
		outDone <- err
	}()
	var outErr error
	finished := false
	select {
	case outErr = <-outDone:
		finished = true
	case err := <-inDone:
		// Input ended: the reply may still be on its way.
		select {
		case outErr = <-outDone:
			finished = true
		case <-time.After(5 * time.Second):
		}
		if err != nil && !errors.Is(err, wsconn.ErrClosed) {
			outErr = err
		}
	}
	_ = ws.Close()
	if !finished {
		// Nothing is written to out once this returns.
		select {
		case <-outDone:
		case <-time.After(3 * time.Second):
			ws.Abort()
			<-outDone
		}
	}
	if outErr == nil || wsconn.IsClose(outErr) || errors.Is(outErr, wsconn.ErrClosed) {
		return nil
	}
	return outErr
}

// relayRefusal says why the server refused the relay, in its own words.
func relayRefusal(err error) error {
	var he *wsconn.HandshakeError
	if !errors.As(err, &he) {
		return err
	}
	var body struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(he.Body, &body) == nil {
		switch e := body.Error.(type) {
		case string:
			return fmt.Errorf("the server refused the SSH relay (%d): %s", he.Status, e)
		case map[string]any:
			if msg, ok := e["message"].(string); ok {
				return fmt.Errorf("the server refused the SSH relay (%d): %s", he.Status, msg)
			}
		}
	}
	return fmt.Errorf("the server refused the SSH relay (%d)", he.Status)
}
