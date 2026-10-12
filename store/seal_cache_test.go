package store

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"testing"
)

// The write-cipher cache keeps at most its bound, however many sessions a
// long-running store writes.
func TestKeyCacheIsBounded(t *testing.T) {
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	c := keyCache{max: 8}
	for i := range 100 {
		c.store(fmt.Sprintf("s%d", i), a)
	}
	if n := c.len(); n != 8 {
		t.Fatalf("cache holds %d ciphers; want 8", n)
	}
	if _, ok := c.load("s99"); !ok {
		t.Fatal("the latest cipher was let go")
	}
	c.drop("s99")
	if _, ok := c.load("s99"); ok {
		t.Fatal("a dropped cipher is still cached")
	}
	var zero keyCache
	for i := range keyCacheMax + 10 {
		zero.store(fmt.Sprintf("s%d", i), a)
	}
	if n := zero.len(); n != keyCacheMax {
		t.Fatalf("default cache holds %d; want %d", n, keyCacheMax)
	}
}
