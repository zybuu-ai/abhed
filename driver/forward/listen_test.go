package forward_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
	"github.com/zybuu-ai/abhed/driver/forward"
)

// Connections arriving while the listener closes: each is either served or
// refused, Done closes only after the accept loop and every connection have
// ended, and go test -race finds no Add racing the final Wait.
func TestListenerCloseWhileAcceptingStress(t *testing.T) {
	f := drivertest.New("fake", driver.CapForward)
	ctx := context.Background()
	if _, err := f.Create(ctx, driver.Spec{ID: "sbx_1"}); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		l, err := forward.Listen(ctx, f, "sbx_1", 80, forward.ListenOptions{Record: func(string, map[string]any) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
				if err == nil {
					_ = c.Close()
				}
			}()
		}
		_ = l.Close()
		wg.Wait()
		select {
		case <-l.Done():
		case <-time.After(10 * time.Second):
			t.Fatal("the listener never finished closing")
		}
	}
}
