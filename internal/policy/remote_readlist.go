package policy

import (
	"strings"
)

// readSpec is how a program on the read-only list may be called as a read
// through a wildcard entry in ssh.read_commands. The checks run on the
// entry at load and on every call the entry's pattern matches.
type readSpec struct {
	// verbs, when set, are the words the first operand may be (systemctl
	// status); the entry must write one out.
	verbs map[string]bool
	// objects and actions are ip's: the first operand must be an object and
	// the second, when there is one, an action that shows.
	objects, actions map[string]bool
	// deny are options a call may not use, long ones also by any prefix
	// getopt would accept, short ones also inside a cluster.
	deny []string
	// allow, when set, are the only options a call may use, as written.
	allow map[string]bool
	// operands: "" any, "none" none, "+" each must start with + (date).
	operands string
	// values are options whose next word is their value, not an operand.
	values map[string]bool
	// words, when set, refuses a call with any of these words anywhere.
	words map[string]bool
	// strict refuses $ outside single quotes and an unquoted glob, either of
	// which could turn a word into an option on the host.
	strict bool
}

// knownReads are the programs a wildcard entry may name, each with how it
// may be called. Any other program is declared by its exact command.
var knownReads = func() map[string]readSpec {
	m := map[string]readSpec{}
	for _, p := range []string{
		"cat", "head", "tail", "wc", "ls", "stat", "file", "df", "du", "free", "uptime", "uname", "id", "whoami",
		"groups", "ps", "pgrep", "netstat", "lsblk", "findmnt", "grep", "egrep", "fgrep", "md5sum", "sha1sum",
		"sha224sum", "sha256sum", "sha384sum", "sha512sum", "b2sum", "cksum", "lscpu", "lsmem", "lsmod",
		"lspci", "lsusb", "nproc", "w", "who", "getent", "vmstat", "basename", "dirname", "realpath",
		"readlink", "cut", "cmp", "diff", "lsof", "zcat",
	} {
		m[p] = readSpec{}
	}
	m["file"] = readSpec{deny: []string{"--compile", "-C"}, strict: true}
	m["rg"] = readSpec{deny: []string{"--pre", "--pre-glob", "--search-zip", "-z"}, strict: true}
	m["sort"] = readSpec{deny: []string{"--output", "-o", "--compress-program", "--temporary-directory", "-T"}, strict: true}
	m["date"] = readSpec{deny: []string{"--set", "-s", "--file", "-f"}, operands: "+",
		values: set("-d", "--date", "-r", "--reference"), strict: true}
	m["hostname"] = readSpec{allow: set("-f", "-s", "-i", "-I", "-d", "-a", "-A", "--fqdn", "--long", "--short",
		"--ip-address", "--all-ip-addresses", "--domain", "--alias", "--all-fqdns"), operands: "none", strict: true}
	m["dmesg"] = readSpec{deny: []string{"--clear", "-C", "--read-clear", "-c", "--console-off", "-D",
		"--console-on", "-E", "--console-level", "-n"}, strict: true}
	m["journalctl"] = readSpec{deny: []string{"--vacuum-size", "--vacuum-time", "--vacuum-files", "--rotate", "--flush",
		"--sync", "--relinquish-var", "--smart-relinquish-var", "--setup-keys", "--update-catalog"}, strict: true}
	m["systemctl"] = readSpec{verbs: set("status", "show", "cat", "is-active", "is-enabled", "is-failed",
		"list-units", "list-unit-files", "list-timers", "list-sockets", "list-dependencies"), strict: true}
	m["hostnamectl"] = readSpec{verbs: set("status"), strict: true}
	m["timedatectl"] = readSpec{verbs: set("status", "show"), strict: true}
	// ip takes any unique prefix of an option or a word (-ba is -batch, a is
	// add), so only these options, as written, and whole words are read.
	m["ip"] = readSpec{objects: set("addr", "address", "a", "link", "l", "route", "r", "neigh", "neighbour", "n", "rule", "maddr"),
		actions: set("show", "list", "ls", "lst", "get"), words: set("netns", "exec", "vrf"),
		allow: set("-4", "-6", "-0", "-o", "-oneline", "-br", "-brief", "-s", "-stats", "-statistics", "-d", "-details",
			"-j", "-json", "-p", "-pretty", "-c", "-color", "-h", "-human", "-iec", "-r", "-resolve", "-f", "-family"),
		values: set("-f", "-family"), strict: true}
	m["openssl"] = readSpec{verbs: set("x509", "s_client", "verify", "version"),
		deny: []string{"-out", "-keyout", "-engine", "-provider", "-provider-path", "-config", "-sess_out",
			"-keylogfile", "-msgfile", "-writerand", "-signkey"}, words: set("-out", "-keyout", "-engine", "-provider",
			"-provider-path", "-config", "-sess_out", "-keylogfile", "-msgfile", "-writerand", "-signkey"), strict: true}
	m["docker"] = readSpec{verbs: set("ps", "images", "inspect", "logs", "version", "info"), strict: true}
	m["podman"] = m["docker"]
	m["apt"] = readSpec{verbs: set("list", "show", "policy", "search"), strict: true}
	m["apt-cache"] = readSpec{verbs: set("policy", "show", "search", "depends", "rdepends"), strict: true}
	m["dpkg"] = readSpec{allow: set("-l", "--list", "-s", "--status", "-L", "--listfiles", "-S", "--search",
		"-p", "--print-avail", "--get-selections"), strict: true}
	m["dpkg-query"] = readSpec{allow: set("-l", "-W", "-s", "-L", "-S", "-f", "--show", "--list", "--status",
		"--listfiles", "--search", "--showformat"), values: set("-f", "--showformat"), strict: true}
	m["rpm"] = readSpec{allow: set("-q", "-qa", "-qi", "-ql", "-qf", "-qc", "-qd", "-qR", "-qai", "--query", "--all",
		"--info", "--list", "--file", "--requires", "--provides", "--whatprovides", "--whatrequires",
		"--changelog", "--last", "--queryformat", "--qf"), values: set("--queryformat", "--qf"), strict: true}
	return m
}()

