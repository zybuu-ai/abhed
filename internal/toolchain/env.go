package toolchain

import (
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

// Vars are the variables an installed toolchain needs besides PATH.
func Vars(in Installed) map[string]string {
	switch in.Lang {
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
