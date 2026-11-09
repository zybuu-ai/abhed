package policy

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ReadOnlyRule names the read-only classifier as the rule that allowed an
// ssh call, in the record and the reason.
const ReadOnlyRule = "ssh.read_only"

// remoteReadOnly judges an ssh call to a host the operator declared by its
// text alone: a read when it is a pipeline (or one command) in which every
// program is on the read-only list and called in a read form, with no
// redirection, expansion or substitution, and nothing the bash parser reads
// as a write, a hidden program or a destructive command. A program that
// prints a file's contents counts only when the operator opted in
// (RemoteContents). It returns the programs read, for the reason, or why it
// is not a read.
func (e *Engine) remoteReadOnly(rr remoteRead) (programs []string, why string) {
	if e.RemoteHosts == nil || !e.RemoteHosts(rr.host) {
		return nil, "the host was not declared by the operator"
	}
	if rr.stdin {
		return nil, "the call writes a stored secret to the command's input"
	}
	programs, why = ClassifyRemoteRead(rr.command, e.RemoteContents)
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
// contents lets a program print the contents of the files it names
// (ssh.read_contents); without it only metadata and the lines piped from it
// are read.
func ClassifyRemoteRead(command string, contents bool) (programs []string, why string) {
	stages, why := tools.PipelineWords(command)
	if why != "" {
		return nil, why
	}
	for i, words := range stages {
		if why := judgeCall(words, i > 0, contents, true); why != "" {
			return nil, why
		}
		programs = append(programs, words[0])
	}
	return programs, ""
}

// bareProgram is a program named without a path, an assignment or anything
// a shell would expand.
var bareProgram = regexp.MustCompile(`^[a-z][a-z0-9._+-]*$`)

// judgeCall reports why words, one simple command, are not a read, or "".
// piped says its input is the pipe from the command before; contents is
// the operator's opt-in to programs that print files; findOK lets it be
// find, whose -exec must run another such call.
func judgeCall(words []string, piped, contents, findOK bool) string {
	name := words[0]
	if !bareProgram.MatchString(name) {
		return fmt.Sprintf("%q is not a program named on its own (no path, no capitals)", name)
	}
	if why := unsafeWords(words[1:]); why != "" {
		return why
	}
	if name == "find" && findOK {
		return findReadOnly(words[1:], contents)
	}
	spec, known := knownReads[name]
	if !known {
		return name + " is not on the read-only list"
	}
	if why := reachesOut(name, words[1:]); why != "" {
		return why
	}
	if name == "apt" || name == "apt-cache" {
		return name + " may rewrite its package cache when run as root, so it is not a read here"
	}
	args := splitClusters(spec, words[1:])
	if !readCallOK(spec, args, never) {
		return fmt.Sprintf("%s is called with an option, verb or operand that is not a read (%s)", name, readFormSummary(name, spec))
	}
	if why := endless(name, args); why != "" {
		return why
	}
	if why := opensDevices(name, args); why != "" {
		return why
	}
	operands := operandsOf(spec, args)
	if readsStdin[name] && len(fileOperands(name, args, operands)) == 0 {
		if !piped {
			return name + " with no file reads the command's input, which never ends; name the file"
		}
		// A filter of the lines piped to it reads no file of its own.
		if stdinFilter(name, args) {
			return ""
		}
	}
	if !printsContents(name, args) {
		return ""
	}
	if !contents {
		return name + " prints the contents of files, which runs unasked only where the operator sets ssh.read_contents"
	}
	for _, w := range words[1:] {
		if m := secretWord(w); m != "" {
			return fmt.Sprintf("%q may name a file that holds credentials (%s), so reading it asks", w, m)
		}
	}
	return ""
}

// printsContents reports a call whose output is the contents of the files
// it reads, or of a container's or unit's environment: not metadata.
func printsContents(name string, args []string) bool {
	switch name {
	case "cat", "head", "tail", "cut", "cmp", "diff", "zcat", "grep", "egrep", "fgrep", "rg", "sort",
		"journalctl", "dmesg":
		return true
	case "openssl":
		return len(args) == 0 || args[0] != "version"
	case "docker", "podman":
		return len(args) > 0 && (args[0] == "logs" || args[0] == "inspect")
	case "systemctl":
		if len(args) > 0 && args[0] == "status" {
			return !statusWithoutLog(args[1:])
		}
		return len(args) > 0 && (args[0] == "cat" || args[0] == "show")
	case "md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum", "b2sum", "cksum":
		// A hash of a short secret gives the secret back to a guess.
		return true
	case "findmnt":
		// The fstab holds mount options, a CIFS password among them.
		return slices.Contains(args, "-s") || slices.Contains(args, "--fstab")
	}
	return false
}

// statusWithoutLog reports a systemctl status call that prints no journal
// lines: -n 0 or --lines=0, and no -o. Any other prints the unit's log.
func statusWithoutLog(args []string) bool {
	zero := false
	for i, a := range args {
		switch {
		case a == "-o" || a == "--output" || strings.HasPrefix(a, "--output="):
			return false
		case a == "-n" || a == "--lines":
			if i+1 >= len(args) || args[i+1] != "0" {
				return false
			}
			zero = true
		case strings.HasPrefix(a, "--lines="):
			if a != "--lines=0" {
				return false
			}
			zero = true
		}
	}
	return zero
}

// readsStdin are the programs that read their input when given no file.
var readsStdin = set("cat", "head", "tail", "cut", "zcat", "grep", "egrep", "fgrep", "rg", "sort", "wc",
	"md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum", "b2sum", "cksum")

// stdinFilter reports a call that, given no file, only filters the lines
// piped to it: no option that names a file to read or a tree to search.
func stdinFilter(name string, args []string) bool {
	switch name {
	case "head", "tail", "sort", "cut", "wc":
		return true
	case "grep", "egrep", "fgrep":
		for _, a := range args {
			if a == "-f" || a == "-r" || a == "-R" || strings.HasPrefix(a, "--file") || strings.HasPrefix(a, "--recursive") ||
				strings.HasPrefix(a, "--dereference-recursive") {
				return false
			}
		}
		return true
	}
	return false
}

// fileOperands are the operands a call reads as files: grep's first operand
// is its pattern unless -e gives one.
func fileOperands(name string, args, operands []string) []string {
	switch name {
	case "grep", "egrep", "fgrep", "rg":
		for _, a := range args {
			if a == "-e" || a == "-f" || strings.HasPrefix(a, "--regexp") || strings.HasPrefix(a, "--file") {
				return operands
			}
		}
		if len(operands) > 0 {
			return operands[1:]
		}
	}
	return operands
}

// endless says why a call would never finish, or "": a form that follows a
// file or repeats, which the operator may declare in ssh.read_commands but
// the classifier does not run unasked.
func endless(name string, args []string) string {
	follow := func(words ...string) string {
		for _, a := range args {
			for _, w := range words {
				if a == w || strings.HasPrefix(w, "--") && strings.HasPrefix(a, w+"=") {
					return name + " " + a + " runs until it is stopped"
				}
			}
		}
		return ""
	}
	switch name {
	case "tail":
		return follow("-f", "-F", "--follow", "--retry")
	case "journalctl":
		return follow("-f", "--follow")
	case "dmesg":
		return follow("-w", "-W", "--follow", "--follow-new")
	case "free":
		return follow("-s", "--seconds", "-c", "--count")
	case "docker", "podman":
		if len(args) > 0 && args[0] == "logs" {
			return follow("-f", "--follow")
		}
	case "vmstat":
		if ops := operandsOf(knownReads[name], args); len(ops) > 0 {
			return "vmstat " + ops[0] + " repeats until it is stopped"
		}
	}
	return ""
}

// opensDevices says why a call would open a device, or "": grep -D read
// reads devices it meets, and file -s opens the device it is named.
func opensDevices(name string, args []string) string {
	for _, a := range args {
		switch {
		case (name == "grep" || name == "egrep" || name == "fgrep") && (a == "-D" || strings.HasPrefix(a, "--devices")):
			return name + " " + a + " may read a device, and opening one can change the machine"
		case name == "file" && (a == "-s" || a == "--special-files"):
			return "file -s opens the devices it is named, and opening one can change the machine"
		case name == "lsusb" && (a == "-v" || a == "--verbose"):
			return "lsusb -v opens the devices under /dev/bus/usb"
		}
	}
	return ""
}

// unsafeWords says why a word of a call reaches what a read must not: a
// device (opening /dev/watchdog arms it; a tape rewinds), /proc and /sys
// (/proc/kmsg is consumed as it is read, /proc/self/root/dev is /dev again),
// an automounted network path (/net/<host> mounts and looks up a name), or
// the command's input (-). Each path is cleaned first, so /./dev and //dev
// are /dev, and a glob that could match such a folder counts too.
func unsafeWords(args []string) string {
	for _, a := range args {
		w := a
		if strings.HasPrefix(w, "-") {
			if _, v, ok := strings.Cut(w, "="); ok {
				w = v
			} else if w == "-" {
				return "- reads the command's input, which never ends"
			} else {
				continue
			}
		}
		if w == ".." || strings.HasPrefix(w, "../") || strings.Contains(w, "/../") || strings.HasSuffix(w, "/..") {
			return fmt.Sprintf("%q climbs with .., so where it leads is not read; write the path from /", w)
		}
		if !strings.HasPrefix(w, "/") {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(path.Clean(w), "/"), "/")
		for _, top := range []string{"dev", "proc", "sys", "net", "misc"} {
			if segmentMatches(first, top) {
				return fmt.Sprintf("%q is or may be under /%s, where opening a file can change the machine, never end or reach another", w, top)
			}
		}
	}
	return ""
}

// segmentMatches reports a path segment, perhaps a glob, that is or could
// expand to name.
func segmentMatches(segment, name string) bool {
	if segment == name {
		return true
	}
	ok, err := path.Match(segment, name)
	return ok || err != nil
}

// remoteSecretNames are the names, beside secretfiles.Names, of files on a
// host that hold credentials or another process's secrets.
var remoteSecretNames = []string{"shadow", "shadow-", "gshadow", "gshadow-", "ssh_host_*_key", "environ", "kcore",
	".my.cnf", ".bash_history", ".zsh_history", ".mysql_history", ".psql_history", "admin.conf", "kubeconfig",
	"*.kubeconfig", "master.key", "secrets.yml", "secrets.yaml"}

// secretSamples are a name each secret pattern matches, for testing a glob
// segment against: * becomes x, and a trailing * is also dropped.
var secretSamples = func() []string {
	var out []string
	for _, p := range append(slices.Clone(secretfiles.Names), remoteSecretNames...) {
		out = append(out, strings.ReplaceAll(p, "*", "x"))
		if strings.HasSuffix(p, "*") {
			out = append(out, strings.ReplaceAll(strings.TrimSuffix(p, "*"), "*", "x"))
		}
	}
	return out
}()

// secretWord returns the secret name a word may name, "" when none: each
// segment is matched as secretfiles matches it, and a segment that is a
// glob is matched against a sample of every secret name, so /etc/shado?
// and .ss?/id_* are caught as /etc/shadow and .ssh/id_rsa are.
func secretWord(w string) string {
	if strings.HasPrefix(w, "-") {
		_, v, ok := strings.Cut(w, "=")
		if !ok {
			return ""
		}
		w = v
	}
	if m := secretfiles.Match(w); m != "" {
		return m
	}
	for _, seg := range strings.Split(strings.ToLower(w), "/") {
		if seg == "" {
			continue
		}
		for _, p := range remoteSecretNames {
			if ok, _ := path.Match(p, seg); ok {
				return p
			}
		}
		if !strings.ContainsAny(seg, "*?[") {
			continue
		}
		for _, sample := range secretSamples {
			if ok, err := path.Match(seg, sample); ok || err != nil {
				return sample
			}
		}
	}
	return ""
}

// operandsOf are the words of a read form's call that are not options or
// their values, its verb aside.
func operandsOf(spec readSpec, args []string) []string {
	op := spec.opts
	if spec.verbs != nil && len(args) > 0 {
		op, args = spec.verbs[args[0]], args[1:]
	}
	var out []string
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			return append(out, args[i+1:]...)
		}
		if strings.HasPrefix(w, "-") && w != "-" {
			_, value := spec.optionOK(op, w, args[i+1:], never)
			i += value
			continue
		}
		out = append(out, w)
	}
	return out
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

