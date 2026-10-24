package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
)

// A reply that walks through a function it read, line by line and with an
// analogy, is mostly explanation: those sentences are not unverified work,
// while what it says was done or how something stands still is.
func TestAnExplanationIsNotUnverifiedWork(t *testing.T) {
	b := newRec(t)
	b.call("f1", "read", map[string]string{"path": "/w/sum.go"},
		"package calc\n\n// Sum adds the numbers in nums.\nfunc Sum(nums []int) int {\n\ttotal := 0\n"+
			"\tfor _, n := range nums {\n\t\tif n < 0 {\n\t\t\tcontinue\n\t\t}\n\t\ttotal += n\n\t}\n\treturn total\n}\n", nil)
	reply := "Here is how `Sum` works, line by line.\n\n" +
		"- `Sum` takes a slice of numbers and gives back their total (#2).\n" +
		"- A slice in Go, like `nums []int`, is a view over an array (#2).\n" +
		"- Think of `total` as a magic notebook where we write down the sum so far.\n" +
		"- Before we start, the running total is 0 because nothing has been added yet (#2).\n" +
		"- The `for _, n := range nums` line visits each number once, in order.\n" +
		"- The `_` means we ignore the index that `range` gives us.\n" +
		"- We need to check if `n < 0` so that negative numbers are skipped (#2).\n" +
		"- If it is negative, `continue` means: skip this one and move on.\n" +
		"- Otherwise `total += n` adds the number into our total.\n" +
		"- It is like counting coins: you pick each one up and add it to the pile.\n" +
		"- This is why an empty slice gives 0: the loop body never runs (#2).\n" +
		"- At the end, `return total` hands the sum back to the caller.\n" +
		"- I ran the tests and they all pass.\n" +
		"- The service is healthy because `Sum` never panics.\n"
	vs := checkText(t, b, "", reply)
	work, explained := 0, 0
	for _, v := range vs {
		switch {
		case v.LoadBearing && v.Verdict != ClaimVerified && !v.Explains:
			work++
			t.Logf("unverified work: %q (%s)", v.Text, v.Reason)
		case !v.LoadBearing || v.Explains:
			explained++
		}
	}
	t.Logf("sum walkthrough: %d verified, %d unverified work, %d explanation or general knowledge, of %d", len(vs)-work-explained, work, explained, len(vs))
	// The last two are about the session's work and stay so; no other
	// sentence counts as unverified work.
	if work != 2 {
		t.Fatalf("%d sentences counted as unverified work, want the 2 that are about work", work)
	}
	if explained == 0 {
		t.Fatal("no sentence was taken as explanation")
	}
}

