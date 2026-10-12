package index

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// VectorCache keeps embeddings between builds, keyed by a chunk's content,
// so a session start embeds only what changed since the last build. A vector
// only ranks a chunk; what a search returns is always read from the files.
type VectorCache interface {
	Get(key string) ([]float32, bool)
	Put(key string, v []float32)
}

// vectorKey is the cache key of a chunk's text.
func vectorKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// FileVectorCache is a VectorCache in one file, read at open and written by
// Save with only the vectors the last build used, so it holds one
// workspace's current chunks and no more.
type FileVectorCache struct {
	path string
	mu   sync.Mutex
	old  map[string][]float32
	used map[string][]float32
}

// OpenFileVectorCache reads the cache at path; a missing or unreadable file
// starts it empty.
func OpenFileVectorCache(path string) *FileVectorCache {
	c := &FileVectorCache{path: path, old: map[string][]float32{}, used: map[string][]float32{}}
	f, err := os.Open(path) // #nosec G304 -- a file under Abhed's own state folder
	if err != nil {
		return c
	}
	defer f.Close()
	var m map[string][]float32
	if gob.NewDecoder(f).Decode(&m) == nil {
		c.old = m
	}
	return c
}

func (c *FileVectorCache) Get(key string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.used[key]
	if !ok {
		if v, ok = c.old[key]; ok {
			c.used[key] = v
		}
	}
	return v, ok
}

func (c *FileVectorCache) Put(key string, v []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used[key] = v
}

// Save writes the vectors this build used, replacing the file whole so a
// reader never sees half of it.
func (c *FileVectorCache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(c.path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("index cache %s is not a regular file", c.path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".vectors-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := gob.NewEncoder(tmp).Encode(c.used); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

// WithVectorCache makes Build take vectors from c and embed only the rest.
func (ix *Index) WithVectorCache(c VectorCache) *Index {
	ix.vcache = c
	return ix
}
