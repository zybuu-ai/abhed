// Package builtin builds the driver registry a configuration describes: the
// process driver always, and the microvm driver when the managed
// configuration names an abhed-microvm socket. Nothing else changes with
// that setting: the process driver stays the default.
package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/microvm"
	"github.com/zybuu-ai/abhed/driver/plugin"
	"github.com/zybuu-ai/abhed/driver/process"
)

// Default is the driver a sandbox gets when it names none.
const Default = process.Name

// dialTimeout bounds reaching the microvm driver.
const dialTimeout = 5 * time.Second

// Entry says how one configured driver stands, for doctor.
type Entry struct {
	Name   string
	Detail string
	// Err is why the driver is registered but refuses every sandbox.
	Err error
}

// Options are the operator's settings for the built-in drivers.
type Options struct {
	Process process.Options
}

// Registry registers the drivers cfg configures. A microvm driver that
// cannot be reached is still registered under its name, refusing every
// sandbox with the reason, so choosing it fails closed rather than finding
// no driver or another one.
func Registry(ctx context.Context, cfg config.Config) (*driver.Registry, []Entry, error) {
	return RegistryWith(ctx, cfg, Options{})
}

// RegistryWith is Registry with the operator's options.
func RegistryWith(ctx context.Context, cfg config.Config, opts Options) (*driver.Registry, []Entry, error) {
	r := driver.NewRegistry()
	if err := r.Register(process.NewWithOptions(cfg, opts.Process)); err != nil {
		return nil, nil, err
	}
	entries := []Entry{{Name: process.Name, Detail: "the configured sandbox tier"}}
	sock := cfg.Sandbox.MicroVMSocket
	if sock == "" {
		return r, entries, nil
	}
	var d driver.Driver
	var err error
	if !filepath.IsAbs(sock) {
		err = fmt.Errorf("sandbox.microvm_socket %q is not an absolute path", sock)
	} else {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		d, err = plugin.Dial(dctx, microvm.Name, sock)
		cancel()
	}
	e := Entry{Name: microvm.Name, Detail: "abhed-microvm at " + sock}
	if err != nil {
		e.Err = err
		d = unavailable{name: microvm.Name, why: fmt.Sprintf("the microvm driver at %s cannot be reached: %v", sock, err)}
	}
	if err := r.Register(d); err != nil {
		return nil, nil, err
	}
	return r, append(entries, e), nil
}

// unavailable is a configured driver that could not be reached.
type unavailable struct{ name, why string }

func (u unavailable) Info() driver.Info { return driver.Info{Name: u.name, Version: "unavailable"} }

func (u unavailable) err() error { return driver.Errorf(driver.ErrUnavailable, "%s", u.why) }

func (u unavailable) Create(context.Context, driver.Spec) (driver.Status, error) {
	return driver.Status{}, u.err()
}

func (u unavailable) Start(context.Context, string) (driver.Status, error) {
	return driver.Status{}, u.err()
}

func (u unavailable) Stop(context.Context, string, time.Duration) (driver.Status, error) {
	return driver.Status{}, u.err()
}

func (u unavailable) Delete(context.Context, string) error { return u.err() }

func (u unavailable) Status(context.Context, string) (driver.Status, error) {
	return driver.Status{}, u.err()
}

func (u unavailable) Exec(context.Context, string, driver.ExecRequest) (driver.ExecResult, error) {
	return driver.ExecResult{}, u.err()
}
