package testdb

import "testing"

func TestCheckRefusesTheDefaultPortSocketsAndOverrides(t *testing.T) {
	t.Setenv("PGPORT", "")
	t.Setenv("PGHOST", "")
	for _, dsn := range []string{
		"postgres://u:p@127.0.0.1:5432/db",
		"postgres://u:p@127.0.0.1/db", // the driver's default port
		"postgres://u:p@[::1]:5432/db",
		"postgres://u:p@127.0.0.1:44444/db?port=5432",
		"postgres://u:p@127.0.0.1:44444/db?host=127.0.0.1&port=5432",
		"postgres://u:p@127.0.0.1:44444,127.0.0.1:5432/db",
		"postgres://u:p@127.0.0.1:44444/db?host=/tmp",
		"host=/var/run/postgresql port=44444",
		"host=127.0.0.1 port=5432",
		"postgres:///db",
		"not a dsn ://",
	} {
		if Check(dsn) == "" {
			t.Errorf("%s was taken", dsn)
		}
	}
	t.Setenv("PGPORT", "5432")
	if Check("postgres://u:p@127.0.0.1/db") == "" {
		t.Error("PGPORT=5432 was taken")
	}
	t.Setenv("PGPORT", "")
	if why := Check("postgres://u:p@127.0.0.1:40101/db?sslmode=disable"); why != "" {
		t.Errorf("a port of its own was refused: %s", why)
	}
}
