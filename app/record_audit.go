package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/ocsf"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/chain"
	"github.com/zybuu-ai/abhed/store/local"
	"github.com/zybuu-ai/abhed/store/sqlite"
)

// auditTarget is one control-plane audit chain to check, through chain.Source.
type auditTarget struct {
	name, scope string
	unit        string // what a link is called: "row" or "line"
	src         chain.Source
	// unchained, when set, says what the chain does not cover: a note for
	// rows from before it or a torn last line, or a failure for any other.
	unchained func(context.Context) (note, failed string, err error)
	// rows gives the chain's rows in order with their links, for -format ocsf.
	rows      func(context.Context, func(controlplane.AuditEvent, chain.Link) error) error
	chainName string
}

// verifyAudit is `abhed record audit`: it walks the control-plane audit
// chains serve writes (the database's on Postgres; otherwise the local file
// and the control plane's own SQLite record) and checks each against any
// head given for the API's chain.
func verifyAudit(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	heads := headsFlag{}
	fs.Var(heads, "head", "a head of the API's audit chain kept elsewhere, position:hash, that it must still hold (repeatable)")
	format := fs.String("format", "text", "text: check the chains; ocsf: print every row as an OCSF event, one a line")
	version := fs.String("ocsf-version", ocsf.Version, "the OCSF version for -format ocsf")
	if fs.Parse(args) != nil || fs.NArg() > 0 || (*format != "text" && *format != "ocsf") {
		fmt.Fprintln(stderr, "usage: abhed record audit [-head pos:hash] [-format text|ocsf] [-ocsf-version V]")
		return 2
	}
	if err := ocsf.CheckVersion(*version); err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 2
	}
	ctx := context.Background()
	var targets []auditTarget
	scope := "A passing check shows no line or row was edited, added, moved or removed. The machine's owner can rewrite the files and their chains."
	if cfg.Storage.Driver == "postgres" {
		pg, err := store.Open(ctx, storeConfig(cfg))
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		defer pg.Close()
		targets = append(targets, auditTarget{name: "audit (tenant " + pg.Tenant() + ")", scope: pg.Tenant(), unit: "row", src: pg.AuditChain(),
			rows: pg.AuditRows, chainName: store.AuditChainName,
			unchained: func(ctx context.Context) (string, string, error) {
				r, err := pg.VerifyAuditChain(ctx)
				if err != nil || r.OK() {
					return preChainNote(r.PreChain), "", err
				}
				return "", r.Reason, nil
			}})
		scope = "A passing check shows no audit row was changed, removed or reordered, and none written with the chain's trigger off; " +
			"each hash is recomputed here. The table's owner can recompute the chain or cut its end; a head kept elsewhere (-head) shows that."
	} else {
		tenant := orDefault(cfg.Storage.Tenant, "default")
		path := local.AuditPath(orDefault(cfg.Record.Dir, mustDefaultDir()), tenant)
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(stderr, "abhed: the audit record: %v\n", err)
			return 1
		}
		targets = append(targets, auditTarget{name: path, scope: tenant, unit: "line", src: local.AuditChain(cfg.Record.Dir),
			rows: func(ctx context.Context, fn func(controlplane.AuditEvent, chain.Link) error) error {
				return local.AuditRows(ctx, path, fn)
			}, chainName: local.AuditChainName,
			unchained: func(context.Context) (string, string, error) {
				r, err := local.VerifyAudit(path)
				if err != nil || !r.Torn {
					return "", "", err
				}
				return "; its last line is incomplete, as a crash leaves it, and is cut off at the next write", "", nil
			}})
		if path := sandboxconfig.ControlPlaneDB(cfg); path != "" {
			if _, err := os.Stat(path); err == nil {
				db, err := sqlite.Open(ctx, path)
				if err != nil {
					fmt.Fprintf(stderr, "abhed: the control plane's record: %v\n", err)
					return 1
				}
				defer func() { _ = db.Close() }()
				targets = append(targets, auditTarget{name: db.Path(), unit: "row", src: db.AuditChain(),
					rows: db.AuditRows, chainName: sqlite.AuditChainName,
					unchained: func(ctx context.Context) (string, string, error) {
						r, err := db.VerifyAuditChain(ctx)
						if err != nil || r.OK() {
							return preChainNote(r.Unchained), "", err
						}
						return "", r.Reason, nil
					}})
			} else if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "abhed: the control plane's record: %v\n", err)
				return 1
			}
		}
	}
	if *format == "ocsf" {
		return auditOCSF(ctx, cfg, targets, *version, stdout, stderr)
	}
	bad := false
	for i, t := range targets {
		var h map[int64]string
		if i == 0 {
			h = heads
		}
		ok, err := checkAuditChain(ctx, t, h, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %s: %v\n", t.name, err)
			return 1
		}
		bad = bad || !ok
	}
	fmt.Fprintln(stdout, scope)
	if bad {
		return 1
	}
	return 0
}

// checkAuditChain checks one chain whole, then each kept head against it.
func checkAuditChain(ctx context.Context, t auditTarget, heads map[int64]string, stdout io.Writer) (bool, error) {
	r, err := t.src.VerifyRange(ctx, t.scope, 1, 0)
	if err != nil {
		return false, err
	}
	if !r.OK() {
		fmt.Fprintf(stdout, "FAILED  %s: %s\n", t.name, r.Reason)
		return false, nil
	}
	note := ""
	if t.unchained != nil {
		n, failed, err := t.unchained(ctx)
		if err != nil {
			return false, err
		}
		if failed != "" {
			fmt.Fprintf(stdout, "FAILED  %s: %s\n", t.name, failed)
			return false, nil
		}
		note = n
	}
	for _, pos := range slices.Sorted(maps.Keys(heads)) {
		kept, err := t.src.VerifyRange(ctx, t.scope, 1, pos)
		if err != nil {
			return false, err
		}
		if !kept.OK() || kept.Hash != heads[pos] {
			fmt.Fprintf(stdout, "FAILED  %s: position %d is not the head kept elsewhere: the chain was cut or rewritten\n", t.name, pos)
			return false, nil
		}
	}
	fmt.Fprintf(stdout, "ok      %s: %d chained %s(s), head %s%s\n", t.name, r.To, t.unit, short(r.Hash), note)
	if len(heads) > 0 {
		fmt.Fprintf(stdout, "%d head(s) given were checked\n", len(heads))
	}
	return true, nil
}

func preChainNote(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d row(s) from before the chain, not covered)", n)
}

// mustDefaultDir is the default records directory, or "" when there is no home.
func mustDefaultDir() string {
	d, _ := local.DefaultDir()
	return d
}

// auditOCSF prints every row of the audit chains as OCSF API Activity, its
// place in its chain under unmapped.record. It reads; it does not check.
func auditOCSF(ctx context.Context, cfg config.Config, targets []auditTarget, version string, stdout, stderr io.Writer) int {
	opts := ocsfOptions(orDefault(cfg.Storage.Tenant, "default"))
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	for _, t := range targets {
		err := t.rows(ctx, func(a controlplane.AuditEvent, l chain.Link) error {
			ev := ocsf.MapAuditLink(a, &ocsf.AuditLink{Chain: t.chainName, Pos: l.Pos, Prev: l.Prev, Hash: l.Hash}, opts)
			if version != ocsf.Version {
				var err error
				if ev, err = ocsf.Downgrade(ev, version); err != nil {
					return err
				}
			}
			return enc.Encode(ev)
		})
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %s: %v\n", t.name, err)
			return 1
		}
	}
	return 0
}
