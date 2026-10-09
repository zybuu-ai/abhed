package forward

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/drivertest"
)

// A connection accepted as the listener closes is counted before Done can
// close: Done waits for the accept loop, so no Add follows the last Wait.
func TestDoneWaitsForAnAcceptInFlight(t *testing.T) {
	f := drivertest.New("fake", driver.CapForward)
	ctx := context.Background()
	if _, err := f.Create(ctx, driver.Spec{ID: "sbx_1"}); err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	afterAccept = func() { close(inside); <-release }
	t.Cleanup(func() { afterAccept = func() {} })
	l, err := Listen(ctx, f, "sbx_1", 80, ListenOptions{Record: func(string, map[string]any) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-inside
	_ = l.Close()
	select {
	case <-l.Done():
		t.Fatal("Done closed while a connection was being accepted")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the listener never finished closing")
	}
}
