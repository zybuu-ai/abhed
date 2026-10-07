#!/usr/bin/env bash
# The fence tier's gated tests with ABHED_REQUIRE_FENCE=1: none may skip.
# Usage: fence-gated.sh build|prepare|check|run|run-network DIR (see ci.yml).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
mod=github.com/zybuu-ai/abhed

# suites PKGS prints name|package|-run pattern. Fence packages run whole, so
# a new one is covered.
suites() {
	local pkg
	for pkg in $1; do echo "fence-${pkg##*/}|$pkg|."; done
	echo "sandbox|$mod/internal/sandbox|Fence|Mounts|Launch|Select"
	echo "clitest|$mod/internal/clitest|Fence"
}

build() {
	local dir=$1 fence all name pkg pattern tests files
	# The AppArmor profile trusts what is under DIR, so it must be new and ours.
	[ ! -e "$dir" ] || { echo "$dir already exists"; return 1; }
	install -d -m 0755 "$dir"
	[ -O "$dir" ] || { echo "$dir is not owned by $(id -un)"; return 1; }
	cd "$root"
	python3 scripts/ci/fence-gated-tests.py --self-test
	# On its own, so a failed go list stops the build rather than drop suites.
	fence=$(go list ./internal/fence/...)
	[ -n "$fence" ] || { echo "go list found no fence package"; return 1; }
	all=$(suites "$fence")
	: > "$dir/suites"
	while IFS='|' read -r -u 3 name pkg pattern; do
		tests=$(go list -f '{{range .TestGoFiles}}{{$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}} {{end}}' "$pkg")
		[ -n "$tests" ] || { echo "$pkg has no tests for this platform; nothing to run"; continue; }
		# The package's own code too: a gate helper may live outside the tests.
		files="$(go list -f '{{range .GoFiles}}{{$.Dir}}/{{.}} {{end}}' "$pkg") $tests"
		go test -c -o "$dir/$name.test" "$pkg"
		[ -x "$dir/$name.test" ] || { echo "$pkg has tests but built no binary"; return 1; }
		# Gated by what the tests check, not by name: run must list every one.
		# shellcheck disable=SC2086
		python3 scripts/ci/fence-gated-tests.py $files > "$dir/$name.gated"
		echo "$name|$pkg|$pattern" >> "$dir/suites"
	done 3<<< "$all"
	# Flags as in internal/clitest/harness.go, so no managed policy applies.
	local m=$mod/internal/managed none=$dir/no-managed
	go build -buildvcs=false -o "$dir/abhed" -ldflags \
		"-X $m.ConfigFile=$none/config.json -X $m.AgentsDir=$none/agents -X $m.testDirEnv=ABHED_CLITEST_MANAGED_DIR" ./cmd/abhed
	# The clitest check reads go test -json; this makes it without a toolchain.
	go build -o "$dir/test2json" cmd/test2json
}

# prepare lets DIR's binaries make user namespaces and starts the user's
# systemd manager. It uses sudo and changes no sysctl.
prepare() {
	local dir=$1 bus
	# Ubuntu 24.04 grants user namespaces only to profiled binaries; this
	# profiles DIR alone, where the sysctl would lift it for every process.
	if [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo 0)" = 1 ]; then
		printf '%s\n' 'abi <abi/4.0>,' 'include <tunables/global>' \
			"profile abhed-fence-ci $dir/** flags=(unconfined) {" '  userns,' '}' |
			sudo tee /etc/apparmor.d/abhed-fence-ci > /dev/null
		sudo apparmor_parser -r /etc/apparmor.d/abhed-fence-ci
	fi
	# A service-started runner has no user manager until lingering starts one.
	sudo loginctl enable-linger "$(id -un)"
	bus=/run/user/$(id -u)/bus
	for _ in $(seq 50); do [ -S "$bus" ] && return 0; sleep 0.2; done
	echo "ENVIRONMENT: no user manager at $bus"; return 1
}

