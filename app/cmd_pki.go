package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/pki"
)

// defaultPKIDir is ~/.abhed/pki: outside every workspace, and under the
// directory the agent's tools and sandbox cannot reach.
func defaultPKIDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "pki")
}

func pkiCmd(args []string, out, errOut io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errOut, "usage: abhed pki init | server -dns NAME[,NAME] [-ip ADDR[,ADDR]] | client -user ACCOUNT  [-dir DIR] [-rsa] [-days N] [-force]\n"+
			"  A private CA for one deployment, its server certificates and client certificates.\n"+
			"  Files go to "+defaultPKIDir()+" unless -dir says otherwise; keys are written 0600 and never printed.")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	verb := args[0]
	fs := flag.NewFlagSet("pki "+verb, flag.ContinueOnError)
	fs.SetOutput(errOut)
	dir := fs.String("dir", defaultPKIDir(), "directory holding the CA and the certificates")
	rsa := fs.Bool("rsa", false, "RSA 3072 keys instead of ECDSA P-256")
	days := fs.Int("days", 0, "validity in days (default: CA 5 years, server 397 days, client 1 year)")
	force := fs.Bool("force", false, "replace files that exist")
	name := fs.String("name", "", "file name (server, client) or CA common name (init)")
	dns := fs.String("dns", "", "server: DNS names, comma-separated")
	ips := fs.String("ip", "", "server: IP addresses, comma-separated")
	user := fs.String("user", "", "client: the local account the certificate signs in")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *days < 0 || *days > 3650 {
		return usage()
	}
	o := pki.Options{Dir: *dir, Days: *days, Force: *force, Name: *name, Key: pki.ECDSA}
	if *rsa {
		o.Key = pki.RSA
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	var (
		got pki.Issued
		err error
	)
	switch verb {
	case "init":
		got, err = pki.InitCA(o)
	case "server":
		o.DNS = append(o.DNS, splitList(*dns)...)
		for _, s := range splitList(*ips) {
			ip := net.ParseIP(s)
			if ip == nil {
				return fail(fmt.Errorf("-ip %q is not an IP address", s))
			}
			o.IPs = append(o.IPs, ip)
		}
		got, err = pki.IssueServer(o)
	case "client":
		o.Subject = *user
		got, err = pki.IssueClient(o)
	default:
		return usage()
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "certificate  %s\nkey          %s (mode 600; keep it private)\nsubject      %s\nexpires      %s\nsha256       %s\n",
		got.CertFile, got.KeyFile, got.Subject, got.NotAfter.Format("2006-01-02"), got.Fingerprint)
	switch verb {
	case "init":
		fmt.Fprintf(out, "\nNext: abhed pki server -dir %s -dns HOSTNAME, then set server.tls.cert_file and key_file.\n"+
			"For client certificates, set server.tls.client_ca_file to %s.\n", *dir, got.CertFile)
	case "client":
		fmt.Fprintf(out, "\nGive the certificate and key to %s only; abhed login -cert and -key use them.\n", *user)
	}
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
