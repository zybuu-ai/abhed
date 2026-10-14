package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imageByDigest is an OCI image reference by digest, as an engine pulls it.
var imageByDigest = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// containerEngines are the programs whose image argument a digest may pin:
// any other program could take such an argument and run something else.
var containerEngines = map[string]bool{"docker": true, "podman": true, "nerdctl": true}

// runFlags are run's options that take no value; any other takes the next
// word, so a flag missing here only fails the pin.
var runFlags = map[string]bool{"--interactive": true, "--tty": true, "--rm": true, "--detach": true, "--init": true,
	"--read-only": true, "--privileged": true, "--publish-all": true, "--no-healthcheck": true, "--quiet": true,
	"--oom-kill-disable": true, "--disable-content-trust": true}

// runShort are the one-letter run options that take no value.
const runShort = "itdPq"

// runImage is the image an engine's run (or container run) names: the first
// word past its options and their values; "" for anything else.
func runImage(args []string) string {
	i := 0
	skip := func() bool { // past one option, and its value when it takes one
		a := args[i]
		i++
		switch {
		case strings.Contains(a, "="), runFlags[a]:
		case !strings.HasPrefix(a, "--") && len(a) > 1 && strings.Trim(a[1:], runShort) == "":
		default:
			i++
		}
		return i <= len(args)
	}
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "--" {
		if !skip() {
			return ""
		}
	}
	if i < len(args) && args[i] == "container" {
		i++
	}
	if i >= len(args) || args[i] != "run" {
		return ""
	}
	for i++; i < len(args) && strings.HasPrefix(args[i], "-"); {
		if args[i] == "--" {
			i++
			break
		}
		if !skip() {
			return ""
		}
	}
	if i < len(args) {
		return args[i]
	}
	return ""
}

// engineName is command's program name, without a folder or .exe.
func engineName(command string) string {
	return strings.TrimSuffix(filepath.Base(filepath.ToSlash(command)), ".exe")
}

// ErrDigestMismatch refuses a server whose program is not the one its
// digest pins.
var ErrDigestMismatch = errors.New("the server's program does not match its digest")

// pinnedCommand checks a server's digest and returns the command to start.
// An OCI image named by that digest in the arguments is pinned by the engine
// that pulls it; otherwise the program file the command resolves to is
// hashed, and its resolved path is what starts, so what runs is what was
// checked. A URL server cannot be checked and is refused with a digest.
func pinnedCommand(cfg ServerConfig, env []string) (string, error) {
	if cfg.Digest == "" {
		return cfg.Command, nil
	}
	want := strings.ToLower(strings.TrimSpace(cfg.Digest))
	if !digestForm.MatchString(want) {
		return "", fmt.Errorf("digest %q is not sha256:<64 hex digits>", cfg.Digest)
	}
	if cfg.URL != "" {
		return "", errors.New("digest pins a program Abhed starts; a URL server's cannot be checked, so remove digest or run the server over stdio")
	}
	if containerEngines[engineName(cfg.Command)] {
		if a := runImage(cfg.Args); imageByDigest.MatchString(strings.ToLower(a)) && strings.HasSuffix(strings.ToLower(a), "@"+want) {
			return cfg.Command, nil
		}
	}
	path, err := resolveProgram(cfg.Command, env)
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	got, err := fileDigest(path)
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	if got != want {
		return "", fmt.Errorf("%w: %s is %s, the configuration pins %s", ErrDigestMismatch, path, got, want)
	}
	return path, nil
}

// resolveProgram finds command as the server's own PATH would, links
// followed.
func resolveProgram(command string, env []string) (string, error) {
	path := command
	if !strings.ContainsRune(command, os.PathSeparator) && !strings.ContainsRune(command, '/') {
		search := os.Getenv("PATH")
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "PATH="); ok {
				search = v
			}
		}
		path = ""
		for _, dir := range filepath.SplitList(search) {
			if dir == "" || !filepath.IsAbs(dir) {
				continue // a relative PATH entry depends on where the server starts
			}
			cand := filepath.Join(dir, command)
			if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				path = cand
				break
			}
		}
		if path == "" {
			return "", fmt.Errorf("%s is not found on the server's PATH", command)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
