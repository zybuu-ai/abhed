package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
)

// templates checks the store keeps templates: create once, read back,
// list by selector in pages, and delete guarded by the resource version.
func templates(t *testing.T, st controlplane.Store) {
	ts, ok := st.(controlplane.TemplateStore)
	if !ok {
		t.Fatalf("%T keeps no templates", st)
	}
	ctx := context.Background()
	mk := func(name, team string) controlplane.Template {
		return controlplane.Template{Name: name, Driver: "process", Owner: "root", CreatedAt: t0,
			Labels: map[string]string{"team": team}, Settings: map[string]string{"isolation.min_tier": "container"}}
	}
	got, err := ts.CreateTemplate(ctx, mk("web", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion < 1 {
		t.Fatalf("resource version %d", got.ResourceVersion)
	}
	if _, err := ts.CreateTemplate(ctx, mk("web", "b")); !errors.Is(err, controlplane.ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	if _, err := ts.CreateTemplate(ctx, mk("../x", "b")); !errors.Is(err, controlplane.ErrInvalid) {
		t.Fatalf("a bad name: %v", err)
	}
	back, err := ts.GetTemplate(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "web" || back.Driver != "process" || back.Labels["team"] != "a" || back.Owner != "root" ||
		back.Settings["isolation.min_tier"] != "container" || back.ResourceVersion != got.ResourceVersion || !back.CreatedAt.Equal(t0) {
		t.Fatalf("read back %+v", back)
	}
	if _, err := ts.GetTemplate(ctx, "nope"); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// A template and a sandbox share the store's counter.
	sb, err := st.Create(ctx, sandbox(t, "alice", nil))
	if err != nil {
		t.Fatal(err)
	}
	if sb.ResourceVersion <= got.ResourceVersion {
		t.Fatalf("versions %d then %d", got.ResourceVersion, sb.ResourceVersion)
	}
	for i := range 4 {
		if _, err := ts.CreateTemplate(ctx, mk(fmt.Sprintf("t%d", i), []string{"a", "b"}[i%2])); err != nil {
			t.Fatal(err)
		}
	}
	sel, _ := controlplane.ParseSelector("team=a")
	var names []string
	after := ""
	for {
		page, next, err := ts.ListTemplates(ctx, controlplane.TemplateListOptions{Selector: sel, Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			names = append(names, p.Name)
		}
		if next == "" {
			break
		}
		after = next
	}
	if fmt.Sprint(names) != "[t0 t2 web]" {
		t.Fatalf("listed %v", names)
	}
	if err := ts.DeleteTemplate(ctx, "web", got.ResourceVersion+100); !errors.Is(err, controlplane.ErrVersionMismatch) {
		t.Fatalf("stale delete: %v", err)
	}
	if err := ts.DeleteTemplate(ctx, "web", got.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := ts.DeleteTemplate(ctx, "web", got.ResourceVersion); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
