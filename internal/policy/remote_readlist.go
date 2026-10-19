package policy

import (
	"strings"
)

// opts are the options a read form may use: flags as written, and values,
// options whose value is the next word or follows = or, for a short one,
// is glued on (-n50). Any other option is refused, a prefix of a long
// option getopt would take or a cluster of short ones included.
type opts struct {
	flags, values map[string]bool
}

// o builds opts from space-separated flags and value options.
func o(flags, values string) opts {
	return opts{flags: set(strings.Fields(flags)...), values: set(strings.Fields(values)...)}
}

// readSpec is how a program on the read-only list may be called as a read
// through a wildcard entry in ssh.read_commands. The checks run on the
// entry at load and on every call the entry's pattern matches.
type readSpec struct {
	// plain programs take any arguments: none of their options writes a
	// file or runs a program.
	plain bool
	// verbs, when set, are the words that must come first after the program,
	// before any option, each with the options it may take after it.
	verbs map[string]opts
	// opts are the options of a form with no verbs.
	opts opts
	// objects and actions are ip's: after its options, an object and then
	// nothing or an action that shows.
	objects, actions map[string]bool
	// words refuses a call with any of these words anywhere.
	words map[string]bool
	// operands: "" any, "none" none, "+" each must start with + (date).
	operands string
	// oneDash reads --opt as -opt, as openssl does.
	oneDash bool
	// badValue, when set, refuses an option's value (rpm's query formats).
	badValue func(string) bool
	// noGlue is set for a program that does not read a short option's value
	// glued on (ip and openssl take -force and -engine as words of their own).
	noGlue bool
	// strict refuses $ outside single quotes and an unquoted glob, either of
	// which could turn a word into an option on the host.
	strict bool
}

// verbsWith gives each verb the same options.
func verbsWith(op opts, verbs ...string) map[string]opts {
	m := make(map[string]opts, len(verbs))
	for _, v := range verbs {
		m[v] = op
	}
	return m
}

