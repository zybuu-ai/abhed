package driver_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
)

func TestOutputLimitIsSharedAndTotal(t *testing.T) {
	var a, b bytes.Buffer
	calls := 0
	l := driver.NewOutputLimit(10)
	l.OnExceed = func() { calls++ }
	wa, wb := l.Wrap(&a), l.Wrap(&b)
	if n, err := wa.Write([]byte("123456")); n != 6 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := wb.Write([]byte("abcdef")); n != 4 || !errors.Is(err, driver.ErrOutputLimit) {
		t.Fatalf("crossing write: %d %v", n, err)
	}
	if n, err := wa.Write([]byte("z")); n != 0 || !errors.Is(err, driver.ErrOutputLimit) {
		t.Fatalf("after the limit: %d %v", n, err)
	}
	if a.Len()+b.Len() != 10 || !l.Exceeded() || calls != 1 {
		t.Fatalf("wrote %d, exceeded %v, OnExceed ran %d times", a.Len()+b.Len(), l.Exceeded(), calls)
	}
	if l.Wrap(nil) != nil {
		t.Fatal("a nil writer was wrapped")
	}
}
