// Package builtin builds the driver registry a configuration describes: the
// process driver always, the docker and podman drivers when
// control_plane.docker or control_plane.podman is enabled, and the microvm
// driver when the managed configuration names an abhed-microvm socket. None
// of them changes the default, which stays the process driver.
package builtin

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/container"
	"github.com/zybuu-ai/abhed/driver/microvm"
	"github.com/zybuu-ai/abhed/driver/plugin"
	"github.com/zybuu-ai/abhed/driver/process"
)

// Default is the driver a sandbox gets when it names none.
const Default = process.Name

// dialTimeout bounds reaching the microvm driver and probing an engine.
const dialTimeout = 5 * time.Second

// probeTimeout bounds asking a container engine whether it answers.
const probeTimeout = 10 * time.Second

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
	// Container is what the docker and podman drivers get beside their
	// configured section: the record and the snapshot folder.
	Container container.Options
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
	for _, c := range []struct {
		name string
		sec  config.ContainerDriverConfig
		make func(config.Config, container.Options) (*container.Driver, error)
	}{
		{container.Docker, cfg.ControlPlane.Docker, container.NewDocker},
		{container.Podman, cfg.ControlPlane.Podman, container.NewPodman},
	} {
		if !c.sec.Enabled {
			continue
		}
		d, err := c.make(cfg, ContainerOptions(c.sec, opts.Container))
		if err != nil {
			return nil, nil, fmt.Errorf("control_plane.%s: %w", c.name, err)
		}
		e := Entry{Name: c.name, Detail: "engine " + orName(c.sec.Binary, c.name)}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		// Registered either way: an engine that does not answer refuses each
		// sandbox until it does, and never hands it to another driver.
		e.Err = d.Probe(pctx)
		cancel()
		if err := r.Register(d); err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
	}
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

// ContainerOptions are a container driver's options from its configured
// section, on top of base.
func ContainerOptions(sec config.ContainerDriverConfig, base container.Options) container.Options {
	o := base
	o.Binary, o.Image, o.Images, o.GPUs, o.CPUs, o.SeccompProfile = sec.Binary, sec.Image, sec.Images, sec.GPUs, sec.CPUs, sec.SeccompProfile
	o.Mounts, o.MountRoots = nil, nil
	for _, m := range sec.Mounts {
		o.Mounts = append(o.Mounts, container.Mount{Host: m.Host, Target: m.Target, Writable: m.Writable, Relabel: m.Relabel})
	}
	for _, m := range sec.MountRoots {
		o.MountRoots = append(o.MountRoots, container.MountRoot{Path: m.Path, Writable: m.Writable, Relabel: m.Relabel})
	}
	return o
}

func orName(v, name string) string {
	if v == "" {
		return name + " on PATH"
	}
	return v
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
