package app

import (
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
	ok     bool // a regular file Abhed can trust by content
}

// homeStartupFiles are the start-up files present in the person's HOME.
func homeStartupFiles() ([]startupFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var out []startupFile
	for _, n := range tools.HomeStartupNames() {
		p := filepath.Join(home, n)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		size, sum, ok := policy.StartupFileMark(p)
		out = append(out, startupFile{path: p, size: size, sha256: sum, ok: ok})
	}
	return out, nil
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
		fmt.Fprintf(out, "%s\n  %d bytes, sha256 %s\n", config.Printable(f.path), f.size, f.sha256)
		for _, l := range firstLines(f.path, 3) {
			fmt.Fprintf(out, "  | %s\n", l)
		}
		keep = append(keep, config.TrustedStartupFile{Path: f.path, Size: f.size, SHA256: f.sha256, At: time.Now().UTC()})
	}
	if len(keep) == 0 {
		fmt.Fprintln(out, "No shell start-up file to trust in your home folder.")
	} else {
		fmt.Fprintf(out, "\nThese are the files a shell the agent starts on the process tier or none reads first. Trusted, each is not judged\n"+
			"against the rules while it stays exactly as shown; a change makes it judged again.\n")
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

// firstLines are a file's first n lines, with control characters shown as ?.
func firstLines(p string, n int) []string {
	data, err := os.ReadFile(p) // #nosec G304 -- a start-up file in the person's own HOME
	if err != nil {
		return nil
	}
	lines := strings.SplitN(string(data), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	var out []string
	for _, l := range lines {
		s := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\t' {
				return '?'
			}
			return r
		}, l)
		if len(s) > 100 {
			s = s[:100] + "…"
		}
		out = append(out, s)
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
