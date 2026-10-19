package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// A secret's value is never shown, and neither is a plain hash of it: a
// short password is guessed from its sha256 offline. A fingerprint is an
// HMAC-SHA256 under a key made at random when the process starts and never
// written anywhere, cut to 12 hex digits. Two fingerprints compare only while
// this process runs; one recorded is worthless to anyone guessing the value.

var (
	fpOnce sync.Once
	fpKey  [32]byte
)

func fingerprintKey() []byte {
	fpOnce.Do(func() {
		if _, err := rand.Read(fpKey[:]); err != nil {
			panic(fmt.Sprintf("secrets: no randomness for the fingerprint key: %v", err))
		}
	})
	return fpKey[:]
}

// Fingerprint is v's keyed fingerprint, 12 hex digits.
func Fingerprint(v []byte) string {
	m := hmac.New(sha256.New, fingerprintKey())
	m.Write(v)
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// Withheld describes a secret value by its length and fingerprint, never
// the value or a plain hash of it.
func Withheld(v []byte) string {
	return fmt.Sprintf("[withheld: %d bytes, fingerprint %s, comparable only while this Abhed runs]", len(v), Fingerprint(v))
}
