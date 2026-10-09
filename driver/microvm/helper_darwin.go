package microvm

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// codesign is Apple's tool, by absolute path so PATH cannot replace it.
var codesign = "/usr/bin/codesign"

func probeHelper(path, team string) error {
	if path == "" {
		return ErrNoHelper
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w (looked for it at %s: %w)", ErrNoHelper, path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be an executable file writable only by its owner", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := verifyHelper(ctx, path, team, fi); err != nil {
		return err
	}
	return runHelperCheck(ctx, path)
}

// verified remembers helpers whose signature was checked, by the file's
// identity and times, so a replaced or changed file is checked again.
var verified sync.Map

func fileStamp(fi os.FileInfo, team string) string {
	st, _ := fi.Sys().(*syscall.Stat_t)
	if st == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d.%d:%d.%d:%s", st.Dev, st.Ino, st.Size,
		st.Mtimespec.Sec, st.Mtimespec.Nsec, st.Ctimespec.Sec, st.Ctimespec.Nsec, team)
}

// helperRequirement is the code requirement the helper must satisfy: its
// identifier, and with a team, a Developer ID certificate of that team.
func helperRequirement(team string) string {
	r := fmt.Sprintf("identifier %q", HelperIdentifier)
	if team != "" {
		r += fmt.Sprintf(" and anchor apple generic and certificate leaf[subject.OU] = %q", team)
	}
	return r
}

// verifyHelper refuses a helper that is unsigned, modified since it was
// signed, signed as another program or by another team, without the
// virtualization entitlement, or without the hardened runtime.
func verifyHelper(ctx context.Context, path, team string, fi os.FileInfo) error {
	stamp := fileStamp(fi, team)
	if v, ok := verified.Load(path); ok && stamp != "" && v.(string) == stamp {
		return nil
	}
	who := "signed as " + HelperIdentifier
	if team != "" {
		who += " by Apple Developer team " + team
	}
	if out, err := exec.CommandContext(ctx, codesign, "--verify", "--strict", "-R="+helperRequirement(team), path).CombinedOutput(); err != nil {
		return fmt.Errorf("the helper %s is not %s: %s", path, who, codesignMessage(out, err))
	}
	var info bytes.Buffer
	cmd := exec.CommandContext(ctx, codesign, "-d", "--verbose=2", path)
	cmd.Stderr = &info
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reading the helper's signature: %s", codesignMessage(info.Bytes(), err))
	}
	if !hardenedRuntime(info.String()) {
		return fmt.Errorf("the helper %s is not signed with the hardened runtime (codesign -o runtime)", path)
	}
	ents, err := exec.CommandContext(ctx, codesign, "-d", "--entitlements", "-", "--xml", path).Output()
	if err != nil {
		return fmt.Errorf("reading the helper's entitlements: %w", err)
	}
	if ok, err := plistTrue(ents, "com.apple.security.virtualization"); err != nil || !ok {
		return fmt.Errorf("the helper %s is not signed with the com.apple.security.virtualization entitlement", path)
	}
	if stamp != "" {
		verified.Store(path, stamp)
	}
	return nil
}

func codesignMessage(out []byte, err error) string {
	if m := strings.TrimSpace(string(out)); m != "" {
		return m
	}
	return err.Error()
}

var cdFlags = regexp.MustCompile(`(?m)^CodeDirectory .*\bflags=0x([0-9a-f]+)`)

// hardenedRuntime reads the runtime flag (0x10000) from codesign -d output.
func hardenedRuntime(info string) bool {
	m := cdFlags.FindStringSubmatch(info)
	if m == nil {
		return false
	}
	f, err := strconv.ParseUint(m[1], 16, 32)
	return err == nil && f&0x10000 != 0
}

// plistTrue reports whether the top-level dictionary of an XML property
// list maps key to true.
func plistTrue(doc []byte, key string) (bool, error) {
	d := xml.NewDecoder(bytes.NewReader(doc))
	depth, inDict, lastKey, readingKey := 0, false, "", false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2 && t.Name.Local == "dict":
				inDict = true
			case inDict && depth == 3 && t.Name.Local == "key":
				readingKey, lastKey = true, ""
			case inDict && depth == 3 && lastKey == key:
				return t.Name.Local == "true", nil
			case inDict && depth == 3:
				lastKey = ""
			}
		case xml.CharData:
			if readingKey {
				lastKey += string(t)
			}
		case xml.EndElement:
			readingKey = false
			depth--
		}
	}
}
