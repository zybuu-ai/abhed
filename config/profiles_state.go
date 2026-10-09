package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ProfilesEnv overrides where `abhed login` keeps its profiles.
const ProfilesEnv = "ABHED_PROFILES_FILE"

// ProfilesPath is $ABHED_PROFILES_FILE, else ~/.abhed/profiles.json.
func ProfilesPath() (string, error) {
	if p := os.Getenv(ProfilesEnv); p != "" {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "profiles.json"), nil
}

// ProfileFiles are what `abhed login` keeps that commands must not reach:
// the profiles file, which holds sign-in tokens, and the client
// certificate, key and CA each profile names, which `login -cert`, `-key`
// and `-ca` saved as absolute paths. A file that cannot be read adds none.
func ProfileFiles() []string {
	path, err := ProfilesPath()
	if err != nil {
		return nil
	}
	out := []string{path}
	b, err := os.ReadFile(path) // #nosec G304 -- the person's own profiles file
	if err != nil {
		return out
	}
	var f struct {
		Profiles       map[string]profileFiles `json:"profiles"`
		PendingLogouts []profileFiles          `json:"pending_logouts"`
	}
	if json.Unmarshal(b, &f) != nil {
		return out
	}
	add := func(p profileFiles) {
		for _, x := range []string{p.CAFile, p.CertFile, p.KeyFile} {
			if filepath.IsAbs(x) {
				out = append(out, filepath.Clean(x))
			}
		}
	}
	for _, p := range f.Profiles {
		add(p)
	}
	for _, p := range f.PendingLogouts {
		add(p)
	}
	return out
}

// profileFiles are the file settings of one saved profile.
type profileFiles struct {
	CAFile   string `json:"ca_file"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}