// shellWords splits a simple command into its words as the shell would,
// quotes undone, and says whether a $ appears outside single quotes or an
// unquoted glob character does.
func shellWords(s string) (words []string, dollar, glob bool) {
	var b strings.Builder
	in := false
	var quote byte
	flush := func() {
		if in {
			words = append(words, b.String())
			b.Reset()
			in = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s):
				i++
				b.WriteByte(s[i])
			default:
				dollar = dollar || c == '$'
				b.WriteByte(c)
			}
		case c == ' ' || c == '\t':
			flush()
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
			in = true
		default:
			dollar = dollar || c == '$'
			glob = glob || c == '*' || c == '?' || c == '['
			b.WriteByte(c)
			in = true
		}
	}
	flush()
	return words, dollar, glob
}

// denied reports an option word that names one of deny: as written, with a
// value after =, as a prefix of a long one getopt would take, or a short
// one inside a cluster such as -cT.
func denied(word string, deny []string) bool {
	if strings.HasPrefix(word, "--") {
		name, _, _ := strings.Cut(word, "=")
		for _, d := range deny {
			if strings.HasPrefix(d, "--") && len(name) > 2 && strings.HasPrefix(d, name) {
				return true
			}
			// A single-dash long option, as openssl's, may be spelt with two.
			if !strings.HasPrefix(d, "--") && len(d) > 2 && name == "-"+d {
				return true
			}
		}
		return false
	}
	for _, d := range deny {
		if strings.HasPrefix(d, "--") {
			continue
		}
		if word == d || strings.HasPrefix(word, d+"=") {
			return true
		}
		// A single-letter option, alone or inside a cluster.
		if len(d) == 2 && len(word) > 1 && !strings.HasPrefix(word, "--") && strings.ContainsRune(word[1:], rune(d[1])) {
			return true
		}
	}
	return false
}

// readCallOK reports whether args, the words after the program, are a read
// by spec; skip says a word of the entry holds a wildcard and is not judged.
func readCallOK(spec readSpec, args []string, skip func(string) bool) bool {
	ended := false
	var operands []string
	for i := 0; i < len(args); i++ {
		w := args[i]
		// An option with a wildcard is judged by what is written before it.
		if skip(w) && !ended && strings.HasPrefix(w, "-") {
			if pre := w[:strings.IndexAny(w, "*?")]; pre != "-" && pre != "--" && denied(strings.TrimSuffix(pre, "="), spec.deny) {
				return false
			}
			continue // the calls it matches are judged as they come
		}
		if skip(w) {
			operands = append(operands, w)
			continue
		}
		if spec.words[w] || strings.HasPrefix(w, "--") && spec.words["-"+strings.TrimLeft(w, "-")] {
			return false
		}
		if !ended && w == "--" {
			ended = true
			continue
		}
		if !ended && strings.HasPrefix(w, "-") && w != "-" {
			if spec.allow != nil && !spec.allow[w] {
				return false
			}
			if denied(w, spec.deny) {
				return false
			}
			if spec.values[w] {
				i++ // its value
			}
			continue
		}
		operands = append(operands, w)
	}
	switch spec.operands {
	case "none":
		if len(operands) > 0 {
			return false
		}
	case "+":
		for _, o := range operands {
			if !skip(o) && !strings.HasPrefix(o, "+") {
				return false
			}
		}
	}
	first := func(n int) (string, bool) {
		if len(operands) > n {
			return operands[n], true
		}
		return "", false
	}
	if spec.verbs != nil {
		v, ok := first(0)
		if !ok || skip(v) || !spec.verbs[v] {
			return false
		}
	}
	if spec.objects != nil {
		o, ok := first(0)
		if !ok || skip(o) || !spec.objects[o] {
			return false
		}
		if a, ok := first(1); ok && !skip(a) && !spec.actions[a] {
			return false
		}
	}
	return true
}
