#!/usr/bin/env bash
# The fence tier's gated tests, run for real: every test that skips without
# ABHED_REQUIRE_FENCE=1 runs here with it set, so a skip is a failure.
#
#   fence-gated.sh build DIR   test binaries and the abhed binary into DIR
#   fence-gated.sh check DIR   the kernel and user qualify (Landlock, cgroup v2, user namespaces)
#   fence-gated.sh run DIR     inside a delegated scope: every listed test must run and pass
#
# The binaries are built once and run from DIR, because Ubuntu grants a user
# namespace by the path of the binary that asks for it (see ci.yml).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
mod=github.com/zybuu-ai/abhed

# name|package|-run pattern. Every fence package runs whole, so a new one
# (such as mountns) is covered when it lands; the rest run their fence tests.
suites() {
	(cd "$root" && go list ./internal/fence/...) | while read -r pkg; do
		echo "fence-${pkg##*/}|$pkg|."
	done
	echo "sandbox|$mod/internal/sandbox|Fence|Mounts|Launch|Select"
	echo "clitest|$mod/internal/clitest|Fence"
}

build() {
	local dir=$1
	mkdir -p "$dir"
	cd "$root"
	suites > "$dir/suites"
	while IFS='|' read -r name pkg _; do
		# A package with no test files builds no binary, and has nothing to run.
		go test -c -o "$dir/$name.test" "$pkg"
	done < "$dir/suites"
	# The clitest harness runs this in place of building its own; the flags
	# must match internal/clitest/harness.go so no managed policy applies.
	local m=$mod/internal/managed none=$dir/no-managed
	go build -buildvcs=false -o "$dir/abhed" -ldflags \
		"-X $m.ConfigFile=$none/config.json -X $m.AgentsDir=$none/agents -X $m.testDirEnv=ABHED_CLITEST_MANAGED_DIR" ./cmd/abhed
	# The clitest check reads go test -json; this makes it without a toolchain.
	go build -o "$dir/test2json" cmd/test2json
}

check() {
	local dir=$1 bad=0
	echo "kernel: $(uname -srm)"
	if [ "$(id -u)" -eq 0 ]; then
		echo "ENVIRONMENT: running as root; the fence refuses root"; bad=1
	fi
	# landlock_create_ruleset(NULL, 0, VERSION) is syscall 444 on every
	# architecture the fence ships for, and returns the ABI.
	local abi
	abi=$(python3 -c 'import ctypes; l=ctypes.CDLL(None, use_errno=True); l.syscall.restype=ctypes.c_long; print(l.syscall(444, None, ctypes.c_size_t(0), ctypes.c_uint32(1)))')
	echo "landlock ABI: $abi"
	if [ "$abi" -lt 3 ]; then
		echo "ENVIRONMENT: Landlock ABI 3 or later is needed (lsm: $(cat /sys/kernel/security/lsm 2>/dev/null || echo unknown))"; bad=1
	fi
	local fs
	fs=$(stat -fc %T /sys/fs/cgroup)
	echo "cgroup: $fs"
	if [ "$fs" != cgroup2fs ]; then
		echo "ENVIRONMENT: the unified cgroup v2 hierarchy is needed"; bad=1
	fi
	# A user and a mount namespace together, from DIR, is what the fence asks
	# for; a restriction that allows the namespace but no mount in it fails here.
	cp "$(command -v unshare)" "$dir/unshare-probe"
	if "$dir/unshare-probe" --user --map-root-user --mount true 2> "$dir/unshare.err"; then
		echo "user namespaces: an ordinary user can make one from $dir"
	else
		echo "ENVIRONMENT: an ordinary user cannot make a user namespace from $dir: $(cat "$dir/unshare.err")"; bad=1
	fi
	rm -f "$dir/unshare-probe" "$dir/unshare.err"
	return $bad
}

# Tests for other platforms, which skip on Linux by design; Linux's own
# tests cover the same ground. Nothing else may skip.
off_linux=" TestDiscoverElsewhere TestRestrictRefusesOffLinux "

# passed_all LOG LIST fails unless every test named in LIST passed in LOG,
# or skipped as a test for another platform.
passed_all() {
	local log=$1 list=$2 n=0 bad=0
	while read -r t; do
		case $t in Test*) ;; *) continue ;; esac
		if grep -q -- "--- PASS: $t " "$log"; then
			n=$((n + 1))
		elif [[ $off_linux == *" $t "* ]] && grep -q -- "--- SKIP: $t " "$log"; then
			:
		else
			echo "$t did not run and pass with ABHED_REQUIRE_FENCE=1:"
			sed -n "/=== RUN   $t\$/,/--- [A-Z]*: $t /p" "$log"
			bad=1
		fi
	done < "$list"
	[ "$bad" -eq 0 ] || return 1
	[ "$n" -gt 0 ] || { echo "no test matched"; return 1; }
	echo "$n passed"
}

run() {
	local dir=$1 fail=0
	# The delegated scope is what the cgroup tests and the fence itself need;
	# a run outside one would fail them, but name the cause first.
	local cg
	cg=/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)
	echo "scope: $cg"
	for c in memory pids; do
		grep -qw "$c" "$cg/cgroup.controllers" || { echo "ENVIRONMENT: the $c controller is not delegated to $cg"; return 1; }
	done
	[ -O "$cg/cgroup.procs" ] || { echo "ENVIRONMENT: $cg is not delegated to this user"; return 1; }
	export ABHED_REQUIRE_FENCE=1 ABHED_CLITEST_BINARY=$dir/abhed
	# The network-on test installs from PyPI and nodejs.org, which the runner reaches.
	export ABHED_FENCE_NETWORK=1
	while IFS='|' read -r name pkg pattern; do
		local bin=$dir/$name.test
		[ -e "$bin" ] || continue
		local rel=${pkg#"$mod"/}
		echo "== $rel -run '$pattern'"
		(cd "$root/$rel" && "$bin" -test.list "$pattern") > "$dir/$name.list"
		if [ "$name" = clitest ]; then
			# Through the same skip check as the hosted job, with nothing pending.
			(cd "$root/$rel" && "$dir/test2json" -p "$pkg" "$bin" -test.v=test2json -test.count=1 -test.run "$pattern") > "$dir/$name.json" || true
			python3 -c 'import json,sys
for l in sys.stdin:
    if l.startswith("{"): print(json.loads(l).get("Output", ""), end="")' < "$dir/$name.json" > "$dir/$name.log"
			ABHED_CLITEST_PENDING="" python3 "$root/scripts/ci/clitest-skips.py" < "$dir/$name.json" || fail=1
		else
			(cd "$root/$rel" && "$bin" -test.v -test.count=1 -test.run "$pattern") > "$dir/$name.log" 2>&1 || { grep -E -- "^(--- FAIL|FAIL|panic)" "$dir/$name.log" || true; fail=1; }
		fi
		passed_all "$dir/$name.log" "$dir/$name.list" || fail=1
	done < "$dir/suites"
	return $fail
}

case ${1:-} in
build | check | run) [ -n "${2:-}" ] || { echo "usage: $0 $1 DIR" >&2; exit 2; }; "$1" "$2" ;;
*) echo "usage: $0 build|check|run DIR" >&2; exit 2 ;;
esac
