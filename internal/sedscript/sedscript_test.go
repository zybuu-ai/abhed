package sedscript

import "testing"

func TestParse(t *testing.T) {
	inert := []string{
		"s/x/y/", "s/rm -rf build/make clean/", "s|a|b|g", "1,20p", "/dropdb/d", "$d",
		"s/x/y/; s/a/b/2", "/^#/!d", "0~2d", "s/\\/e/x/g", "y/abc/xyz/", ":a;N;$!ba;s/\\n/ /g",
		"$a\\\nrm -rf /srv/data", "1i hello", "/x/{s/a/b/;p}", "s/a/b/I", "#n\np",
		"/start/,/end/d", "5q", "l 40", "r other.txt",
	}
	for _, src := range inert {
		s, err := Parse(src)
		if err != nil || !s.Inert() {
			t.Errorf("%q: want inert, got %+v err=%v", src, s, err)
		}
	}
	runs := []string{
		"1e rm -rf /srv/data", "s/.*/rm -rf \\/srv\\/data/e", "s/x/y/; 1e rm -rf /srv/data", "e",
		"$e date", "/x/e", "s/a/b/ge", "s/foo/bar/w /srv/data/app.db", "w out", "1!e x", "/a/{e x\n}",
	}
	for _, src := range runs {
		s, err := Parse(src)
		if err == nil && s.Inert() {
			t.Errorf("%q: want not inert", src)
		}
	}
	for _, src := range []string{"s/x/y", "k", "s/x/y/q", "/x"} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%q: want error", src)
		}
	}
}

func TestScripts(t *testing.T) {
	cases := []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"-i", "s/x/y/", "f"}, "s/x/y/", true},
		{[]string{"-n", "1e rm", "f"}, "1e rm", true},
		{[]string{"-e", "a", "-e", "b", "f"}, "a\nb", true},
		{[]string{"-ne", "p", "f"}, "p", true},
		{[]string{"-i.bak", "s/e/f/", "f"}, "s/e/f/", true},
		{[]string{"--expression=1e x", "f"}, "1e x", true},
		{[]string{"-f", "x.sed", "f"}, "", false},
		{[]string{"-nf", "x.sed"}, "", false},
	}
	for _, c := range cases {
		got, ok := Scripts(c.args)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v want %q %v", c.args, got, ok, c.want, c.ok)
		}
	}
}
