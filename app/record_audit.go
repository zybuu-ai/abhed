package app

import (
	"context"
	"fmt"
	"io"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// verifyAudit is `abhed record audit`: it walks the control-plane audit
// chain serve writes, in the database on Postgres or the local file.
func verifyAudit(cfg config.Config, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: abhed record audit")
		return 2
	}
	if cfg.Storage.Driver == "postgres" {
		ctx := context.Background()
		pg, err := store.Open(ctx, storeConfig(cfg))
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		defer pg.Close()
		r, err := pg.VerifyAuditChain(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		if !r.OK() {
			fmt.Fprintf(stdout, "FAILED  audit (tenant %s): the chain breaks at row %d of %d: a row was changed, removed or put out of order\n", pg.Tenant(), r.FirstBad, r.Rows)
			return 1
		}
		note := ""
		if r.Unchained > 0 {
			note = fmt.Sprintf(" (%d row(s) from before the chain, not covered)", r.Unchained)
		}
		fmt.Fprintf(stdout, "ok      audit (tenant %s): %d chained row(s), head %s%s\n", pg.Tenant(), r.Rows, short(r.Head), note)
		fmt.Fprintln(stdout, "A passing check shows no audit row was changed, removed or reordered by anyone short of the table's owner, who can recompute the chain.")
		return 0
	}
	dir := cfg.Record.Dir
	if dir == "" {
		d, err := local.DefaultDir()
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		dir = d
	}
	path := local.AuditPath(dir, orDefault(cfg.Storage.Tenant, "default"))
	r, err := local.VerifyAudit(path)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: the audit record: %v\n", err)
		return 1
	}
	if !r.OK() {
		fmt.Fprintf(stdout, "FAILED  %s: line %d: %s\n", path, r.FirstBad, r.Why)
		return 1
	}
	torn := ""
	if r.Torn {
		torn = "; its last line is incomplete, as a crash leaves it, and is cut off at the next write"
	}
	fmt.Fprintf(stdout, "ok      %s: %d chained line(s), head %s%s\n", path, r.Lines, short(r.Head), torn)
	fmt.Fprintln(stdout, "A passing check shows no line was edited, added, moved or removed. The machine's owner can rewrite the file and its chain.")
	return 0
}
