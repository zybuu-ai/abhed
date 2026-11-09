package policy

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// ReadOnlyRule names the read-only classifier as the rule that allowed an
// ssh call, in the record and the reason.
const ReadOnlyRule = "ssh.read_only"

// remoteReadOnly judges an ssh call to a host the operator declared by its
// text alone: a read when it is a pipeline (or one command) in which every
// program is on the read-only list and called in a read form, with no
// redirection, expansion or substitution, and nothing the bash parser reads
// as a write, a hidden program or a destructive command. It returns the
// programs read, for the reason, or why it is not a read.
func (e *Engine) remoteReadOnly(rr remoteRead) (programs []string, why string) {
	if e.RemoteHosts == nil || !e.RemoteHosts(rr.host) {
		return nil, "the host was not declared by the operator"
	}
	if rr.stdin {
		return nil, "the call writes a stored secret to the command's input"
	}
	programs, why = ClassifyRemoteRead(rr.command)
	if why != "" {
		return nil, why
	}
	// The bash parser's view must agree: nothing hidden, written or run. (Its
	// Reworded is not asked: find's {} sets it, and the words are read above.)
	c := rr.canon
	if !rr.complete || c.Hidden || c.IFSSplit || c.Unparsed() || c.Incomplete() ||
		c.Opaque() != "" || c.ProgramHidden() || len(c.Writes()) > 0 || c.WriteHidden() != "" || c.GitRunsOrWrites() != "" {
		return nil, "the shell parser reads more into it than plain words"
	}
	if what, destructive := c.DestructiveWith(e.GitExtensions); destructive {
		return nil, what
	}
	return programs, ""
}

// ClassifyRemoteRead reads command as the remote classifier does, host
// aside: the programs of a read-only pipeline, or why it is not one.
func ClassifyRemoteRead(command string) (programs []string, why string) {
	stages, why := tools.PipelineWords(command)
	if why != "" {
		return nil, why
	}
	for _, words := range stages {
		if why := readOnlyCall(words, true); why != "" {
			return nil, why
		}
		for _, w := range words[1:] {
			if credentialPath(w) {
				return nil, fmt.Sprintf("%q names a file that commonly holds credentials, so reading it asks, as the read tool's does", w)
			}
		}
		programs = append(programs, words[0])
	}
	return programs, ""
}

// credentialPath reports a word naming a file that commonly holds
// credentials, as the default ask rules for the read tool do, and the
// folders that hold keys. A speed bump, not protection: grep -r over a
// home folder still reads them.
func credentialPath(w string) bool {
	if _, v, ok := strings.Cut(w, "="); ok && strings.HasPrefix(w, "-") {
		w = v
	}
	for _, dir := range []string{".ssh", ".gnupg", ".aws", ".kube", ".docker"} {
		if w == dir || strings.HasSuffix(w, "/"+dir) || strings.HasPrefix(w, dir+"/") || strings.Contains(w, "/"+dir+"/") {
			return true
		}
	}
	base := path.Base(w)
	switch base {
	case ".env", ".netrc", ".pgpass", "shadow", "shadow-", "gshadow", "gshadow-", "credentials", ".git-credentials":
		return true
	}
	return strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") ||
		strings.HasPrefix(base, "id_rsa") || strings.HasPrefix(base, "id_ed25519") || strings.HasPrefix(base, "id_ecdsa") ||
		strings.HasPrefix(base, "id_dsa")
}

// bareProgram is a program named without a path, an assignment or anything
// a shell would expand.
var bareProgram = regexp.MustCompile(`^[a-z][a-z0-9._+-]*$`)

// readOnlyCall reports why words, one simple command, are not a call of a
// program on the read-only list in a read form, or "". findOK lets the
// command be find, whose -exec must run another such call.
func readOnlyCall(words []string, findOK bool) string {
	name := words[0]
	if !bareProgram.MatchString(name) {
		return fmt.Sprintf("%q is not a program named on its own (no path, no capitals)", name)
	}
	if name == "find" && findOK {
		return findReadOnly(words[1:])
	}
	spec, known := knownReads[name]
	if !known {
		return name + " is not on the read-only list"
	}
	if why := reachesOut(name, words[1:]); why != "" {
		return why
	}
	if !readCallClustered(spec, words[1:]) {
		return fmt.Sprintf("%s is called with an option, verb or operand that is not a read (%s)", name, readFormSummary(name, spec))
	}
	return ""
}

// reachesOut says why a read form is not judged a read unasked: it reaches
// beyond the host, so a call could carry data out (openssl s_client, and
// getent, which asks the resolver).
func reachesOut(name string, args []string) string {
	switch {
	case name == "getent":
		return "getent may ask DNS or a directory server, a way off the host"
	case name == "openssl" && len(args) > 0 && strings.TrimLeft(args[0], "-") == "s_client":
		return "openssl s_client connects from the host to another, a way off it"
	}
	return ""
}

