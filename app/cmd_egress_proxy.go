package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
)

// egressProxyOptions are `abhed egress-proxy`'s settings.
type egressProxyOptions struct {
	listen string
	config string
	call   string
	check  bool
}

// egressProxyCmd runs the egress allowlist proxy on its own, for a sandbox
// whose only way out is this process: a Kubernetes egress pod, say.
func egressProxyCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o egressProxyOptions
	fs.StringVar(&o.listen, "listen", "", "address to serve on, such as :3128 (required)")
	fs.StringVar(&o.config, "config", "", "JSON file holding the egress section: rules, default, mode, record_paths, idle_seconds (required)")
	fs.StringVar(&o.call, "call", "sandbox", "the call id every decision is recorded under")
	fs.BoolVar(&o.check, "check", false, "check the configuration, then exit without serving")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: abhed egress-proxy -listen ADDR -config FILE [-call ID] [-check]")
		fmt.Fprintln(errOut, "Serves the egress allowlist; each decision is one JSON line on standard output.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || o.config == "" || (o.listen == "" && !o.check) {
		fs.Usage()
		return 2
	}
	pol, err := loadEgressPolicy(o.config)
	if err != nil {
		fmt.Fprintf(errOut, "abhed egress-proxy: %s: %v\n", o.config, err)
		return 1
	}
	if o.check {
		fmt.Fprintf(out, "abhed egress-proxy: %s: %d rules, default %s\n", o.config, pol.Rules(), egressDefault(pol))
		return 0
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		fmt.Fprintf(errOut, "abhed egress-proxy: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := serveEgressProxy(ctx, ln, pol, o.call, out, errOut); err != nil {
		fmt.Fprintf(errOut, "abhed egress-proxy: %v\n", err)
		return 1
	}
	return 0
}

// loadEgressPolicy reads and compiles the egress section, refusing what the
// stand-alone proxy cannot honour rather than running weaker than written.
func loadEgressPolicy(path string) (*egress.Policy, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator's own configuration file
	if err != nil {
		return nil, err
	}
	if err := config.CheckManagedJSON(data); err != nil {
		return nil, err
	}
	var c egress.Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	if c.InspectTLS {
		// Its clients would need the proxy's CA, which nothing hands them here.
		return nil, errors.New("inspect_tls is not supported by the stand-alone proxy")
	}
	return egress.Compile(c)
}

func egressDefault(p *egress.Policy) string {
	if p.DefaultAllow() {
		return "allow"
	}
	return "deny"
}

// serveEgressProxy serves ln as one call's proxy until ctx ends, writing each
// decision to out as a JSON line, then records the pending summaries.
func serveEgressProxy(ctx context.Context, ln net.Listener, pol *egress.Policy, call string, out, errOut io.Writer) error {
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	record := func(e egress.Event) {
		mu.Lock()
		defer mu.Unlock()
		if err := enc.Encode(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano),
			"event": egress.EventName, "payload": e.Payload()}); err != nil {
			fmt.Fprintf(errOut, "abhed egress-proxy: writing the record: %v\n", err)
		}
	}
	p, err := egress.Start(egress.Options{Policy: pol, Record: record})
	if err != nil {
		_ = ln.Close()
		return err
	}
	c, err := p.Issue(call)
	if err != nil {
		_ = ln.Close()
		_ = p.Close()
		return err
	}
	if err := c.Serve(ln); err != nil {
		_ = p.Close()
		return err
	}
	fmt.Fprintf(errOut, "abhed egress-proxy: serving on %s, %d rules, default %s\n", ln.Addr(), pol.Rules(), egressDefault(pol))
	<-ctx.Done()
	c.End()
	return p.Close()
}
