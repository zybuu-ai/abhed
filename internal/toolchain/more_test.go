package toolchain

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sum512(b []byte) string { s := sha512.Sum512(b); return hex.EncodeToString(s[:]) }

// Rust: the channel manifest names the build and its sha256; the components
// are laid out under the prefix as install.sh would, the docs left out.
func TestRustFromItsChannel(t *testing.T) {
	f := newFake(t)
	top := "rust-1.80.1-x86_64-unknown-linux-gnu/"
	arch := tarGz(t,
		entry{name: top + "components", body: "rustc\ncargo\nrust-docs\n"},
		entry{name: top + "install.sh", body: "#!/bin/sh\nexit 1\n", mode: 0o755},
		entry{name: top + "rustc/manifest.in", body: "file:bin/rustc\ndir:lib/rustlib/x\n"},
		entry{name: top + "rustc/bin/rustc", body: "rustc", mode: 0o755},
		entry{name: top + "rustc/lib/rustlib/x/a", body: "a"},
		entry{name: top + "cargo/manifest.in", body: "file:bin/cargo\n"},
		entry{name: top + "cargo/bin/cargo", body: "cargo", mode: 0o755},
		entry{name: top + "rust-docs/manifest.in", body: "file:share/doc/big\n"},
		entry{name: top + "rust-docs/share/doc/big", body: "docs"})
	u := f.put("/rust/2026-10-01/rust-1.80.1-x86_64-unknown-linux-gnu.tar.gz", arch)
	manifest := fmt.Sprintf("manifest-version = \"2\"\n\n[pkg.rust]\nversion = \"1.80.1 (abc 2026-10-01)\"\n\n"+
		"[pkg.rust.target.x86_64-unknown-linux-gnu]\navailable = true\nurl = %q\nhash = %q\n\n[pkg.rust.target.aarch64-apple-darwin]\navailable = false\n", u, sum(arch))
	f.put("/rust/channel-rust-stable.toml", []byte(manifest))
	f.put("/rust/channel-rust-1.80.toml", []byte(manifest))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "rust"}, linuxAMD64)
	if err != nil || a.Version != "1.80.1" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := src.Resolve(context.Background(), Spec{Lang: "rust", Version: "1.80"}, linuxAMD64); err != nil || a.Version != "1.80.1" {
		t.Fatalf("1.80: %+v %v", a, err)
	}
	if _, err := src.Resolve(context.Background(), Spec{Lang: "rust"}, Platform{"darwin", "arm64"}); err == nil {
		t.Fatal("an unavailable target resolved")
	}
	in, err := testCache(t).Install(context.Background(), src, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/rustc", "bin/cargo", "lib/rustlib/x/a"} {
		if _, err := os.Stat(filepath.Join(in.Dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "share", "doc", "big")); err == nil {
		t.Error("the docs were installed")
	}
}

// Java: Temurin's newest JDK of the newest LTS, from the Adoptium API.
func TestJavaFromAdoptium(t *testing.T) {
	f := newFake(t)
	arch := tarGz(t, entry{name: "jdk-21.0.5+11/bin/java", body: "java", mode: 0o755})
	u := f.put("/gh/OpenJDK21U-jdk_x64_linux_hotspot_21.0.5_11.tar.gz", arch)
	f.put("/adoptium/v3/info/available_releases", []byte(`{"most_recent_lts":21}`))
	f.put("/adoptium/v3/assets/latest/21/hotspot?architecture=x64&image_type=jdk&os=linux&vendor=eclipse",
		[]byte(fmt.Sprintf(`[{"binary":{"package":{"checksum":%q,"link":%q,"name":"OpenJDK21U-jdk_x64_linux_hotspot_21.0.5_11.tar.gz","size":%d}},"version":{"major":21,"minor":0,"security":5}}]`, sum(arch), u, len(arch))))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "java"}, linuxAMD64)
	if err != nil || a.Version != "21.0.5" || a.Digest != "sha256:"+sum(arch) {
		t.Fatalf("%+v %v", a, err)
	}
	in, err := testCache(t).Install(context.Background(), src, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Vars(in)["JAVA_HOME"]; got != in.Dir {
		t.Fatalf("JAVA_HOME %q", got)
	}
	mac := Installed{Lang: "java", Platform: "darwin-arm64", Dir: "/c/jdk"}
	if BinDirs(mac)[0] != "/c/jdk/Contents/Home/bin" {
		t.Fatalf("mac bin %v", BinDirs(mac))
	}
}

// Maven: the newest final version from Maven Central's metadata, its sha512.
func TestMavenFromCentral(t *testing.T) {
	f := newFake(t)
	arch := tarGz(t, entry{name: "apache-maven-3.9.9/bin/mvn", body: "mvn", mode: 0o755})
	f.put("/maven2/org/apache/maven/apache-maven/maven-metadata.xml", []byte(`<metadata><versioning><versions>
	  <version>3.8.8</version><version>3.9.9</version><version>4.0.0-rc-2</version></versions></versioning></metadata>`))
	f.put("/maven2/org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-bin.tar.gz", arch)
	f.put("/maven2/org/apache/maven/apache-maven/3.9.9/apache-maven-3.9.9-bin.tar.gz.sha512", []byte(sum512(arch)+"  apache-maven-3.9.9-bin.tar.gz\n"))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "maven"}, linuxAMD64)
	if err != nil || a.Version != "3.9.9" || a.Digest != "sha512:"+sum512(arch) {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := testCache(t).Install(context.Background(), src, a, nil); err != nil {
		t.Fatal(err)
	}
}

