package server

import (
	"path/filepath"
	"strings"
	"testing"
)

// A multipart filename is client-supplied and untrusted:
// "../../../.ssh/authorized_keys" is a legal one.
func TestSafeUploadNameCannotEscape(t *testing.T) {
	for _, raw := range []string{
		"../../../../etc/passwd",
		"..\\..\\windows\\system32\\config",
		"/etc/shadow",
		"....//....//escape",
		"foo/bar/baz.pdf",
	} {
		got := safeUploadName(raw)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeUploadName(%q) = %q, still contains a path separator", raw, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("safeUploadName(%q) = %q, still contains ..", raw, got)
		}
		if filepath.Base(got) != got {
			t.Errorf("safeUploadName(%q) = %q, is not a bare filename", raw, got)
		}
	}
}

// A name that is only dots or separators must still produce something usable
// rather than an empty path that resolves to the directory itself.
func TestSafeUploadNameHandlesDegenerateInput(t *testing.T) {
	for _, raw := range []string{"", ".", "..", "/", "...", "\x00\x01"} {
		got := safeUploadName(raw)
		if got == "" || strings.HasPrefix(got, ".") || strings.ContainsAny(got, `/\`) {
			t.Errorf("safeUploadName(%q) = %q, not a safe bare name", raw, got)
		}
	}
}

// Two uploads of report.pdf must not overwrite each other: the agent would
// silently read different content than the one that was just attached.
func TestSafeUploadNameIsUnique(t *testing.T) {
	a := safeUploadName("report.pdf")
	b := safeUploadName("report.pdf")
	if a == b {
		t.Errorf("two uploads of the same name collided: %q", a)
	}
	for _, n := range []string{a, b} {
		if filepath.Ext(n) != ".pdf" {
			t.Errorf("%q lost its extension; a person recognises files by it", n)
		}
		if !strings.HasPrefix(n, "report-") {
			t.Errorf("%q is no longer recognisable as report.pdf", n)
		}
	}
}

func TestSafeUploadNameBoundsLength(t *testing.T) {
	long := strings.Repeat("a", 500) + ".pdf"
	got := safeUploadName(long)
	if len(got) > 140 {
		t.Errorf("name is %d chars, want it bounded", len(got))
	}
	if filepath.Ext(got) != ".pdf" {
		t.Errorf("truncation lost the extension: %q", got)
	}
}

func TestIsProbablyBinaryAgreesWithRead(t *testing.T) {
	if !isProbablyBinary([]byte{0x00, 0x01, 0x02}) {
		t.Error("NUL bytes should read as binary")
	}
	if isProbablyBinary([]byte("plain text content")) {
		t.Error("plain text flagged as binary")
	}
}

func TestFirstLinesIsBounded(t *testing.T) {
	got := firstLines(strings.Repeat("line\n", 100), 3)
	if strings.Count(got, "\n") > 2 {
		t.Errorf("returned more than 3 lines: %q", got)
	}
	long := firstLines(strings.Repeat("x", 1000), 3)
	if len(long) > 420 {
		t.Errorf("preview is %d chars, want it bounded", len(long))
	}
}

// TestUploadIsNotWrittenToADeniedPath pins where attachments land.
//
// Uploads were written to <workspace>/.abhed/uploads/<session>. Any deployment
// carrying the ordinary secret-protection rule read(**/.abhed/**) — which
// exists to keep the agent away from Abhed's own config and users.json — then
// refused every file a user attached, and the model reported "denied by rule"
// for a document it had just been handed on purpose.
//
// The deny rule was right; the location was wrong. This test fails if an
// upload is ever put back under a dot-directory, where a blanket rule can
// swallow it.
func TestUploadIsNotWrittenToADeniedPath(t *testing.T) {
	if strings.HasPrefix(uploadDirName, ".") {
		t.Fatalf("uploads go to %q: a dot-directory invites a blanket deny rule "+
			"like read(**/.abhed/**), which is what broke attachments before", uploadDirName)
	}
	if strings.Contains(uploadDirName, "abhed") {
		t.Fatalf("uploads go to %q, inside Abhed's own state directory: that tree "+
			"holds users.json and is legitimately denied to the agent", uploadDirName)
	}
}
