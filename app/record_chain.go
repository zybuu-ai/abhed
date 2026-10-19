package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/chain"
	"github.com/zybuu-ai/abhed/store/witness"
)

// headsFlag collects -head pos:hash, a head kept somewhere else to check an
// export against.
type headsFlag map[int64]string

func (h headsFlag) String() string { return "" }

func (h headsFlag) Set(v string) error {
	p, hash, ok := strings.Cut(v, ":")
	n, err := strconv.ParseInt(p, 10, 64)
	if !ok || err != nil || n < 1 || len(hash) != 64 {
		return errors.New("a head is position:hash, such as 12:<64 hex digits>")
	}
	h[n] = strings.ToLower(hash)
	return nil
}

// isChainExport reports whether path is a file written by exporting a
// Postgres chain.
func isChainExport(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- a file the person names to check
	if err != nil {
		return false
	}
	defer f.Close()
	start := make([]byte, 4096)
	n, _ := io.ReadFull(bufio.NewReader(f), start)
	return chain.IsExport(start[:n])
}

// verifyChainExport checks an exported chain with nothing but the file, the
// heads given, and the heads a witness file holds for its session. It
// reports whether the check failed.
func verifyChainExport(cfg config.Config, path string, heads map[int64]string, witnessPath, witnessKeyGiven string, stdout, stderr io.Writer) bool {
	f, err := os.Open(path) // #nosec G304 -- a file the person names to check
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return true
	}
	defer f.Close()
	rep, held, err := chain.VerifyExport(f, heads)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %s: %v\n", path, err)
		return true
	}
	bad := !rep.OK()
	if bad {
		fmt.Fprintf(stdout, "FAILED  %s: %s: %s\n", path, rep.Header.Scope, rep.Reason)
	} else {
		fmt.Fprintf(stdout, "ok      %s: %s, %d chained event(s), head %s\n", path, rep.Header.Scope, rep.Links, short(rep.Hash))
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(stdout, "NOTE    %s\n", n)
	}
	if len(heads) > 0 {
		fmt.Fprintf(stdout, "%d head(s) given were checked\n", len(heads))
	}
	if witnessPath != "" {
		key, err := witnessKey(cfg, witnessKeyGiven)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: the witness's heads cannot be checked without the installation's key (-witness-key): %v\n", err)
			return true
		}
		wf, err := os.Open(witnessPath) // #nosec G304 -- the managed witness file, or one the person names
		if err != nil {
			fmt.Fprintf(stderr, "abhed: the witness's heads: %v\n", err)
			return true
		}
		defer wf.Close()
		all, unreadable := witness.Read(wf)
		for _, err := range unreadable {
			fmt.Fprintf(stdout, "FAILED  witness: %v\n", err)
		}
		var stmts []witness.Statement
		for _, s := range all {
			if s.Session == rep.Header.Scope {
				stmts = append(stmts, s)
			}
		}
		problems, err := witness.Check(stmts, key, "postgres", rep.Header.Tenant, func(_ string, n int64) (string, bool, error) {
			h, ok := held[n]
			return h, ok, nil
		}, nil)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: the witness's heads: %v\n", err)
			return true
		}
		printWitnessProblems(stdout, problems)
		fmt.Fprintf(stdout, "%d witnessed head(s) of %s checked against key %s\n", len(stmts), rep.Header.Scope, base64.StdEncoding.EncodeToString(key))
		bad = bad || len(unreadable) > 0 || witness.Failed(problems) > 0
	}
	return bad
}

// chainExportScope is what a passing check of an export shows, and what it cannot.
const chainExportScope = "A passing check shows every event in the file is as the database hashed it, in order, with none removed, " +
	"up to the head the file ends with. Whoever holds the file or the database can recompute a whole chain; " +
	"a head kept elsewhere (-head, or record.witness) is what shows that."

// exportPostgres is `abhed record export <session>` with storage.driver
// postgres: the session's chain, to check anywhere with `abhed record verify`.
func exportPostgres(cfg config.Config, workspace string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "where to write it (default ~/.abhed/exports/<session>.chain.jsonl; - for stdout)")
	unverified := fs.Bool("unverified", false, "export a chain that fails verification; the file then fails its own check")
	if fs.Parse(reorderFlags(args)) != nil || fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: abhed record export <session> [-o path] [-unverified]")
		return 2
	}
	id := fs.Arg(0)
	path := *out
	if path == "" {
		p, err := exportPath("", id+".chain", "jsonl")
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		path = p
	}
	ctx := context.Background()
	pg, err := store.Open(ctx, storeConfig(cfg))
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	defer pg.Close()
	if _, err := pg.GetSession(ctx, id); err != nil {
		fmt.Fprintf(stderr, "abhed: %s: %v\n", id, err)
		return 1
	}
	w := stdout
	var f *os.File
	if path != "-" {
		if f, err = openExport(path, workspace); err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		w = f
	}
	r, err := pg.ExportChain(ctx, w, id, *unverified)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	if path != "-" {
		fmt.Fprintf(stderr, "wrote %d chained event(s) of %s to %s\n", r.Rows, id, path)
	}
	return 0
}
