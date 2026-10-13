package app

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// trustIn is where abhed trust startup-files reads its answer; tests replace it.
var trustIn io.Reader = os.Stdin

// startupFile is a shell start-up file in the person's HOME as it is now.
type startupFile struct {
	path   string
	size   int64
	sha256 string
	data   []byte
	ok     bool   // a regular file Abhed can trust by content
	by     string // the start-up file that sources or runs it, "" for one itself
}

// maxStartupClosure bounds the files the start-up files source or run that
// are shown and trusted with them.
const maxStartupClosure = 64

// homeStartupFiles are the start-up files present in the person's HOME, and
// the files they source or run by a path the check can resolve.
func homeStartupFiles() ([]startupFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var out []startupFile
	seen := map[string]bool{}
	add := func(p, by string) {
		if seen[p] || len(out) >= maxStartupClosure {
			return
		}
		seen[p] = true
		data, ok := policy.ReadStartupFile(p)
		f := startupFile{path: p, data: data, ok: ok, by: by}
		if ok {
			sum := sha256.Sum256(data)
			f.size, f.sha256 = int64(len(data)), hex.EncodeToString(sum[:])
		}
		out = append(out, f)
	}
	for _, n := range tools.HomeStartupNames() {
		if p := filepath.Join(home, n); fileThere(p) {
			add(p, "")
		}
	}
	env := map[string]string{"HOME": home}
	for i := 0; i < len(out); i++ {
		if !out[i].ok {
			continue
		}
		for _, s := range tools.CanonicalCommandEnv(string(out[i].data), env).Scripts() {
			p := s.Path
			if p == "~" || strings.HasPrefix(p, "~/") {
				p = home + p[1:]
			}
			if filepath.IsAbs(p) && fileThere(p) {
				add(filepath.Clean(p), out[i].path)
			}
		}
	}
	return out, nil
}

// fileThere reports a path that names anything at all, a link included.
func fileThere(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// trustStartupFilesCmd shows the shell start-up files in the person's HOME
// and, once confirmed, records them as the baseline the policy trusts.
func trustStartupFilesCmd(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("abhed trust startup-files", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("y", false, "record them without asking")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	files, err := homeStartupFiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: trust startup-files: %v\n", err)
		return 1
	}
	var keep []config.TrustedStartupFile
	for _, f := range files {
		if !f.ok {
			fmt.Fprintf(out, "%s\n  not a regular file, or too long: always judged by what it holds\n", config.Printable(f.path))
			continue
		}
		by := ""
		if f.by != "" {
			by = ", sourced or run by " + config.Printable(f.by)
		}
		fmt.Fprintf(out, "%s\n  %d bytes, sha256 %s%s\n", config.Printable(f.path), f.size, f.sha256, by)
		for _, l := range shownLines(f.data) {
			fmt.Fprintf(out, "  | %s\n", l)
		}
		keep = append(keep, config.TrustedStartupFile{Path: f.path, Size: f.size, SHA256: f.sha256, At: time.Now().UTC()})
	}
	if len(keep) == 0 {
		fmt.Fprintln(out, "No shell start-up file to trust in your home folder.")
	} else {
		fmt.Fprintf(out, "\nThese are the files a shell the agent starts on the process tier or none reads first, and those they source or run.\n"+
			"Trusted, each one's own text is not judged against the rules while it stays exactly as shown; a change makes it\n"+
			"judged again. What a file sources or runs that is not listed here is still judged, and on a line that sets the\n"+
			"shell's environment (an assignment, env, export or cd) nothing is trusted.\n")
		if !*yes {
			fmt.Fprintf(out, "Trust these %d file(s) as your own? [y/N] ", len(keep))
			answer, err := readAnswer(trustIn)
			if err != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
				fmt.Fprintln(out, "Nothing was recorded.")
				return 1
			}
		}
	}
	if err := config.TrustStartupFiles(keep); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: trust startup-files: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "Recorded %d start-up file(s) as yours.\n", len(keep))
	return 0
}

// shownLines are a file's lines as read, with control characters shown as ?.
func shownLines(data []byte) []string {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		out = append(out, strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\t' {
				return '?'
			}
			return r
		}, l))
	}
	return out
}

// printDoctorStartupFiles lists the trusted start-up files and any changed
// since, and suggests the command when one would be judged.
func printDoctorStartupFiles(out io.Writer) {
	files, err := homeStartupFiles()
	if err != nil || len(files) == 0 {
		return
	}
	base := config.StartupBaseline()
	judged := 0
	for _, f := range files {
		want, had := base[f.path]
		switch {
		case had && f.ok && want.Size == f.size && want.SHA256 == f.sha256:
			fmt.Fprintf(out, "startup     %s trusted (sha256 %s)\n", config.Printable(f.path), f.sha256[:12])
		case had:
			fmt.Fprintf(out, "startup     %s ⚠ changed since you trusted it: judged by what it holds\n", config.Printable(f.path))
			judged++
		default:
			fmt.Fprintf(out, "startup     %s judged by what it holds\n", config.Printable(f.path))
			judged++
		}
	}
	if judged > 0 {
		fmt.Fprintln(out, "            under deny or ask rules, a shell reading a file Abhed cannot follow (such as one a package manager wrote) asks; if they are your own, run `abhed trust startup-files`")
	}
}
