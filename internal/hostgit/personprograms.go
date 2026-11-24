package hostgit

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// hostPATH is the PATH host git runs with for a brokered call: the person's
// own, less every folder that is relative (found from where git runs) or
// lies in areas (the workspace and what commands can write), so neither git,
// ssh, a wrapper nor a ProxyCommand finds a program the agent put there.
func hostPATH(areas []string) string {
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		inArea := false
		for _, a := range areas {
			if a != "" && within(dir, a) {
				inArea = true
			}
		}
		if !inArea {
			keep = append(keep, dir)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// personProgramsHeld says why a program the person's own git configuration,
// environment or ssh configuration names for host git to run cannot be
// trusted as theirs: core.sshCommand (or ssh when it names none) and the
// ssh configuration it reads; core.askPass and SSH_ASKPASS; core.gitProxy;
// each credential helper. Each word is judged by programUntrusted, programs
// found on path, the PATH host git runs with. nil when every one is theirs.
func personProgramsHeld(ctx context.Context, cwd string, areas []string, path string) error {
	last := func(key string) string {
		if v := personValues(ctx, cwd, key); len(v) > 0 {
			return v[len(v)-1][1]
		}
		return ""
	}
	type named struct {
		what  string
		words []string
	}
	var progs []named
	sshCmd := last("core.sshcommand")
	if sshCmd == "" {
		sshCmd = "ssh"
	}
	sshWords, ok := shellWords(sshCmd)
	if !ok {
		return fmt.Errorf("the ssh command your git configuration names (%q) could not be read as words", sshCmd)
	}
	progs = append(progs, named{"core.sshCommand", sshWords})
	// Single paths, run as they are.
	for _, k := range []string{"core.askpass", "core.gitproxy"} {
		if v := last(k); v != "" {
			progs = append(progs, named{k, []string{v}})
		}
	}
	if v := os.Getenv("SSH_ASKPASS"); v != "" {
		progs = append(progs, named{"SSH_ASKPASS", []string{v}})
	}
	for _, k := range credentialKeys(ctx, cwd) {
		for _, v := range personValues(ctx, cwd, k) {
			if v[1] == "" {
				continue
			}
			cmd := helperCommand(ctx, cwd, v[1])
			if strings.HasPrefix(v[1], "!") {
				if why := shellSyntax(cmd); why != "" {
					return fmt.Errorf("the program your git configuration names for %s (%q) %s", k, cmd, why)
				}
			}
			words, ok := shellWords(cmd)
			if !ok {
				return fmt.Errorf("the program your git configuration names for %s (%q) could not be read as words", k, cmd)
			}
			progs = append(progs, named{k, words})
			for _, f := range helperDefaults(words) {
				if why := trustFileUntrusted(f, areas); why != "" {
					return fmt.Errorf("the credential helper your git configuration names for %s (%q) keeps credentials in %s, which %s", k, cmd, f, why)
				}
			}
		}
	}
	// ssh's own options and configuration first, for the reason that names them.
	if err := sshConfigHeld(sshWords, areas, path); err != nil {
		return err
	}
	if err := httpTrustHeld(ctx, cwd, areas); err != nil {
		return err
	}
	for _, p := range progs {
		if why := programUntrusted(p.words, areas, path); why != "" {
			return fmt.Errorf("the program your git configuration names for %s (%q) %s", p.what, strings.Join(p.words, " "), why)
		}
	}
	return nil
}

// shellSyntax says why a ! helper, run by the shell, is more than a program
// and its words: a function, variable, substitution, pipe or second command.
func shellSyntax(cmd string) string {
	if strings.ContainsAny(cmd, "$`;|&(){}<>*?[]\n") {
		return "is a shell command with more than a program and its words (a function, variable, substitution, pipe or second command), " +
			"so what it runs is not known; name a helper script instead"
	}
	return ""
}

// shellWords splits s as the shell splits a simple command: on spaces,
// with '...' and "..." quoting and \ escaping; false when a quote is open.
func shellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if in {
		words = append(words, cur.String())
	}
	return words, true
}

// expandHome is p with a leading ~/ made the home folder, as the shell and
// ssh do.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// helperCommand is the command git runs for credential helper value h:
// the shell form's command, a path as it is, or git credential-<name>
// found where git looks for its own commands, then PATH.
func helperCommand(ctx context.Context, cwd, h string) string {
	first := strings.Fields(h)[0]
	switch {
	case strings.HasPrefix(h, "!"):
		return strings.TrimSpace(h[1:])
	case filepath.IsAbs(first), strings.HasPrefix(first, "~/"):
		return h
	}
	// The helper's own words after its name are judged too (store --file …).
	args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), first))
	name := "git-credential-" + first
	if dir, err := gitOut(ctx, cwd, "--exec-path"); err == nil && dir != "" {
		if p := filepath.Join(dir, name); executable(p) {
			name = p
		}
	}
	return strings.TrimSpace(name + " " + args)
}