check() {
	local dir=$1 bad=0 abi fs
	echo "kernel: $(uname -srm)"
	if [ "$(id -u)" -eq 0 ]; then
		echo "ENVIRONMENT: running as root; the fence refuses root"; bad=1
	fi
	# landlock_create_ruleset(NULL, 0, VERSION) is syscall 444 on x86-64 and arm64.
	if ! abi=$(python3 -c 'import ctypes; l=ctypes.CDLL(None, use_errno=True); l.syscall.restype=ctypes.c_long; print(l.syscall(444, None, ctypes.c_size_t(0), ctypes.c_uint32(1)))' 2>&1); then
		echo "ENVIRONMENT: could not read the Landlock ABI: $abi"; abi=-1; bad=1
	fi
	echo "landlock ABI: $abi"
	# ABI 4 refuses TCP; below it the network-off tests skip, which fails the run.
	if [ "$abi" -lt 4 ]; then
		echo "ENVIRONMENT: Landlock ABI 4 (Linux 6.7) or later is needed (lsm: $(cat /sys/kernel/security/lsm 2>/dev/null || echo unknown))"; bad=1
	fi
	fs=$(stat -fc %T /sys/fs/cgroup)
	echo "cgroup: $fs"
	if [ "$fs" != cgroup2fs ]; then
		echo "ENVIRONMENT: the unified cgroup v2 hierarchy is needed"; bad=1
	fi
	# A user and a mount namespace from DIR, as the fence makes them.
	cp "$(command -v unshare)" "$dir/unshare-probe"
	if "$dir/unshare-probe" --user --map-root-user --mount true 2> "$dir/unshare.err"; then
		echo "user namespaces: an ordinary user can make one from $dir"
	else
		echo "ENVIRONMENT: an ordinary user cannot make a user namespace from $dir: $(cat "$dir/unshare.err")"; bad=1
	fi
	rm -f "$dir/unshare-probe" "$dir/unshare.err"
	return $bad
}

# Tests for other platforms, which skip on Linux by design.
off_linux=" TestDiscoverElsewhere TestRestrictRefusesOffLinux "

# passed_all NAME DIR GATED fails unless every listed test passed, or skipped
# off-Linux or pending on an item in FENCE_PENDING, and GATED is all listed.
passed_all() {
	local name=$1 dir=$2 gated=$3 n=0 bad=0 t item
	local log=$dir/$name.log
	while read -r t; do
		case $t in Test*) ;; *) continue ;; esac
		if grep -q -- "--- SKIP: $t/" "$log"; then
			echo "a subtest of $t skipped with ABHED_REQUIRE_FENCE=1:"; grep -- "--- SKIP: $t/" "$log"
			bad=1; continue
		fi
		if grep -q -- "--- PASS: $t " "$log"; then
			n=$((n + 1)); continue
		fi
		if grep -q -- "--- SKIP: $t " "$log"; then
			[[ $off_linux == *" $t "* ]] && continue
			item=$(sed -n "/=== RUN   $t\$/,/--- SKIP: $t /s/^ *[^ ]*\.go:[0-9]*: clitest: pending (\([a-z]*\)):.*/\1/p" "$log" | head -1)
			[[ $name == clitest && -n $item && " ${FENCE_PENDING:-} " == *" $item "* ]] && continue
		fi
		echo "$t did not run and pass with ABHED_REQUIRE_FENCE=1:"
		sed -n "/=== RUN   $t\$/,/--- [A-Z]*: $t /p" "$log"
		bad=1
	done < "$dir/$name.list"
	# A gated test the run pattern leaves out would never be checked.
	while read -r t; do
		[ -n "$t" ] || continue
		grep -qx -- "$t" "$dir/$name.list" || { echo "$t is gated on the fence but not in the run list"; bad=1; }
	done < "$gated"
	[ "$bad" -eq 0 ] || return 1
	[ "$n" -gt 0 ] || { echo "no test passed"; return 1; }
	echo "$n passed"
}

