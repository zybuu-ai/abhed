package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// entry is one member of a fake archive.
type entry struct {
	name, body, link string
	mode             int64
	typ              byte
}

// tarGz packs entries into a .tar.gz.
func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body))}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// fake is a release source: paths to bodies, and a count of what was asked.
type fake struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	asked []string
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.asked = append(f.asked, r.URL.RequestURI())
		body, ok := f.files[r.URL.RequestURI()]
		if !ok {
			body, ok = f.files[r.URL.Path]
		}
		f.mu.Unlock()
		if strings.HasPrefix(string(body), "REDIRECT ") {
			http.Redirect(w, r, strings.TrimPrefix(string(body), "REDIRECT "), http.StatusFound)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) put(path string, body []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = body
	return f.srv.URL + path
}

func (f *fake) source() *Source {
	u, _ := url.Parse(f.srv.URL)
	b := f.srv.URL + "/"
	return &Source{Endpoints: Endpoints{GoDL: b + "dl/", NodeDist: b + "dist/", PythonAPI: b + "repos/pbs/releases",
		RustDist: b + "rust/", AdoptiumAPI: b + "adoptium/v3/", MavenRepo: b + "maven2/", GradleAPI: b + "gradle/",
		DotnetIndex: b + "dotnet/releases-index.json", RubyDownload: b + "ruby/", ExtraHosts: []string{u.Host}}}
}

// goRelease serves a Go release list with archives for the platforms.
func (f *fake) goRelease(version string, stable bool, archive []byte, platforms ...Platform) {
	var files []string
	for _, p := range platforms {
		name := fmt.Sprintf("go%s.%s-%s.tar.gz", version, p.OS, p.Arch)
		f.put("/dl/"+name, archive)
		files = append(files, fmt.Sprintf(`{"filename":%q,"os":%q,"arch":%q,"sha256":%q,"size":%d,"kind":"archive"}`,
			name, p.OS, p.Arch, sum(archive), len(archive)))
	}
	entry := fmt.Sprintf(`{"version":"go%s","stable":%v,"files":[%s]}`, version, stable, strings.Join(files, ","))
	f.mu.Lock()
	cur := string(f.files["/dl/?mode=json&include=all"])
	f.mu.Unlock()
	if cur == "" {
		cur = "[]"
	}
	if cur == "[]" {
		cur = "[" + entry + "]"
	} else {
		cur = "[" + entry + "," + strings.TrimPrefix(cur, "[")
	}
	f.put("/dl/?mode=json&include=all", []byte(cur))
}

// testCache is a cache in a temporary directory, removed whole afterwards
// though its toolchains are read-only.
func testCache(t *testing.T) Cache {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { _ = removeTree(root) })
	return Cache{Root: root}
}