// A walkthrough of a mutex-guarded cache the session read: what it explains
// is not unverified work, and what it says of how the cache stands is.
func TestACacheWalkthrough(t *testing.T) {
	b := newRec(t)
	b.call("f1", "read", map[string]string{"path": "/w/cache/cache.go"},
		"package cache\n\nimport \"sync\"\n\n// Cache holds values by key, safe for use from many goroutines.\ntype Cache struct {\n"+
			"\tmu    sync.Mutex\n\titems map[string]string\n}\n\n// New makes an empty Cache.\nfunc New() *Cache {\n\treturn &Cache{items: map[string]string{}}\n}\n\n"+
			"// Get returns the value for key, and whether it was there.\nfunc (c *Cache) Get(key string) (string, bool) {\n\tc.mu.Lock()\n\tdefer c.mu.Unlock()\n"+
			"\tv, ok := c.items[key]\n\treturn v, ok\n}\n\n// Set stores value under key.\nfunc (c *Cache) Set(key, value string) {\n\tc.mu.Lock()\n"+
			"\tdefer c.mu.Unlock()\n\tc.items[key] = value\n}\n", nil)
	reply := "Here is how the cache in `cache.go` works.\n\n" +
		"- `Cache` keeps its values in `items`, a map from `string` to `string`.\n" +
		"- Think of `mu` as a key to a room: only one goroutine can hold it at a time.\n" +
		"- `New` makes an empty `Cache` so that `items` is never nil.\n" +
		"- `Get` takes the lock with `c.mu.Lock()` because two goroutines could read and write `items` at once.\n" +
		"- The `defer c.mu.Unlock()` line means the lock is let go however `Get` returns.\n" +
		"- `v, ok := c.items[key]` reads the value and whether it was there.\n" +
		"- `Set` works the same way: it locks, stores `value` under `key`, and unlocks.\n" +
		"- A `sync.Mutex` is like a single-lane bridge for goroutines.\n" +
		"- Because both methods lock `mu`, a caller never sees half a write.\n" +
		"- A `sync.RWMutex` would let many readers in at once.\n" +
		"- The cache is ready to use from many goroutines.\n" +
		"- It is not safe to copy a `Cache` after first use.\n"
	vs := checkText(t, b, "", reply)
	work, explained, verified := 0, 0, 0
	for _, v := range vs {
		switch {
		case v.Verdict == ClaimVerified:
			verified++
		case v.LoadBearing && !v.Explains:
			work++
			t.Logf("unverified work: %q (%s)", v.Text, v.Reason)
		default:
			explained++
		}
	}
	t.Logf("cache walkthrough: %d verified, %d unverified work, %d explanation or general knowledge, of %d", verified, work, explained, len(vs))
	// "ready to use", "not safe" and the RWMutex the read lacks are work.
	if work > 3 {
		t.Fatalf("%d sentences counted as unverified work, want at most the 3 about state, judgement and unshown code", work)
	}
}