// Gradle: the newest final release, a zip, checked against its sha256.
func TestGradleFromServices(t *testing.T) {
	f := newFake(t)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "gradle-8.10/bin/gradle", Method: zip.Deflate})
	_, _ = w.Write([]byte("gradle"))
	_ = zw.Close()
	z := buf.Bytes()
	u := f.put("/gradle/distributions/gradle-8.10-bin.zip", z)
	f.put("/gradle/versions/all", []byte(fmt.Sprintf(`[
	  {"version":"9.0-rc-1","downloadUrl":"x","checksum":"","rcFor":"9.0"},
	  {"version":"8.10","downloadUrl":%q,"checksum":%q},
	  {"version":"8.9","downloadUrl":"y","checksum":"","broken":true}]`, u, sum(z))))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "gradle"}, linuxAMD64)
	if err != nil || a.Version != "8.10" || a.Format != "zip" {
		t.Fatalf("%+v %v", a, err)
	}
	in, err := testCache(t).Install(context.Background(), src, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "bin", "gradle")); err != nil {
		t.Fatal(err)
	}
}

// .NET: the newest supported channel's newest SDK, or the channel asked
// for, with its sha512; an archive with no top folder is the toolchain.
func TestDotnetFromItsMetadata(t *testing.T) {
	f := newFake(t)
	arch := tarGz(t, entry{name: "dotnet", body: "dotnet", mode: 0o755}, entry{name: "sdk/8.0.403/x", body: "x"})
	u := f.put("/dotnet/8.0/dotnet-sdk-8.0.403-linux-x64.tar.gz", arch)
	rel8 := fmt.Sprintf(`{"releases":[{"sdk":{"version":"8.0.403","files":[{"name":"dotnet-sdk-linux-x64.tar.gz","rid":"linux-x64","url":%q,"hash":%q}]},"sdks":[]}]}`, u, sum512(arch))
	f.put("/dotnet/8.0/releases.json", []byte(rel8))
	f.put("/dotnet/releases-index.json", []byte(fmt.Sprintf(`{"releases-index":[
	  {"channel-version":"9.0","support-phase":"preview","releases.json":"%s/dotnet/9.0/releases.json"},
	  {"channel-version":"8.0","support-phase":"active","releases.json":"%s/dotnet/8.0/releases.json"}]}`, f.srv.URL, f.srv.URL)))
	src := f.source()
	a, err := src.Resolve(context.Background(), Spec{Lang: "dotnet"}, linuxAMD64)
	if err != nil || a.Version != "8.0.403" || a.Digest != "sha512:"+sum512(arch) {
		t.Fatalf("%+v %v", a, err)
	}
	in, err := testCache(t).Install(context.Background(), src, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "dotnet")); err != nil || BinDirs(in)[0] != in.Dir || Vars(in)["DOTNET_ROOT"] != in.Dir {
		t.Fatalf("%v %v", err, BinDirs(in))
	}
}

// Ruby: only a version and platform in the pinned table, from ruby-builder.
func TestRubyFromThePinnedTable(t *testing.T) {
	src := &Source{Endpoints: Official}
	a, err := src.Resolve(context.Background(), Spec{Lang: "ruby"}, linuxAMD64)
	if err != nil || a.Version != "3.4.6" || a.URL != Official.RubyDownload+"ruby-3.4.6-ubuntu-22.04.tar.gz" ||
		a.Digest != "sha256:6fe5f3d274308e6d159bec44286aef46568ce3de82b9fec172eba6edb503bd52" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := src.Resolve(context.Background(), Spec{Lang: "ruby", Version: "3.3"}, Platform{"darwin", "arm64"}); err != nil || a.Version != "3.3.9" {
		t.Fatalf("3.3: %+v %v", a, err)
	}
	for _, bad := range []Spec{{Lang: "ruby", Version: "2.7"}, {Lang: "ruby", Version: "3.4.1"}} {
		if _, err := src.Resolve(context.Background(), bad, linuxAMD64); err == nil {
			t.Errorf("%v resolved", bad)
		}
	}
	if _, err := src.Resolve(context.Background(), Spec{Lang: "ruby"}, Platform{"darwin", "amd64"}); err == nil {
		t.Error("a platform with no pinned build resolved")
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "bin", "gem"), []byte("#!/opt/hostedtoolcache/Ruby/3.4.6/x64/bin/ruby\nputs 1\n"), 0o755)
	if _, err := postInstall["ruby"](dir, a); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bin", "gem")); !strings.HasPrefix(string(b), "#!/usr/bin/env ruby\n") {
		t.Fatalf("shebang %q", b)
	}
}

// An opt-in check against the real sources: ABHED_TOOLCHAIN_LIVE=1 resolves
// every language for this host, downloading nothing.
func TestLiveResolve(t *testing.T) {
	if os.Getenv("ABHED_TOOLCHAIN_LIVE") != "1" {
		t.Skip("set ABHED_TOOLCHAIN_LIVE=1 to ask the real release sources")
	}
	src := &Source{Endpoints: Official, UserAgent: "abhed-test"}
	for _, p := range []Platform{Host(), {OS: "linux", Arch: Host().Arch}} {
		for _, l := range Langs {
			a, err := src.Resolve(context.Background(), Spec{Lang: l.Name}, p)
			if err != nil {
				t.Errorf("%s %s: %v", p, l.Name, err)
				continue
			}
			t.Logf("%s %s %s %s %s", p, l.Name, a.Version, a.URL, a.Digest[:20])
		}
	}
	// One real install, the smallest: Node for this host, which then runs.
	a, err := src.Resolve(context.Background(), Spec{Lang: "node"}, Host())
	if err != nil {
		t.Fatal(err)
	}
	in, err := testCache(t).Install(context.Background(), src, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(BinDirs(in)[0], "node"), "--version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "v"+a.Version {
		t.Fatalf("node --version: %q %v", out, err)
	}
}