// helperDefaults are the files git's own cache and store helpers use when
// words name none: the cache's socket (in ~/.git-credential-cache where it
// exists, else the XDG cache folder) and the store's credential files.
func helperDefaults(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	has := func(flag string) bool {
		for _, w := range words[1:] {
			if w == flag || strings.HasPrefix(w, flag+"=") {
				return true
			}
		}
		return false
	}
	home, _ := os.UserHomeDir()
	xdg := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, fallback)
	}
	switch filepath.Base(words[0]) {
	case "git-credential-cache":
		if has("--socket") || home == "" {
			return nil
		}
		if info, err := os.Stat(filepath.Join(home, ".git-credential-cache")); err == nil && info.IsDir() {
			return []string{filepath.Join(home, ".git-credential-cache", "socket")}
		}
		return []string{filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "git", "credential", "socket")}
	case "git-credential-store":
		if has("--file") || home == "" {
			return nil
		}
		return []string{filepath.Join(home, ".git-credentials"), filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "git", "credentials")}
	}
	return nil
}

// shells are programs that run a command line given with -c, which Abhed
// cannot judge.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true}

// programUntrusted says why what words run is not the person's alone, or ""
// when it is (or names nothing that exists, which runs nothing). Every word
// is judged, and the value after = in each (ssh's -o ProxyCommand=...),
// with a leading ~/ made the home folder: a relative path is refused, as it
// would be found from where git runs; an absolute one must lie outside the
// areas and be the person's or root's alone. The program itself is found on
// path, the PATH host git runs with.
func programUntrusted(words []string, areas []string, path string) string {
	if len(words) == 0 {
		return ""
	}
	first := expandHome(words[0])
	if strings.ContainsAny(first, "$`;|&(){}<>*?[]") || strings.HasSuffix(first, "()") {
		return "does not start with a program Abhed can find (a shell function, variable or substitution), so what it runs is not known"
	}
	// The shell runs these lines: a glob or other shell syntax in any word
	// could expand to a path the agent chose.
	for _, w := range words[1:] {
		if strings.ContainsAny(w, "$`;|&(){}<>*?[]") {
			return "has " + w + ", which the shell would expand or treat as more than a word, so what it names is not known; write it in full"
		}
	}
	if shells[filepath.Base(first)] && slices.Contains(words[1:], "-c") {
		return "runs a shell command line (-c), so what it runs is not known; name a script instead"
	}
	prog, why := programPath(first, path)
	if why != "" {
		return why
	}
	if prog != "" {
		if why := pathUntrusted(prog, areas); why != "" {
			return why
		}
	}
	for _, w := range words[1:] {
		v := w
		if _, val, ok := strings.Cut(w, "="); ok {
			v = val // an option's value: -oProxyCommand=...
		}
		v = expandHome(v)
		switch {
		case !strings.Contains(v, "/"), strings.Contains(v, "://"): // not a path; a URL is not one
		case !filepath.IsAbs(v):
			return "names " + v + ", a relative path, which would be found from where git runs"
		default:
			if why := pathUntrusted(v, areas); why != "" {
				return "names " + v + ", which " + why
			}
		}
	}
	return ""
}

// programPath is where the program first names is, found on path for a bare
// name; "" when it is found nowhere (so nothing runs), or why it cannot be
// found as the person's.
func programPath(first, path string) (string, string) {
	switch {
	case strings.Contains(first, "/") && !filepath.IsAbs(first):
		return "", "is a relative path, which git would resolve from where it runs"
	case filepath.IsAbs(first):
		return first, ""
	}
	for _, dir := range filepath.SplitList(path) {
		if p := filepath.Join(dir, first); executable(p) {
			return p, ""
		}
	}
	return "", ""
}

