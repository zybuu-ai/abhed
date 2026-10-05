package auth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Two stores on one file, as two `abhed user` processes are, each with its
// own in-process lock: every change lands, and one name is created once.
func TestFileUserStoreChangesAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	a, _ := NewFileUserStore(path)
	b, _ := NewFileUserStore(path)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			if err := s.Put(ctx, &User{Username: fmt.Sprintf("u%02d", i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if users, _ := a.List(ctx); len(users) != 20 {
		t.Fatalf("%d of 20 accounts survived", len(users))
	}
	var created, taken int
	var mu sync.Mutex
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			err := s.Create(ctx, &User{Username: "same", Name: fmt.Sprint(i)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrUserExists):
				taken++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if created != 1 || taken != 9 {
		t.Fatalf("created %d, refused %d", created, taken)
	}
}
