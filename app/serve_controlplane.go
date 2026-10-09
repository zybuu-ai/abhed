package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/driver/process"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
)

// controlPlaneStore is where the control plane keeps sandboxes: the
// Postgres the event store uses, or memory when serve keeps events there.
func controlPlaneStore(ctx context.Context, events server.EventStore) (controlplane.Store, controlplane.AuditLog, string, error) {
	var pg *store.Postgres
	switch st := events.(type) {
	case *store.Postgres:
		pg = st
	case *store.Held:
		pg = st.Postgres
	}
	if pg == nil {
		mem := controlplane.NewMemory()
		return mem, mem, "memory (sandboxes do not survive restart)", nil
	}
	q, err := pg.Sandboxes(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	return q, q, "postgres (tenant " + pg.Tenant() + ")", nil
}

// sandboxWorkspaces gives each sandbox its folder: one of its own under
// root, or the server's workspace when root is empty.
func sandboxWorkspaces(root, workspace string) (get func(string) (string, error), release func(string) error, err error) {
	if root == "" {
		return func(string) (string, error) { return workspace, nil }, nil, nil
	}
	if !filepath.IsAbs(root) {
		return nil, nil, fmt.Errorf("control_plane.workspace_root %q is not an absolute path", root)
	}
	dir := func(id string) (string, error) {
		if !controlplane.ValidID(id) {
			return "", fmt.Errorf("sandbox id %q", id)
		}
		return filepath.Join(root, id), nil
	}
	get = func(id string) (string, error) {
		d, err := dir(id)
		if err != nil {
			return "", err
		}
		return d, os.MkdirAll(d, 0o700)
	}
	release = func(id string) error {
		d, err := dir(id)
		if err != nil {
			return err
		}
		return os.RemoveAll(d)
	}
	return get, release, nil
}

// buildControlPlane builds the control plane serve runs when
// control_plane.enabled is set: the process driver over the configured
// sandbox, the store beside the event store, and the settings registry.
func buildControlPlane(ctx context.Context, cfg config.Config, workspace string, events server.EventStore,
	redact func(string) string) (*controlplane.Service, string, error) {
	settings, err := controlplane.SettingsFromConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("control_plane: %w", err)
	}
	drivers := driver.NewRegistry()
	if err := drivers.Register(process.New(cfg)); err != nil {
		return nil, "", fmt.Errorf("control_plane: %w", err)
	}
	st, audit, label, err := controlPlaneStore(ctx, events)
	if err != nil {
		return nil, "", fmt.Errorf("control_plane: %w", err)
	}
	get, release, err := sandboxWorkspaces(cfg.ControlPlane.WorkspaceRoot, workspace)
	if err != nil {
		return nil, "", err
	}
	svc, err := controlplane.New(controlplane.Config{Store: st, Audit: audit, Drivers: drivers, Settings: settings,
		DefaultDriver: process.Name, Workspace: get, ReleaseWorkspace: release, Redact: redact})
	if err != nil {
		return nil, "", err
	}
	return svc, label, nil
}

// checkLine is one preflight result.
type checkLine struct {
	name, detail string
	ok           bool
}

// serveCheck is `abhed serve -check`: everything serve would refuse to
// start over, found without serving. It binds the listen address only to
// release it, and starts nothing.
func (a *App) serveCheck(w io.Writer, workspace, addr string) int {
	var lines []checkLine
	add := func(name string, err error, detail string) {
		if err != nil {
			detail = err.Error()
		}
		lines = append(lines, checkLine{name: name, ok: err == nil, detail: detail})
	}
	defer func() { printChecks(w, lines) }()

	cfg, err := a.loadConfig(workspace)
	if err == nil {
		err = cfg.Workspace.DeploymentError("serve")
	}
	add("config", err, "loaded")
	if err != nil {
		var ed *EditionError
		if errors.As(err, &ed) {
			return 2
		}
		return 1
	}
	var unknown []string
	for _, u := range cfg.Unknown {
		unknown = append(unknown, u.String())
	}
	if len(unknown) > 0 {
		add("config keys", fmt.Errorf("keys nothing reads: %s", strings.Join(unknown, "; ")), "")
	}
	if keys := cfg.NotYetInEffect(); len(keys) > 0 {
		add("config keys", fmt.Errorf("set but not in effect in this version: %s", strings.Join(keys, ", ")), "")
	}
	if p, err := cfg.Provider(); err != nil {
		add("model", err, "")
	} else {
		add("model", nil, p.Model)
	}
	add("secrets", vaultLoads(), "readable")

	cfg, sb, _, err := serveSandbox(cfg, workspace)
	if err == nil {
		detail := string(sb.Tier())
		if f := fenceOf(sb); f != nil && f.Mode() != sandbox.FenceModeMounts {
			err = errors.New("sandbox.tier is fence and this host does not give serve its mount_namespace mode")
		}
		add("sandbox", err, detail)
		if c, ok := sb.(io.Closer); ok {
			_ = c.Close()
		}
	} else {
		add("sandbox", err, "")
	}

	if mw, err := a.buildAuth(context.Background(), cfg, workspace); err != nil {
		add("auth", err, "")
	} else {
		add("auth", nil, authLabel(cfg, mw))
	}

	events, closeStore, err := openServeStore(context.Background(), cfg)
	if err != nil {
		add("storage", err, "")
	} else {
		defer closeStore()
		add("storage", nil, serveStorageLabel(cfg))
	}

	if cfg.ControlPlane.Enabled {
		switch {
		case events == nil:
			add("control plane", errors.New("needs the event store, which did not open"), "")
		default:
			svc, label, err := buildControlPlane(context.Background(), cfg, workspace, events, nil)
			if err == nil {
				_, _, err = svc.Store().List(context.Background(), controlplane.ListOptions{Limit: 1})
			}
			add("control plane", err, "on, store "+label)
			if svc != nil {
				for _, s := range svc.Settings().List() {
					detail := s.Value
					if detail == "" {
						detail = "(none)"
					}
					detail += " (" + s.Source
					if s.Ceiling {
						detail += ", ceiling"
					}
					lines = append(lines, checkLine{name: "  " + s.Key, ok: true, detail: detail + ")"})
				}
			}
		}
	} else {
		add("control plane", nil, "off (control_plane.enabled)")
	}

	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
	}
	add("listen", err, addr)

	for _, l := range lines {
		if !l.ok {
			return 1
		}
	}
	return 0
}

func printChecks(w io.Writer, lines []checkLine) {
	failed := 0
	for _, l := range lines {
		mark := "ok  "
		if !l.ok {
			mark, failed = "FAIL", failed+1
		}
		fmt.Fprintf(w, "%s %-28s %s\n", mark, l.name, l.detail)
	}
	if failed == 0 {
		fmt.Fprintln(w, "\nReady to serve.")
		return
	}
	fmt.Fprintf(w, "\nNot ready: %d check(s) failed.\n", failed)
}