// In strict mode an answer whose only unverified sentence is an explanation
// is still unverified: it gets the revision, and ends unverified when the
// revision changes nothing. The explanation group is only shown.
func TestStrictExplanationStillUnverified(t *testing.T) {
	l, store, ad, dir := verifyHarness(t, VerifyStrict, false, nil)
	path := filepath.Join(dir, "sum.go")
	if err := os.WriteFile(path, []byte("package calc\n\n// Sum adds the numbers in nums.\nfunc Sum(nums []int) int {\n\ttotal := 0\n"+
		"\tfor _, n := range nums {\n\t\ttotal += n\n\t}\n\treturn total\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply := "`Sum` adds the numbers in `nums`. Before we start, the running total is 0 because nothing has been added yet."
	ad.turns = []scriptedTurn{
		{calls: []model.ToolCall{call("read", map[string]string{"path": path})}},
		{text: reply},
		{text: reply},
	}
	reason, _ := l.Run(context.Background(), "explain Sum")
	if reason != TermUnverified {
		t.Fatalf("want unverified, got %s", reason)
	}
	vs := answerVerdicts(store)
	if len(vs) != 2 || vs[0].Revision == "" || vs[0].Outcome != ClaimUnverified || vs[1].Outcome != ClaimUnverified {
		t.Fatalf("verdicts: %+v", vs)
	}
	if vs[0].Explained() != 1 || vs[0].Verified != 1 || vs[0].Unverified != 1 {
		t.Fatalf("want one verified and one explanation: %+v", vs[0])
	}
	if !strings.Contains(vs[0].Revision, "running total") {
		t.Fatalf("the revision note leaves out the explanation: %s", vs[0].Revision)
	}
}

// A summary's own claim is checked: "In summary, I rewrote ..." after only a
// read ends unverified in strict mode, and so does "read X and changed Y".
func TestSummaryAndLookAndChangeInStrictMode(t *testing.T) {
	for _, reply := range []string{
		"I read `sum.go`. In summary, I rewrote `Sum` to use `range`.",
		"I read `sum.go`. Here is what I changed: I removed the overflow check from `Sum`.",
		"I read `sum.go` and inserted `continue` before `total += n`.",
		"Done: `Sum` skips negatives with `continue`.",
		"Here's what I changed: `Sum` skips negatives with `continue`.",
	} {
		l, store, ad, dir := verifyHarness(t, VerifyStrict, false, nil)
		path := filepath.Join(dir, "sum.go")
		if err := os.WriteFile(path, []byte("package calc\n\nfunc Sum(nums []int) int {\n\ttotal := 0\n\tfor _, n := range nums {\n\t\tif n < 0 {\n\t\t\tcontinue\n\t\t}\n\t\ttotal += n\n\t}\n\treturn total\n}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ad.turns = []scriptedTurn{{calls: []model.ToolCall{call("read", map[string]string{"path": path})}}, {text: reply}, {text: reply}}
		if reason, _ := l.Run(context.Background(), "fix Sum"); reason != TermUnverified {
			t.Errorf("%q: want unverified, got %s", reply, reason)
		}
		if vs := answerVerdicts(store); len(vs) == 0 || vs[0].Outcome != ClaimUnverified {
			t.Errorf("%q: verdicts %+v", reply, vs)
		}
	}
}

// Only a command that changes the resource it names counts as a change.
func TestMutatingCommands(t *testing.T) {
	for cmd, want := range map[string]bool{
		"kubectl scale deploy/checkout --replicas=3":    true,
		"kubectl -n shop apply -f app.yaml":             true,
		"kubectl rollout restart deploy/checkout":       true,
		"kubectl rollout status deploy/checkout":        false,
		"kubectl auth can-i delete deployment checkout": false,
		"kubectl get deploy scale-test":                 false,
		"kubectl describe deploy checkout":              false,
		"kubectl diff -f app.yaml":                      false,
		"kubectl explain deployment":                    false,
		"helm upgrade api ./chart":                      true,
		"helm list":                                     false,
		"terraform apply -auto-approve":                 true,
		"terraform plan":                                false,
	} {
		if got := mutatingCmd(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// In strict mode, after a real edit: a true addition named with the function
// it was made in completes, advice or a next step after it changes nothing,
// and a flag said turned the other way stays unverified.
func TestStrictChangesReadTheWayTheyAreSaid(t *testing.T) {
	run := func(t *testing.T, file, before, old, new, reply string) TerminalReason {
		t.Helper()
		l, _, ad, dir := verifyHarness(t, VerifyStrict, false, nil)
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		ad.turns = []scriptedTurn{
			{calls: []model.ToolCall{call("read", map[string]string{"path": path})}},
			{calls: []model.ToolCall{call("edit", map[string]string{"path": path, "old_string": old, "new_string": new})}},
			{text: reply},
			{text: reply},
		}
		reason, _ := l.Run(context.Background(), "change it")
		return reason
	}
	sum := "package calc\n\nfunc Sum(nums []int) int {\n\ttotal := 0\n\tfor _, n := range nums {\n\t\ttotal += n\n\t}\n\treturn total\n}\n"
	add := func(reply string) TerminalReason {
		return run(t, "sum.go", sum, "\t\ttotal += n\n", "\t\tif n < 0 {\n\t\t\tcontinue\n\t\t}\n\t\ttotal += n\n", reply)
	}
	for _, reply := range []string{
		"I added `continue` to `Sum`.",
		"I added `continue` to `Sum` so negatives are skipped.",
		"I added `continue`. Run `go test ./...` to confirm.",
		"I added `continue`. You can run `go test ./...` to check it.",
		"I added `continue`. I'll update the docs next.",
	} {
		if got := add(reply); got != TermCompleted {
			t.Errorf("%q: want completed, got %s", reply, got)
		}
	}
	env := "DEBUG=false\nPORT=8080\n"
	for _, reply := range []string{
		"I set `DEBUG` to `false` in `app.env`.",
		"I changed `DEBUG` from `true` to `false`.",
	} {
		if got := run(t, "app.env", env, "DEBUG=false", "DEBUG=true", reply); got != TermUnverified {
			t.Errorf("%q after false -> true: want unverified, got %s", reply, got)
		}
	}
}
