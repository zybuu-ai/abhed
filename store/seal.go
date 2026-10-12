package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Erasure by key. Events are append-only, so a delete cannot remove what a
// session said; instead its payloads and checkpoint pre-images are sealed
// with a key of the session's own (session_keys), and a delete removes the
// key. What stays in the table is ciphertext nobody can open.

// sealedKey is the one field of a sealed payload.
const sealedKey = "abhed.sealed"

// erasedPayload stands in for a payload whose key is gone.
var erasedPayload = json.RawMessage(`{"erased":true}`)

// sealedBlob marks a sealed checkpoint pre-image.
var sealedBlob = []byte("abhed.sealed.v1:")

// sealer returns the session's cipher for a write, making its key when the
// session has none and is not deleted. Nil with no error means the session
// has no key: written before keys, or erased. Reads never use this cache: see
// readKeys.
func (p *Postgres) sealer(ctx context.Context, sessionID string) (cipher.AEAD, error) {
	if a, ok := p.keys.Load(sessionID); ok {
		return a.(cipher.AEAD), nil
	}
	key, err := p.sessionKey(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if key == nil {
		fresh := make([]byte, 32)
		if _, err := rand.Read(fresh); err != nil {
			return nil, fmt.Errorf("make the key of session %s: %w", sessionID, err)
		}
		if _, err := p.pool.Exec(ctx, `
			INSERT INTO session_keys (session_id, tenant_id, key)
			SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND deleted_at IS NULL)
			ON CONFLICT (session_id) DO NOTHING`, sessionID, p.tenant, fresh); err != nil {
			return nil, fmt.Errorf("store the key of session %s: %w", sessionID, err)
		}
		if key, err = p.sessionKey(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	if key == nil {
		return nil, nil
	}
	a, err := newAEAD(sessionID, key)
	if err != nil {
		return nil, err
	}
	p.keys.Store(sessionID, a)
	return a, nil
}

func newAEAD(sessionID string, key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("the key of session %s: %w", sessionID, err)
	}
	return cipher.NewGCM(block)
}

func (p *Postgres) sessionKey(ctx context.Context, sessionID string) ([]byte, error) {
	return p.queryKey(ctx, `SELECT key FROM session_keys WHERE session_id = $1`, sessionID)
}

func (p *Postgres) queryKey(ctx context.Context, q, sessionID string) ([]byte, error) {
	var key []byte
	err := p.pool.QueryRow(ctx, q, sessionID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the key of session %s: %w", sessionID, err)
	}
	return key, nil
}

// readKeys opens the payloads of one read. Each session's key is read from the
// database once per read, never from the cache, so a session another process
// erased opens as erased here too; a deleted session's key counts as gone.
type readKeys struct {
	p   *Postgres
	got map[string]cipher.AEAD
}

func (p *Postgres) readKeys() *readKeys { return &readKeys{p: p, got: map[string]cipher.AEAD{}} }

func (r *readKeys) key(ctx context.Context, sessionID string) (cipher.AEAD, error) {
	if a, ok := r.got[sessionID]; ok {
		return a, nil
	}
	key, err := r.p.queryKey(ctx, `
		SELECT k.key FROM session_keys k JOIN sessions s ON s.id = k.session_id
		WHERE k.session_id = $1 AND s.deleted_at IS NULL`, sessionID)
	if err != nil {
		return nil, err
	}
	var a cipher.AEAD
	if key != nil {
		if a, err = newAEAD(sessionID, key); err != nil {
			return nil, err
		}
	}
	r.got[sessionID] = a
	return a, nil
}

// aad binds a sealed payload to its place, so one cannot be moved to another step.
func aad(sessionID string, seq int64) []byte {
	return []byte(sessionID + "\x00" + strconv.FormatInt(seq, 10))
}

// sealPayload is payload as it is stored for the session's key.
func sealPayload(a cipher.AEAD, sessionID string, seq int64, payload []byte) ([]byte, error) {
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := a.Seal(nonce, nonce, payload, aad(sessionID, seq))
	return json.Marshal(map[string]string{sealedKey: base64.StdEncoding.EncodeToString(out)})
}

// openPayload is a stored payload as it was written: unsealed with the
// session's key, or the erased marker when the key is gone or does not open it.
func (r *readKeys) openPayload(ctx context.Context, sessionID string, seq int64, stored []byte) ([]byte, error) {
	if !bytes.Contains(stored, []byte(sealedKey)) {
		return stored, nil
	}
	var m map[string]string
	if json.Unmarshal(stored, &m) != nil || len(m) != 1 || m[sealedKey] == "" {
		return stored, nil //nolint:nilerr // not a sealed payload: one written before keys, kept as it is
	}
	a, err := r.key(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(m[sealedKey])
	if a == nil || err != nil || len(raw) < a.NonceSize() {
		return erasedPayload, nil //nolint:nilerr // no key, or none that opens it: erased, not a failed read
	}
	plain, err := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], aad(sessionID, seq))
	if err != nil {
		return erasedPayload, nil //nolint:nilerr // as above
	}
	return plain, nil
}

// sealBlob seals a checkpoint pre-image; nil (no file before) stays nil.
func sealBlob(a cipher.AEAD, sessionID string, seq int64, b []byte) ([]byte, error) {
	if b == nil || a == nil {
		return b, nil
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(append(append([]byte{}, sealedBlob...), nonce...), nonce, b, aad(sessionID, seq)), nil
}