// readCallClustered is readCallOK with short options also read in a cluster
// as getopt reads one (-rh is -r -h; -rn5 is -r -n 5), for a program that
// takes them so.
func readCallClustered(spec readSpec, args []string) bool {
	if spec.noGlue || spec.oneDash {
		return readCallOK(spec, args, never)
	}
	split := make([]string, 0, len(args))
	op := spec.opts
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			split = append(split, args[i:]...)
			break
		}
		if i == 0 && spec.verbs != nil {
			op = spec.verbs[w]
		}
		if letters, ok := cluster(op, w); ok {
			split = append(split, letters...)
		} else {
			split = append(split, w)
		}
	}
	return readCallOK(spec, split, never)
}

// cluster splits w, a cluster of short options, as getopt does: each letter
// an option the form allows, the first that takes a value taking the rest
// of the word, if any, as it. ok is false for a word that is not such a
// cluster, which is then judged whole: -n50, or a listed -eo whose o
// takes the next word as getopt reads it, are both read here.
func cluster(op opts, w string) (letters []string, ok bool) {
	if len(w) < 3 || w[0] != '-' || w[1] == '-' {
		return nil, false
	}
	for j := 1; j < len(w); j++ {
		letter := "-" + w[j:j+1]
		switch {
		case op.values[letter]:
			letters = append(letters, letter)
			if j+1 < len(w) {
				letters = append(letters, w[j+1:])
			}
			return letters, true
		case op.flags[letter]:
			letters = append(letters, letter)
		default:
			return nil, false
		}
	}
	return letters, true
}

func never(string) bool { return false }

// findReadOnly reports why find's arguments are not a read, or "": tests and
// actions that only print, and -exec or -execdir of a read-only call.
// -delete, -fprint*, -fls, -ok and anything unknown are not reads.
func findReadOnly(args []string) string {
	i := 0
	// Options before the paths.
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	// The paths: up to the first word that starts an expression.
	for i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" {
		i++
	}
	for ; i < len(args); i++ {
		w := args[i]
		switch {
		case findAlone[w]:
		case findValued[w] || strings.HasPrefix(w, "-newer") && len(w) == len("-newerXY"):
			if i+1 >= len(args) {
				return "find " + w + " needs a value"
			}
			i++
		case w == "-exec" || w == "-execdir":
			end := i + 1
			for end < len(args) && args[end] != ";" && args[end] != "+" {
				end++
			}
			if end == len(args) || end == i+1 {
				return "find " + w + " has no command ended by ; or +"
			}
			if why := readOnlyCall(args[i+1:end], false); why != "" {
				return "find " + w + " runs what is not a read: " + why
			}
			i = end
		default:
			return fmt.Sprintf("find %s is not a read (find reads only with tests, -print, -printf, -ls and -exec of a read)", w)
		}
	}
	return ""
}

// findAlone are find's words that take no value and only test or print.
var findAlone = set("-print", "-print0", "-ls", "-prune", "-quit", "-depth", "-mount", "-xdev", "-noleaf",
	"-follow", "-daystart", "-empty", "-false", "-true", "-readable", "-writable", "-executable",
	"-nouser", "-nogroup", "-o", "-or", "-a", "-and", "-not", "(", ")",
	"-ignore_readdir_race", "-noignore_readdir_race")

// findValued are find's words that take one value and only test or print.
var findValued = set("-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-lname", "-ilname",
	"-regex", "-iregex", "-regextype", "-type", "-xtype", "-size", "-mtime", "-mmin", "-atime", "-amin",
	"-ctime", "-cmin", "-newer", "-anewer", "-cnewer", "-used", "-user", "-group", "-uid", "-gid",
	"-perm", "-links", "-inum", "-samefile", "-maxdepth", "-mindepth", "-fstype", "-printf", "-context")

// readFormSummary names the options and verbs a program's read form takes,
// for an error that says what would be accepted.
func readFormSummary(name string, spec readSpec) string {
	if spec.verbs != nil {
		return "its read verbs are " + strings.Join(sortedKeys(spec.verbs), ", ")
	}
	var parts []string
	if len(spec.opts.flags) > 0 {
		parts = append(parts, "options "+strings.Join(sortedKeys(spec.opts.flags), " "))
	}
	if len(spec.opts.values) > 0 {
		parts = append(parts, "options with a value "+strings.Join(sortedKeys(spec.opts.values), " "))
	}
	if spec.operands == "none" {
		parts = append(parts, "no operand")
	}
	if len(parts) == 0 {
		return name + " takes no options"
	}
	return name + "'s read form takes " + strings.Join(parts, "; ")
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
