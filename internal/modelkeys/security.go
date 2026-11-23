package modelkeys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Service is the Keychain service Abhed's model keys are stored under; the
// account is the key's name, such as ANTHROPIC_API_KEY.
const Service = "Abhed model key"

// Security is the Keychain through macOS's security program. The value is
// written to its standard input in interactive mode, never in its arguments,
// so no other process sees it in a process listing.
type Security struct {
	// Program is the security program; Keychain the keychain file, "" for
	// the default search list.
	Program  string
	Keychain string
	// Timeout bounds one call; an unlock prompt waits for the person.
	Timeout time.Duration
}

// notFound is security's exit status when there is no such item.
const notFound = 44

func (s Security) run(stdin string, args ...string) (string, int, error) {
	t := s.Timeout
	if t <= 0 {
		t = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), t)
	defer cancel()
	if s.Keychain != "" && len(args) > 0 {
		args = append(args, s.Keychain)
	}
	cmd := exec.CommandContext(ctx, s.Program, args...) // #nosec G204 -- the system's security program, with fixed verbs
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitCode(), fmt.Errorf("%s", firstLine(errb.String(), ee.Error()))
	}
	return out.String(), 0, err
}

func firstLine(s, or string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return or
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Get reads the item's password.
func (s Security) Get(name string) (string, bool, error) {
	out, code, err := s.run("", "find-generic-password", "-s", Service, "-a", name, "-w")
	switch {
	case code == notFound:
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return strings.TrimRight(out, "\r\n"), true, nil
}

// Set adds the item, or updates it. The command goes on standard input.
func (s Security) Set(name, value string) error {
	if strings.ContainsAny(value, "\"\\' \t\r\n") || strings.ContainsAny(name, "\"\\' \t\r\n") {
		return errors.New("the value holds a character the Keychain command cannot take")
	}
	line := fmt.Sprintf("add-generic-password -U -s \"%s\" -a \"%s\" -l \"%s\" -w \"%s\"", Service, name, Service+" "+name, value)
	if s.Keychain != "" {
		line += " \"" + s.Keychain + "\""
	}
	if _, _, err := s.run(line+"\n", "-i"); err != nil {
		return err
	}
	// Interactive mode exits 0 whatever the command did, so the item is read back.
	got, ok, err := s.Get(name)
	switch {
	case err != nil:
		return err
	case !ok || got != value:
		return errors.New("the Keychain did not keep the key")
	}
	return nil
}

// Delete removes the item.
func (s Security) Delete(name string) (bool, error) {
	_, code, err := s.run("", "delete-generic-password", "-s", Service, "-a", name)
	switch {
	case code == notFound:
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}