// pathUntrusted says why absolute path p (where it exists) is not the
// person's alone: in one of areas, links followed, or another user's or
// writable by others.
func pathUntrusted(p string, areas []string) string {
	for _, a := range areas {
		if a == "" {
			continue
		}
		for _, q := range []string{p, filepath.Dir(p)} {
			if within(q, a) {
				return "is in " + a + ", which commands can write"
			}
		}
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return untrustedProgram(p)
}

// executable reports a regular file someone may run.
func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// credentialKeys are every credential helper key the configuration names.
func credentialKeys(ctx context.Context, cwd string) []string {
	keys := []string{"credential.helper"}
	out, err := gitOut(ctx, cwd, "config", "--includes", "--name-only", "--get-regexp", `^credential\..*\.helper$`)
	if err != nil {
		return keys
	}
	for _, k := range strings.Fields(out) {
		if k != "" && k != "credential.helper" {
			keys = append(keys, k)
		}
	}
	return keys
}

// sshCommandsRun are the ssh configuration keywords whose value is a
// command ssh runs.
var sshCommandsRun = map[string]bool{"proxycommand": true, "localcommand": true, "knownhostscommand": true}

// sshConfigHeld says why the ssh configuration host git's ssh reads is not
// the person's alone: /etc/ssh/ssh_config, ~/.ssh/config (or the -F file
// the ssh command names), and every file they Include (recursively, globs
// expanded, relative ones from ~/.ssh or /etc/ssh as ssh finds them), each
// checked as a git configuration file is; and each command a ProxyCommand,
// LocalCommand, KnownHostsCommand or Match exec there names, judged as a
// git program is. nil when all is theirs.
func sshConfigHeld(sshWords []string, areas []string, path string) error {
	home, _ := os.UserHomeDir()
	userDir := filepath.Join(home, ".ssh")
	type root struct{ file, rel string }
	roots := []root{{"/etc/ssh/ssh_config", "/etc/ssh"}}
	if f := sshConfigFlag(sshWords); f != "" {
		if why := sshExpanded(f); why != "" {
			return fmt.Errorf("the ssh configuration your ssh command names (-F %s) %s", f, why)
		}
		f = expandHome(f)
		if !filepath.IsAbs(f) {
			return fmt.Errorf("the ssh configuration your ssh command names (-F %s) is a relative path", f)
		}
		roots = append(roots, root{f, userDir}) // -F replaces ~/.ssh/config
	} else if home != "" {
		roots = append(roots, root{filepath.Join(userDir, "config"), userDir})
	}
	for _, o := range sshOptions(sshWords) {
		if why := sshDirectiveUntrusted(o[0], o[1], areas, path); why != "" {
			return fmt.Errorf("your ssh command's option -o %s=%s %s", o[0], o[1], why)
		}
	}
	if v := os.Getenv("SSH_SK_PROVIDER"); v != "" {
		if why := sshDirectiveUntrusted("securitykeyprovider", v, areas, path); why != "" {
			return fmt.Errorf("SSH_SK_PROVIDER in your environment (%s) %s", v, why)
		}
	}
	seen := map[string]bool{}
	for len(roots) > 0 {
		r := roots[0]
		roots = roots[1:]
		f := filepath.Clean(r.file)
		if seen[f] {
			continue
		}
		if len(seen) >= maxConfigFiles {
			return fmt.Errorf("your ssh configuration includes more than %d files, so not all of them were checked", maxConfigFiles)
		}
		seen[f] = true
		for _, a := range areas {
			if a == "" {
				continue
			}
			for _, p := range []string{f, filepath.Dir(f)} {
				if within(p, a) {
					return fmt.Errorf("your ssh configuration file %s is in %s, which commands can write", f, a)
				}
			}
		}
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if why := untrustedFile(f); why != "" {
			return fmt.Errorf("your ssh configuration file %s %s", f, why)
		}
		if err := readSSHConfig(f, r.rel, areas, path, func(m, rel string) { roots = append(roots, root{m, rel}) }); err != nil {
			return err
		}
	}
	return nil
}

// readSSHConfig reads ssh configuration file f: each Include it names goes
// to include, and each command it runs is judged.
func readSSHConfig(f, rel string, areas []string, path string, include func(file, rel string)) error {
	fh, err := os.Open(f) // #nosec G304 -- the person's own ssh configuration, read only
	if err != nil {
		return fmt.Errorf("your ssh configuration file %s could not be read (%v), so what it holds is not known", f, err)
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, rest := sshKeyValue(line)
		switch {
		case key == "include":
			words, ok := shellWords(rest)
			if !ok {
				return fmt.Errorf("your ssh configuration file %s has an Include that could not be read", f)
			}
			for _, w := range words {
				if why := sshExpanded(w); why != "" {
					return fmt.Errorf("your ssh configuration file %s includes %q, which %s", f, w, why)
				}
				w = expandHome(w)
				if !filepath.IsAbs(w) {
					w = filepath.Join(rel, w)
				}
				matches, _ := filepath.Glob(w)
				if len(matches) == 0 {
					matches = []string{w} // where a file could be made
				}
				for _, m := range matches {
					include(m, rel)
				}
			}
		case sshCommandsRun[key] || sshPathKeys[key] || sshTrustKeys[key] || key == "controlpath":
			if why := sshDirectiveUntrusted(key, rest, areas, path); why != "" {
				return fmt.Errorf("your ssh configuration file %s sets %s %q, which %s", f, key, rest, why)
			}
		case key == "match":
			for _, cmd := range matchExecs(rest) {
				if why := sshCommandUntrusted(cmd, areas, path); why != "" {
					return fmt.Errorf("your ssh configuration file %s runs %q (Match exec), which %s", f, cmd, why)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("your ssh configuration file %s could not be read (%v)", f, err)
	}
	return nil
}

// sshPathKeys are the ssh keywords whose value is a file ssh loads or runs
// as a program of its own: a library (PKCS11Provider, SecurityKeyProvider)
// or xauth.
var sshPathKeys = map[string]bool{"pkcs11provider": true, "securitykeyprovider": true, "xauthlocation": true}

// sshTrustKeys are the ssh keywords whose value is a file ssh trusts: the
// host keys it accepts and revokes, and the certificate it presents. One
// the agent could write would let it vouch for a host of its own.
var sshTrustKeys = map[string]bool{"userknownhostsfile": true, "globalknownhostsfile": true, "revokedhostkeys": true, "certificatefile": true,
	"identityfile": true}

// trustFileUntrusted says why a file ssh or git trusts (a known hosts file,
// a certificate, a CA bundle or folder, a key), named as v, is not the
// person's alone: a path ssh or git would expand (write the full path), a
// relative one, one in areas or its folder there (links followed), or one
// another user's, writable by others or unreadable. "" when it is, or
// where none is there, outside every area.
func trustFileUntrusted(v string, areas []string) string {
	if why := sshExpanded(v); why != "" {
		return why
	}
	p := expandHome(v)
	if !filepath.IsAbs(p) {
		return "is not an absolute path, so where it is found is not known; write the full path"
	}
	for _, a := range areas {
		if a == "" {
			continue
		}
		for _, q := range []string{p, filepath.Dir(p)} {
			if within(q, a) {
				return "is in " + a + ", which commands can write, where the agent could vouch for a host of its own"
			}
		}
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	if why := untrustedFile(p); why != "" {
		return why
	}
	if fh, err := os.Open(p); err != nil { // #nosec G304 -- the person's own file, read only
		return "could not be read (" + err.Error() + ")"
	} else {
		_ = fh.Close()
	}
	return ""
}

// httpTrustKeys match the person's git settings naming a file https trusts
// or presents, URL-scoped ones too.
const httpTrustKeys = `^http\..*ssl(cainfo|capath|cert|key)$`

// httpTrustHeld says why a CA bundle or folder, client certificate or key
// the person's git configuration names for https is not theirs alone.
func httpTrustHeld(ctx context.Context, cwd string, areas []string) error {
	out, err := gitOut(ctx, cwd, "config", "--includes", "--name-only", "--get-regexp", httpTrustKeys)
	if err != nil {
		return nil // none set
	}
	seen := map[string]bool{}
	for _, k := range strings.Fields(out) {
		if seen[k] {
			continue
		}
		seen[k] = true
		for _, v := range personValues(ctx, cwd, k) {
			if v[1] == "" {
				continue
			}
			if why := trustFileUntrusted(v[1], areas); why != "" {
				return fmt.Errorf("the file your git configuration names for %s (%s) %s", k, v[1], why)
			}
		}
	}
	return nil
}

// sshExpanded says why an Include or -F value cannot be placed as ssh will
// place it: ssh expands environment variables, % tokens and ~user in it.
func sshExpanded(v string) string {
	switch {
	case strings.ContainsAny(v, "$%"), strings.HasPrefix(v, "~") && !strings.HasPrefix(v, "~/"):
		return "names a path ssh expands (an environment variable, a % token or another user's home), so where it leads is not known; write the full path"
	}
	return ""
}

// sshDirectiveUntrusted judges one ssh setting, from a configuration file
// or an -o option: a command it runs, a library or program it loads, or the
// folder of a ControlPath (a socket the agent could stand in for). "" when
// it is the person's alone, or none of these.
func sshDirectiveUntrusted(key, value string, areas []string, path string) string {
	key = strings.ToLower(key)
	value = strings.TrimSpace(value)
	switch {
	case sshCommandsRun[key]:
		return sshCommandUntrusted(value, areas, path)
	case sshPathKeys[key]:
		words, ok := shellWords(value)
		if !ok || len(words) == 0 {
			return "could not be read"
		}
		v := words[0]
		if strings.EqualFold(v, "none") || strings.EqualFold(v, "internal") {
			return ""
		}
		v = expandHome(v)
		if !filepath.IsAbs(v) {
			return "is not an absolute path, so where ssh finds it is not known"
		}
		return pathUntrusted(v, areas)
	case sshTrustKeys[key]:
		words, ok := shellWords(value)
		if !ok {
			return "could not be read"
		}
		for _, w := range words {
			if strings.EqualFold(w, "none") {
				continue
			}
			if why := trustFileUntrusted(w, areas); why != "" {
				return why
			}
		}
		return ""
	case key == "controlpath":
		words, _ := shellWords(value)
		if len(words) == 0 || strings.EqualFold(words[0], "none") {
			return ""
		}
		v := expandHome(words[0])
		dir := filepath.Dir(v)
		if !filepath.IsAbs(v) || strings.ContainsAny(dir, "%$") {
			return "is a socket whose folder is not known until ssh expands it; name a folder of your own"
		}
		for _, a := range areas {
			if a != "" && within(dir, a) {
				return "is a socket in " + a + ", which commands can write, where the agent could stand in for your ssh"
			}
		}
	}
	return ""
}

// sshOptions are the -o settings of an ssh command, as key and value.
func sshOptions(words []string) [][2]string {
	var out [][2]string
	for i := 0; i < len(words); i++ {
		var opt string
		switch {
		case words[i] == "-o" && i+1 < len(words):
			opt = words[i+1]
			i++
		case strings.HasPrefix(words[i], "-o") && len(words[i]) > 2:
			opt = words[i][2:]
		default:
			continue
		}
		key, value := sshKeyValue(opt)
		out = append(out, [2]string{key, value})
	}
	return out
}

// sshCommandUntrusted judges a command an ssh configuration runs; none is
// a command ssh runs nothing for.
func sshCommandUntrusted(cmd string, areas []string, path string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.EqualFold(cmd, "none") || cmd == "" {
		return ""
	}
	words, ok := shellWords(cmd)
	if !ok {
		return "could not be read as words"
	}
	return programUntrusted(words, areas, path)
}

// sshKeyValue is an ssh configuration line's keyword, lowered, and the rest.
// As ssh reads it: the keyword may be quoted, and is followed by spaces,
// an =, or both.
func sshKeyValue(line string) (string, string) {
	line = strings.TrimSpace(line)
	// The keyword is a word as ssh's argv_split reads it: quotes anywhere in
	// it ("Proxy"Command), \ escapes, ended by unquoted space or =.
	var key strings.Builder
	var quote rune
	escaped := false
	i := 0
	for ; i < len(line); i++ {
		c := rune(line[i])
		switch {
		case escaped:
			key.WriteRune(c)
			escaped = false
		case c == '\\':
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				key.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t' || c == '=':
			goto done
		default:
			key.WriteRune(c)
		}
	}
done:
	rest := strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	return sshCanonical(strings.ToLower(key.String())), rest
}

// sshAliases are OpenSSH's other names for keywords judged here, as its
// readconf.c maps them: an alias, or a deprecated name it still reads.
var sshAliases = map[string]string{
	"smartcarddevice":       "pkcs11provider",
	"globalknownhostsfile2": "globalknownhostsfile",
	"userknownhostsfile2":   "userknownhostsfile",
	"identityfile2":         "identityfile",
}

// sshCanonical is key under the name it is judged by.
func sshCanonical(key string) string {
	if c, ok := sshAliases[key]; ok {
		return c
	}
	return key
}

// matchExecs are the commands a Match line runs, as ssh reads its criteria:
// exec and !exec, each followed by its command or joined to it by =.
func matchExecs(rest string) []string {
	words, _ := shellWords(rest)
	var cmds []string
	for i := 0; i < len(words); i++ {
		w := strings.ToLower(strings.TrimPrefix(words[i], "!"))
		switch {
		case w == "exec" && i+1 < len(words):
			cmds = append(cmds, words[i+1])
			i++
		case strings.HasPrefix(w, "exec="):
			cmds = append(cmds, words[i][strings.Index(words[i], "=")+1:])
		}
	}
	return cmds
}

// sshConfigFlag is the file an ssh command's -F names, "" when none.
func sshConfigFlag(words []string) string {
	for i, w := range words {
		switch {
		case w == "-F" && i+1 < len(words):
			return words[i+1]
		case strings.HasPrefix(w, "-F") && len(w) > 2:
			return w[2:]
		}
	}
	return ""
}