# run_suite NAME PKG PATTERN DIR GATED runs one binary in its package directory.
run_suite() {
	local name=$1 pkg=$2 pattern=$3 dir=$4 gated=$5
	local bin=$dir/$name.test rel=${pkg#"$mod"/}
	echo "== $rel -run '$pattern'"
	[ -x "$bin" ] || { echo "$bin is missing"; return 1; }
	(cd "$root/$rel" && "$bin" -test.list "$pattern" < /dev/null) > "$dir/$name.list"
	if [ "$name" = clitest ]; then
		# The hosted job's skip check, accepting only what FENCE_PENDING names.
		(cd "$root/$rel" && "$dir/test2json" -p "$pkg" "$bin" -test.v=test2json -test.count=1 -test.run "$pattern" < /dev/null) > "$dir/$name.json" || true
		python3 -c 'import json,sys
for l in sys.stdin:
    if l.startswith("{"): print(json.loads(l).get("Output", ""), end="")' < "$dir/$name.json" > "$dir/$name.log"
		ABHED_CLITEST_PENDING=${FENCE_PENDING:-} python3 "$root/scripts/ci/clitest-skips.py" < "$dir/$name.json" || return 1
	elif ! (cd "$root/$rel" && "$bin" -test.v -test.count=1 -test.run "$pattern" < /dev/null) > "$dir/$name.log" 2>&1; then
		grep -E -- "^(--- FAIL|FAIL|panic)" "$dir/$name.log" || true
		passed_all "$name" "$dir" "$gated" || true
		return 1
	fi
	passed_all "$name" "$dir" "$gated"
}

# in_scope fails unless this process is in a cgroup delegated to this user
# with the controllers the fence sets limits through.
in_scope() {
	local cg c
	cg=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)
	echo "scope: $cg"
	for c in memory pids; do
		grep -qw "$c" "$cg/cgroup.controllers" || { echo "ENVIRONMENT: the $c controller is not delegated to $cg"; return 1; }
	done
	[ -O "$cg/cgroup.procs" ] || { echo "ENVIRONMENT: $cg is not delegated to this user"; return 1; }
}

# run: every suite, offline. Only the network-on test may skip, as fencenet.
run() {
	local dir=$1 fail=0 ran=0 total name pkg pattern
	in_scope || return 1
	export ABHED_REQUIRE_FENCE=1 ABHED_CLITEST_BINARY=$dir/abhed FENCE_PENDING=fencenet
	unset ABHED_FENCE_NETWORK
	rm -f "$dir"/*.log
	while IFS='|' read -r -u 3 name pkg pattern; do
		run_suite "$name" "$pkg" "$pattern" "$dir" "$dir/$name.gated" || fail=1
		if [ -s "$dir/$name.log" ]; then ran=$((ran + 1)); fi
	done 3< "$dir/suites"
	total=$(grep -c . "$dir/suites")
	[ "$ran" -eq "$total" ] || { echo "only $ran of $total suites left a log"; fail=1; }
	return $fail
}

# run-network: the network-on test alone; it reaches PyPI and nodejs.org.
run_network() {
	local dir=$1
	in_scope || return 1
	export ABHED_REQUIRE_FENCE=1 ABHED_CLITEST_BINARY=$dir/abhed ABHED_FENCE_NETWORK=1 FENCE_PENDING=
	run_suite clitest "$mod/internal/clitest" '^TestFenceEndToEndNetworkOnInstalls$' "$dir" /dev/null
}

case ${1:-} in
build | prepare | check | run | run-network)
	[ -n "${2:-}" ] || { echo "usage: $0 $1 DIR" >&2; exit 2; }
	"${1//-/_}" "$2" ;;
*) echo "usage: $0 build|prepare|check|run|run-network DIR" >&2; exit 2 ;;
esac
