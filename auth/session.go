package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync/atomic"
	"time"
)

// browserSession is what a cookie resolves to, for any provider that holds
// sessions in memory. Shared here rather than defined beside one provider so
// the providers can live in different packages without either owning it.
type browserSession struct {
	Identity *Identity
	Created  time.Time
	Expires  time.Time
	// mustChange confines the session to changing its password.
	mustChange atomic.Bool
	lastSeen   atomic.Int64 // unix nanoseconds
}

// SessionInfo describes one browser session for an administrator. ID is a
// digest of the cookie value, never the value itself.
type SessionInfo struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	Subject    string    `json:"subject"`
	Email      string    `json:"email,omitempty"`
	Name       string    `json:"name,omitempty"`
	Created    time.Time `json:"created"`
	LastSeen   time.Time `json:"last_seen"`
	Expires    time.Time `json:"expires"`
	MustChange bool      `json:"must_change_password,omitempty"`
}

// sessionDigest names a session without revealing the cookie that holds it.
func sessionDigest(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(sum[:8])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A failing CSPRNG is not something to paper over with a weaker source.
		panic("auth: system random source unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