// knownReads are the programs a wildcard entry may name, each with how it
// may be called. Any other program is declared by its exact command.
var knownReads = func() map[string]readSpec {
	m := map[string]readSpec{}
	for _, p := range []string{
		"cat", "head", "tail", "ls", "stat", "df", "free", "uptime", "uname", "id", "whoami",
		"groups", "ps", "pgrep", "netstat", "lsblk", "grep", "egrep", "fgrep",
		"lscpu", "lsmem", "lsmod", "lspci", "nproc", "w", "who", "getent", "vmstat", "basename", "dirname",
		"realpath", "readlink", "cut", "cmp", "diff", "zcat",
	} {
		m[p] = readSpec{plain: true}
	}
	// Programs that show a file's size, type or sum, not its lines, may not
	// take an option that would print another file's lines: file's
	// -m (magic file), wc's and du's --files0-from, the sums' -c (check
	// list), findmnt's -F (table file) and lsusb's -D (device file).
	m["file"] = readSpec{opts: o("-b --brief -L --dereference -h --no-dereference -i --mime --mime-type --mime-encoding "+
		"-k --keep-going -s --special-files -E -N --no-pad -0 --print0 -r --raw", "-F --separator -e --exclude"), strict: true}
	m["wc"] = readSpec{opts: o("-c --bytes -m --chars -l --lines -L --max-line-length -w --words", "--total"), strict: true}
	m["du"] = readSpec{opts: o("-a --all -b --bytes -c --total -h --human-readable -k -m -s --summarize -x --one-file-system "+
		"-L --dereference -P --no-dereference -l --count-links -S --separate-dirs -0 --null --apparent-size --si --inodes "+
		"-sh -hs -ah -ha -sch -csh -hc -ch -sk -sm -xsh",
		"-d --max-depth -t --threshold --time-style -B --block-size --exclude"), strict: true}
	sums := readSpec{opts: o("-b --binary -t --text --tag -z --zero", ""), strict: true}
	for _, p := range []string{"md5sum", "sha1sum", "sha224sum", "sha256sum", "sha384sum", "sha512sum", "b2sum"} {
		m[p] = sums
	}
	m["cksum"] = readSpec{opts: o("--base64 --raw --tag --untagged -z --zero", "-a --algorithm -l --length"), strict: true}
	m["findmnt"] = readSpec{opts: o("-l --list -n --noheadings -J --json -r --raw -a --ascii -D --df -b --bytes "+
		"-u --notruncate -s --fstab -m --mtab -k --kernel -A --all -f --first-only -v --nofsroot",
		"-o --output -t --types -T --target -S --source -O --options -M --mountpoint"), strict: true}
	m["lsusb"] = readSpec{opts: o("-v --verbose -t --tree", "-s -d"), strict: true}
	m["rg"] = readSpec{opts: o("-n --line-number -N --no-line-number -i --ignore-case -S --smart-case -s --case-sensitive "+
		"-w --word-regexp -x --line-regexp -v --invert-match -l --files-with-matches --files-without-match -c --count "+
		"--count-matches -F --fixed-strings -H --with-filename --no-filename --no-heading --heading --hidden --no-ignore "+
		"-u -uu -L --follow --files -o --only-matching --json --column --vimgrep -U --multiline -P --pcre2 -q --quiet "+
		"--sort-files --stats --trim -a --text",
		"-e --regexp -g --glob --iglob -t --type -T --type-not -A --after-context -B --before-context -C --context "+
			"-m --max-count -d --max-depth --max-filesize -E --encoding --sort --sortr"), strict: true}
	m["sort"] = readSpec{opts: o("-n --numeric-sort -r --reverse -u --unique -h --human-numeric-sort -V --version-sort "+
		"-f --ignore-case -b --ignore-leading-blanks -M --month-sort -g --general-numeric-sort -s --stable "+
		"-z --zero-terminated -c --check -C -d --dictionary-order -i --ignore-nonprinting -R --random-sort",
		"-k --key -t --field-separator -S --buffer-size --parallel"), strict: true}
	m["date"] = readSpec{opts: o("-u --utc --universal -R --rfc-email -I -Idate -Ihours -Iminutes -Iseconds -Ins "+
		"--iso-8601 --iso-8601=date --iso-8601=hours --iso-8601=minutes --iso-8601=seconds --iso-8601=ns "+
		"--rfc-3339=date --rfc-3339=seconds --rfc-3339=ns", "-d --date -r --reference"), operands: "+", strict: true}
	m["hostname"] = readSpec{opts: o("-f -s -i -I -d -a -A --fqdn --long --short --ip-address --all-ip-addresses "+
		"--domain --alias --all-fqdns", ""), operands: "none", strict: true}
	m["dmesg"] = readSpec{opts: o("-T --ctime -H --human -k --kernel -u --userspace -x --decode -t --notime -w --follow "+
		"-W --follow-new -P --nopager -r --raw -J --json -e --reltime -d --show-delta -S --syslog -L --color",
		"-l --level -f --facility --since --until --time-format"), strict: true}
	m["journalctl"] = readSpec{opts: o("-b --boot -k --dmesg --no-pager -r --reverse -x --catalog -f --follow -q --quiet "+
		"-a --all --no-hostname --utc -N --fields --system --user --list-boots --disk-usage --header --show-cursor "+
		"--no-full -l --full -m --merge",
		"-u --unit --user-unit -n --lines -p --priority -S --since -U --until -o --output --output-fields "+
			"-g --grep -t --identifier -F --field -D --directory --file --cursor --after-cursor -T --exclude-identifier"), strict: true}
	m["systemctl"] = readSpec{verbs: verbsWith(o("--no-pager -l --full -a --all --value --no-legend --plain -q --quiet "+
		"--user --system -r --recursive --reverse --before --after",
		"-n --lines -o --output -p --property -t --type --state"),
		"status", "show", "cat", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files",
		"list-timers", "list-sockets", "list-dependencies"), strict: true}
	m["hostnamectl"] = readSpec{verbs: verbsWith(o("--static --transient --pretty", ""), "status"), strict: true}
	m["timedatectl"] = readSpec{verbs: verbsWith(o("-a --all --value", "-p --property"), "status", "show"), strict: true}
	// ip takes any unique prefix of an option or a word (-ba is -batch, a is
	// add), so only these options, as written, and whole words are read; its
	// options come before the object.
	m["ip"] = readSpec{opts: o("-4 -6 -0 -o -oneline -br -brief -s -stats -statistics -d -details -j -json -p -pretty "+
		"-c -color -h -human -iec -r -resolve", "-f -family"),
		objects: set("addr", "address", "a", "link", "l", "route", "r", "neigh", "neighbour", "n", "rule", "maddr"),
		actions: set("show", "list", "ls", "lst", "get"), words: set("netns", "exec", "vrf"), noGlue: true, strict: true}
	m["openssl"] = readSpec{verbs: map[string]opts{
		"x509": o("-noout -text -enddate -startdate -dates -subject -issuer -serial -fingerprint -sha1 -sha256 -md5 "+
			"-purpose -hash -subject_hash -issuer_hash -email -ocsp_uri -pubkey -modulus -ext_all",
			"-in -inform -checkend -nameopt -ext -certopt -dateopt"),
		"s_client": o("-showcerts -verify_return_error -brief -tls1_2 -tls1_3 -no_ign_eof -quiet -status -crlf",
			"-connect -servername -CAfile -CApath -verify -alpn -starttls -proxy -host -port"),
		"verify":  o("-verbose -show_chain -crl_check -crl_check_all -partial_chain -x509_strict", "-CAfile -CApath -untrusted -purpose -CRLfile"),
		"version": o("-a -v -b -o -f -p -d", ""),
	}, oneDash: true, noGlue: true, strict: true}
	docker := map[string]opts{
		"ps":      o("-a --all -q --quiet --no-trunc -l --latest -s --size", "-n --last -f --filter --format"),
		"images":  o("-a --all -q --quiet --no-trunc --digests", "-f --filter --format"),
		"inspect": o("-s --size", "-f --format --type"),
		"logs":    o("-f --follow -t --timestamps --details", "-n --tail --since --until"),
		"version": o("", "-f --format"),
		"info":    o("", "-f --format"),
	}
	m["docker"] = readSpec{verbs: docker, strict: true}
	m["podman"] = readSpec{verbs: docker, strict: true}
	m["apt"] = readSpec{verbs: map[string]opts{
		"list": o("--installed --upgradable -a --all-versions", ""), "show": o("-a --all-versions", ""),
		"policy": o("", ""), "search": o("--names-only --full", ""),
	}, strict: true}
	m["apt-cache"] = readSpec{verbs: verbsWith(o("--names-only --full --installed --recurse -i --important", ""),
		"policy", "show", "search", "depends", "rdepends"), strict: true}
	m["dpkg"] = readSpec{opts: o("-l --list -s --status -L --listfiles -S --search -p --print-avail --get-selections", ""), strict: true}
	m["dpkg-query"] = readSpec{opts: o("-l --list -W --show -s --status -L --listfiles -S --search", "-f --showformat"), strict: true}
	m["rpm"] = readSpec{opts: o("-q -qa -qi -ql -qf -qc -qd -qR -qai --query --all --info --list --file --requires "+
		"--provides --whatprovides --whatrequires --changelog --last", "--queryformat --qf"), strict: true,
		// A query format's :expand and %( run macros, and so programs.
		badValue: func(v string) bool { return strings.Contains(v, ":expand") || strings.Contains(v, "%(") }}
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

// optionOK judges one option word of a read form, and says how many of
// the words after it are its value; skip marks a word of an entry that holds
// a wildcard.
func (s readSpec) optionOK(op opts, w string, next []string, skip func(string) bool) (ok bool, value int) {
	if s.oneDash && strings.HasPrefix(w, "--") {
		w = w[1:]
	}
	if skip(w) {
		// A wildcard may stand only for a value written after =.
		name, _, eq := strings.Cut(w, "=")
		return eq && op.values[name] && !strings.ContainsAny(name, "*?"), 0
	}
	if op.flags[w] {
		return true, 0
	}
	bad := func(v string) bool { return s.badValue != nil && s.badValue(v) }
	if op.values[w] {
		// The next word is its value, unless it is an option, which is judged as one.
		if len(next) > 0 && (skip(next[0]) || !strings.HasPrefix(next[0], "-")) {
			return !bad(next[0]), 1
		}
		return true, 0
	}
	if name, v, eq := strings.Cut(w, "="); eq && strings.HasPrefix(w, "--") {
		return op.values[name] && !bad(v), 0
	}
	// A short value option with its value glued on, as -n50.
	if !s.noGlue && len(w) > 2 && !strings.HasPrefix(w, "--") && op.values[w[:2]] {
		return !bad(w[2:]), 0
	}
	return false, 0
}

// readCallOK reports whether args, the words after the program, are a read
// by spec; skip marks a word of an entry that holds a wildcard.
func readCallOK(spec readSpec, args []string, skip func(string) bool) bool {
	if spec.plain {
		return true
	}
	op := spec.opts
	if spec.verbs != nil {
		// The verb is the first word, with no option before it to take it as a value.
		if len(args) == 0 || skip(args[0]) {
			return false
		}
		vo, ok := spec.verbs[args[0]]
		if !ok {
			return false
		}
		op, args = vo, args[1:]
	}
	ended := false
	var operands []string
	for i := 0; i < len(args); i++ {
		w := args[i]
		if spec.words[w] || spec.oneDash && strings.HasPrefix(w, "--") && spec.words[w[1:]] {
			return false
		}
		if !ended && w == "--" {
			ended = true
			continue
		}
		if !ended && strings.HasPrefix(w, "-") && w != "-" {
			// ip's options come before its object; one after it is not read.
			if spec.objects != nil && len(operands) > 0 {
				return false
			}
			ok, value := spec.optionOK(op, w, args[i+1:], skip)
			if !ok {
				return false
			}
			i += value
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
		for _, w := range operands {
			if !skip(w) && !strings.HasPrefix(w, "+") {
				return false
			}
		}
	}
	if spec.objects != nil {
		if len(operands) == 0 || skip(operands[0]) || !spec.objects[operands[0]] {
			return false
		}
		if len(operands) > 1 && !skip(operands[1]) && !spec.actions[operands[1]] {
			return false
		}
	}
	return true
}
