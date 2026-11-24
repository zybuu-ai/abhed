package clitest

import (
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"

	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/identity"
)

// TestPassword is the password of the account a run is signed in as.
const TestPassword = "tester password"

// signInTester makes the account "tester" in osHome and remembers its
// sign-in, so a run starts signed in, as a person's machine does after its
// first start.
func signInTester(osHome string) error {
	s, err := identity.OpenAt(filepath.Join(osHome, home.DirName, home.AccountsDir))
	if err != nil {
		return err
	}
	s = s.WithCost(bcrypt.MinCost)
	e, _, err := s.Create("tester", TestPassword, "", "")
	if err != nil {
		return err
	}
	return s.Remember(e.ID)
}

// PersonDir is the folder of the account a run is signed in as, under
// osHome: where its record, history, memory and secrets are kept.
func PersonDir(osHome string) string {
	found, _ := filepath.Glob(filepath.Join(osHome, home.DirName, home.AccountsDir, "*", ""))
	for _, f := range found {
		if fi, err := os.Stat(f); err == nil && fi.IsDir() {
			return f
		}
	}
	return filepath.Join(osHome, home.DirName)
}