// splitClusters reads short options in a cluster as getopt reads one (-rh
// is -r -h; -rn5 is -r -n 5), for a program that takes them so.
func splitClusters(spec readSpec, args []string) []string {
	if spec.noGlue || spec.oneDash {
		return args
	}
	split := make([]string, 0, len(args))
	op := spec.opts
	for i := 0; i < len(args); i++ {
		w := args[i]
		if w == "--" {
			return append(split, args[i:]...)
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
	return split
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

// metadataExec are the programs find may -exec on any file it meets: they
// read a file's name and inode, except file, which reads a regular file's
// first bytes to name its type (and never opens a device unless -s, which
// is refused). /proc and /sys are refused as roots, so none meets them.
var metadataExec = set("ls", "stat", "du", "basename", "dirname", "realpath", "readlink", "file")

// findReadOnly reports why find's arguments are not a read, or "": tests and
// actions that only print, and -exec or -execdir of a read-only call.
// -delete, -fprint*, -fls, -ok and anything unknown are not reads. A program
// that opens what it is given runs only under a -type f find cannot skip,
// so it never meets a device.
func findReadOnly(args []string, contents bool) string {
	i := 0
	// Options before the paths.
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	// The paths: up to the first word that starts an expression.
	for i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" {
		i++
	}
	var execs [][]string
	branches, regular, regex := false, false, false
	for ; i < len(args); i++ {
		w := args[i]
		switch {
		case findAlone[w]:
			branches = branches || findBranch[w]
		case findValued[w] || strings.HasPrefix(w, "-newer") && len(w) == len("-newerXY"):
			if i+1 >= len(args) {
				return "find " + w + " needs a value"
			}
			regular = regular || w == "-type" && args[i+1] == "f"
			regex = regex || w == "-regex" || w == "-iregex"
			i++
		case w == "-exec" || w == "-execdir":
			// GNU find ends -exec … + only at a + right after {}; any other + is an argument.
			end := i + 1
			for end < len(args) && args[end] != ";" && (args[end] != "+" || args[end-1] != "{}") {
				end++
			}
			if end == len(args) || end == i+1 {
				return "find " + w + " has no command ended by ; or {} +"
			}
			execs = append(execs, args[i+1:end])
			i = end
		default:
			return fmt.Sprintf("find %s is not a read (find reads only with tests, -print, -printf, -ls and -exec of a read)", w)
		}
	}
	for _, cmd := range execs {
		if why := judgeCall(cmd, false, contents, false); why != "" {
			return "find -exec runs what is not a read: " + why
		}
		if metadataExec[cmd[0]] {
			continue
		}
		if !regular || branches {
			return "find -exec of " + cmd[0] + ", which opens what it is given, needs -type f with no -o, -not or ( ), so it never opens a device"
		}
		if printsContents(cmd[0], cmd[1:]) {
			if regex {
				return "find -regex picks files by a pattern no secret check can read; use -name"
			}
			for _, w := range args {
				if m := secretWord(w); m != "" {
					return fmt.Sprintf("find %q may name a file that holds credentials (%s), so reading it asks", w, m)
				}
			}
		}
	}
	return ""
}

// findAlone are find's words that take no value and only test or print.
var findAlone = set("-print", "-print0", "-ls", "-prune", "-quit", "-depth", "-mount", "-xdev", "-noleaf",
	"-follow", "-daystart", "-empty", "-false", "-true", "-readable", "-writable", "-executable",
	"-nouser", "-nogroup", "-o", "-or", "-a", "-and", "-not", "(", ")",
	"-ignore_readdir_race", "-noignore_readdir_race")

// findBranch are the words that let a find expression skip a test.
var findBranch = set("-o", "-or", "-not", "(", ")")

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
