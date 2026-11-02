package toolchain

import (
	"os"
	"path/filepath"
	"strings"
)

// BinDirs are where an installed toolchain's programs are, for PATH.
func BinDirs(in Installed) []string {
	switch in.Lang {
	case "java":
		return []string{filepath.Join(javaHome(in), "bin")}
	case "dotnet":
		return []string{in.Dir}
	case "ruby":
		if hasRubyWrapper(in) {
			return []string{filepath.Join(in.Dir, rubyWrapperDir), filepath.Join(in.Dir, "bin")}
		}
	}
	return []string{filepath.Join(in.Dir, "bin")}
}

// javaHome is a JDK's home: macOS builds keep it under Contents/Home.
func javaHome(in Installed) string {
	if strings.HasPrefix(in.Platform, "darwin-") {
		return filepath.Join(in.Dir, "Contents", "Home")
	}
	return in.Dir
}

// SearchPaths are the path-list variables an installed toolchain puts its
// folders first in: a Linux Ruby's own libraries and the ones it bundles,
// which its build looks for at the build machine's path.
func SearchPaths(in Installed) map[string][]string {
	if in.Lang != "ruby" {
		return nil
	}
	out := rubyPaths(in)
	// A Ruby installed before its wrapper existed finds its libraries this way.
	if strings.HasPrefix(in.Platform, "linux-") && !hasRubyWrapper(in) {
		out["LD_LIBRARY_PATH"] = []string{filepath.Join(in.Dir, "lib"), filepath.Join(in.Dir, "lib", "abhed-deps")}
	}
	return out
}

// hasRubyWrapper reports whether a Linux Ruby has the wrapper that gives it
// its libraries.
func hasRubyWrapper(in Installed) bool {
	info, err := os.Lstat(filepath.Join(in.Dir, rubyWrapperDir, "ruby"))
	return err == nil && info.Mode().IsRegular()
}

// rubyPaths are a Ruby build's library and gem folders where it is: the
// build names the build machine's prefix, so without them neither its
// standard library nor RubyGems loads.
func rubyPaths(in Installed) map[string][]string {
	parts := strings.Split(in.Version, ".")
	if len(parts) < 2 {
		return map[string][]string{}
	}
	abi := parts[0] + "." + parts[1] + ".0"
	lib := filepath.Join(in.Dir, "lib", "ruby")
	arch := ""
	if m, _ := filepath.Glob(filepath.Join(lib, abi, "*", "rbconfig.rb")); len(m) > 0 {
		arch = filepath.Base(filepath.Dir(m[0]))
	}
	var rubylib []string
	for _, d := range []string{"site_ruby", "vendor_ruby"} {
		rubylib = append(rubylib, filepath.Join(lib, d, abi))
		if arch != "" {
			rubylib = append(rubylib, filepath.Join(lib, d, abi, arch))
		}
		rubylib = append(rubylib, filepath.Join(lib, d))
	}
	rubylib = append(rubylib, filepath.Join(lib, abi))
	if arch != "" {
		rubylib = append(rubylib, filepath.Join(lib, abi, arch))
	}
	return map[string][]string{"RUBYLIB": rubylib, "GEM_PATH": {filepath.Join(lib, "gems", abi)}}
}

// Vars are the variables an installed toolchain needs besides PATH. They
// override the host's, which commands inherit: a host GOROOT with this Go
// first on PATH mixes two Go versions in one build.
func Vars(in Installed) map[string]string {
	switch in.Lang {
	case "go":
		return map[string]string{"GOROOT": in.Dir, "GOTOOLCHAIN": "local"}
	case "java":
		return map[string]string{"JAVA_HOME": javaHome(in)}
	case "dotnet":
		return map[string]string{"DOTNET_ROOT": in.Dir, "DOTNET_CLI_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1"}
	case "maven":
		return map[string]string{"MAVEN_HOME": in.Dir}
	case "gradle":
		return map[string]string{"GRADLE_HOME": in.Dir}
	}
	return nil
}

// Unsets are the host's variables that would make an installed toolchain
// use another installation's files, removed while it is active.
func Unsets(in Installed) []string {
	switch in.Lang {
	case "python":
		return []string{"PYTHONHOME"}
	case "ruby":
		return []string{"GEM_HOME", "RUBYOPT", "RUBYLIB_PREFIX"}
	case "rust":
		return []string{"RUSTC", "RUSTDOC", "RUSTUP_TOOLCHAIN", "RUSTC_WRAPPER"}
	case "node":
		return []string{"NODE_PATH"}
	case "java", "maven", "gradle":
		return []string{"JDK_HOME"}
	case "dotnet":
		return []string{"DOTNET_HOST_PATH"}
	}
	return nil
}

// Rebase moves an installed toolchain's directory from the cache root on
// this host to where a sandbox sees it, such as a container's mount.
func Rebase(in Installed, hostRoot, sandboxRoot string) Installed {
	rel, err := filepath.Rel(hostRoot, in.Dir)
	if err != nil {
		return in
	}
	in.Dir = filepath.Join(sandboxRoot, rel)
	return in
}
