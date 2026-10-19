package toolset

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/webfetch"
	"github.com/zybuu-ai/abhed/internal/websearch"
)

func storeWith(t *testing.T, value string) *secrets.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(p, []byte(`{"TOKEN":"`+value+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return secrets.Open(p)
}

// Every tool that reads the store is bound to another by BindStores, so a
// server that binds a session to its owner's store leaves no reader of the
// store the tool set was built with.
func TestBindStoresRebindsEveryReader(t *testing.T) {
	cfg := config.Default()
	cfg.K8s.Enabled = true
	cfg.K8s.AllowWrites = true
	cfg.SSH.Enabled = true
	cfg.SSH.Transfers = true
	cfg.WebFetch.Enabled = true
	cfg.WebSearch.Enabled = true
	cfg.WebSearch.Provider = "duckduckgo"
	built := storeWith(t, "built-with-value-0a1b")
	set := Build(t.Context(), cfg, Options{Workspace: t.TempDir(), Vault: built, Parts: Infra | WebFetch | WebSearch})
	defer set.Close()
	bound := tools.BindStores(set.Registry, storeWith(t, "bound-to-value-9f8e"))

	want := "bound-to-value-9f8e"
	value := map[string]func(tools.Tool) (string, error){
		"bash": func(t tools.Tool) (string, error) {
			env, err := t.(tools.Bash).Secrets([]string{"TOKEN"})
			if err != nil {
				return "", err
			}
			return env[0][len("TOKEN="):], nil
		},
		"k8s_login":   func(t tools.Tool) (string, error) { return t.(k8s.LoginTool).Secret("TOKEN") },
		"ssh_connect": func(t tools.Tool) (string, error) { return t.(remote.ConnectTool).Secret("TOKEN") },
		"ssh":         func(t tools.Tool) (string, error) { return t.(remote.Tool).Secret("TOKEN") },
		"ssh_put":     func(t tools.Tool) (string, error) { return t.(remote.PutTool).Secret("TOKEN") },
		"k8s_apply":   func(t tools.Tool) (string, error) { return t.(k8s.ApplyTool).Secret("TOKEN") },
		"web_fetch": func(t tools.Tool) (string, error) {
			r, err := t.(*webfetch.Tool).Secrets()
			if err != nil {
				return "", err
			}
			if _, ok := r.Find(want); ok {
				return want, nil
			}
			return "", nil
		},
		"web_search": func(t tools.Tool) (string, error) {
			r, err := t.(*websearch.Tool).Secrets()
			if err != nil {
				return "", err
			}
			if _, ok := r.Find(want); ok {
				return want, nil
			}
			return "", nil
		},
	}
	for name, read := range value {
		tool, ok := bound.Get(name)
		if !ok {
			t.Fatalf("%s is not in the tool set", name)
		}
		if _, ok := tool.(tools.StoreBound); !ok {
			t.Errorf("%s reads the store but cannot be bound to another", name)
			continue
		}
		if got, err := read(tool); err != nil || got != want {
			t.Errorf("%s reads %q (%v) after binding, want the bound store's value", name, got, err)
		}
	}
	// Whatever reads a secret by name and holds a store reader is bound,
	// including a tool added later that the list above does not name.
	for _, name := range set.Registry.Names() {
		tool, _ := set.Registry.Get(name)
		_, named := tool.(tools.SecretArgs)
		_, deep := tool.(tools.SecretPaths)
		if !named && !deep {
			continue
		}
		v := reflect.ValueOf(tool)
		if v.Kind() == reflect.Pointer {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}
		f := v.FieldByName("Secret")
		if !f.IsValid() || f.Kind() != reflect.Func || f.IsNil() {
			continue
		}
		if _, ok := tool.(tools.StoreBound); !ok {
			t.Errorf("%s reads secrets by name from the store it was built with and cannot be bound to a session's", name)
			continue
		}
		b, _ := bound.Get(name)
		bv := reflect.ValueOf(b)
		if bv.Kind() == reflect.Pointer {
			bv = bv.Elem()
		}
		out := bv.FieldByName("Secret").Call([]reflect.Value{reflect.ValueOf("TOKEN")})
		if got, _ := out[0].Interface().(string); got != want {
			t.Errorf("%s reads %q after binding, want the bound store's value", name, got)
		}
	}
	if b, _ := bound.Get("bash"); !slices.Equal(b.(tools.Bash).SecretNames, []string{"TOKEN"}) {
		t.Errorf("bash offers %v", b.(tools.Bash).SecretNames)
	}
	// The registry it was built from is left as it was, for other sessions.
	orig, _ := set.Registry.Get("k8s_login")
	if v, _ := orig.(k8s.LoginTool).Secret("TOKEN"); v != "built-with-value-0a1b" {
		t.Errorf("binding changed the shared registry: %q", v)
	}
}
