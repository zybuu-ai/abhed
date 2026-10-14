// Command abhed-microvm serves the microvm driver over the out-of-process
// driver protocol, on a Unix socket only the client's user can open.
//
// On Linux it runs as root, because Firecracker's jailer needs root to
// chroot each VM and drop to its own user; the Abhed server stays
// unprivileged and talks to it over the socket. Workspace images are built
// as the client's user, so the driver copies only what that user can read.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"syscall"

	"github.com/zybuu-ai/abhed/driver/microvm"
	"github.com/zybuu-ai/abhed/driver/plugin"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "abhed-microvm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("abhed-microvm", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "the driver's settings, a JSON file (required)")
	socket := fs.String("socket", "", "the Unix socket to listen on, in a folder no other user can write (required)")
	uid := fs.Int("client-uid", -1, "the user the Abhed server runs as; it alone may open the socket")
	gid := fs.Int("client-gid", -1, "that user's group")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *socket == "" {
		return errors.New("-config and -socket are required")
	}
	// Out of range, an id would wrap: 4294967296 is uid 0.
	if (*uid != -1 || *gid != -1) && (*uid <= 0 || *gid <= 0 || *uid >= math.MaxUint32 || *gid >= math.MaxUint32) {
		return fmt.Errorf("-client-uid and -client-gid must both be given, each from 1 to %d", uint32(math.MaxUint32-1))
	}
	opts, err := loadOptions(*cfgPath)
	if err != nil {
		return err
	}
	root := os.Geteuid() == 0
	if root && (*uid <= 0 || *gid <= 0) {
		return errors.New("running as root, -client-uid and -client-gid must name the unprivileged user the server runs as")
	}
	if *uid > 0 {
		opts.ImageUser = &microvm.Credential{UID: uint32(*uid), GID: uint32(*gid)}
	}
	d, err := microvm.New(opts)
	if err != nil {
		return err
	}
	// VMs a killed run of this driver left are ended before any is started.
	cleaned, err := d.Recover(context.Background())
	for _, c := range cleaned {
		slog.Info("abhed-microvm: cleaned up after an earlier run", "what", c)
	}
	if err != nil {
		_ = d.Close(context.Background())
		return err
	}
	l, err := plugin.Listen(*socket)
	if err != nil {
		_ = d.Close(context.Background())
		return err
	}
	if root {
		if err := os.Chown(*socket, *uid, *gid); err != nil {
			_ = l.Close()
			_ = d.Close(context.Background())
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serr := plugin.Serve(ctx, l, d)
	// Instances live only as long as the driver that tracks them.
	return errors.Join(serr, d.Close(context.Background()))
}

func loadOptions(path string) (microvm.Options, error) {
	var o microvm.Options
	b, err := os.ReadFile(path)
	if err != nil {
		return o, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}
