package config

import (
	"testing"
)

// The container drivers' settings that reach more of the host or loosen a
// bound come only from the managed file; enabling one and its default image
// may come from the user's own.
func TestContainerDriverWideningIsManagedOnly(t *testing.T) {
	for _, engine := range []string{"docker", "podman"} {
		for k := range containerManagedOnly {
			if !ManagedOnly("control_plane." + engine + "." + k) {
				t.Errorf("control_plane.%s.%s is not managed only", engine, k)
			}
		}
		for _, k := range []string{"enabled", "image"} {
			if ManagedOnly("control_plane." + engine + "." + k) {
				t.Errorf("control_plane.%s.%s is managed only", engine, k)
			}
		}
	}
	user := `{"control_plane":{"podman":{"enabled":true,"image":"alpine:3","binary":"/tmp/evil","images":["evil:1"],
		"mounts":[{"host":"/","target":"/host","rw":true}],"mount_roots":[{"path":"/home","rw":true}],
		"gpus":["nvidia.com/gpu=all"],"cpus":64,"seccomp_profile":"/tmp/open.json"},
		"per_session":true,"sessions":{"scope":"user","warm_pool":2}}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.ControlPlane.Podman
	if !p.Enabled || p.Image != "alpine:3" || !cfg.ControlPlane.PerSession || cfg.ControlPlane.Sessions.WarmPool != 2 {
		t.Fatalf("the user's own settings were not kept: %+v %+v", p, cfg.ControlPlane.Sessions)
	}
	if p.Binary != "" || p.Images != nil || p.Mounts != nil || p.MountRoots != nil || p.GPUs != nil || p.CPUs != 0 || p.SeccompProfile != "" {
		t.Fatalf("the user's file widened the podman driver: %+v", p)
	}
	withManaged(t, `{"control_plane":{"docker":{"images":["python:3.12"],"gpus":["nvidia.com/gpu=all"],"cpus":4,
		"mount_roots":[{"path":"/srv/data"}]}}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.ControlPlane.Docker
	if len(d.Images) != 1 || len(d.GPUs) != 1 || d.CPUs != 4 || len(d.MountRoots) != 1 || d.MountRoots[0].Path != "/srv/data" {
		t.Fatalf("the managed settings were not kept: %+v", d)
	}
}
