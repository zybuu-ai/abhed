package agent

import "testing"

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
		case v.LoadBearing && v.Verdict != ClaimVerified:
			work++
			t.Logf("unverified work: %q (%s)", v.Text, v.Reason)
		case !v.LoadBearing:
			explained++
		}
	}
	// The last two are about the session's work and stay so; no other
	// sentence counts as unverified work.
	if work != 2 {
		t.Fatalf("%d sentences counted as unverified work, want the 2 that are about work", work)
	}
	if explained == 0 {
		t.Fatal("no sentence was taken as explanation")
	}
}
