package local

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBlobDamaged is a blob whose content no longer has the hash it is filed under.
var ErrBlobDamaged = errors.New("the checkpoint's content does not match its hash")

// blobStore keeps checkpoint pre-images by the sha256 of their content,
// under blobs/sha256/<first two hex digits>/<hash>, owner-only.
type blobStore struct{ dir string }

func (b *blobStore) path(sha string) string { return filepath.Join(b.dir, sha[:2], sha) }

// Put stores data and returns its sha256. Content already stored is not
// written again.
func (b *blobStore) Put(data []byte) (string, error) {
	sha := hashBytes(data)
	p := b.path(sha)
	if _, err := os.Stat(p); err == nil {
		return sha, nil
	}
	if err := privateDir(filepath.Dir(p)); err != nil {
		return "", err
	}
	if err := writeAtomic(p, data); err != nil {
		return "", fmt.Errorf("store checkpoint: %w", err)
	}
	return sha, nil
}

// Get returns the content stored as sha, checking its hash on the way out.
func (b *blobStore) Get(sha string) ([]byte, error) {
	if !isHash(sha) {
		return nil, fmt.Errorf("checkpoint %q: %w", sha, ErrNotFound)
	}
	data, err := os.ReadFile(b.path(sha)) // #nosec G304 -- a path built from a checked hash
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checkpoint %s: %w", sha, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if hashBytes(data) != sha {
		return nil, fmt.Errorf("checkpoint %s: %w", sha, ErrBlobDamaged)
	}
	return data, nil
}

// remove deletes a blob; one already gone is not an error.
func (b *blobStore) remove(sha string) error {
	if !isHash(sha) {
		return nil
	}
	if err := os.Remove(b.path(sha)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
