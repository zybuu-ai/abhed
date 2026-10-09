package archive

import "testing"

func TestLink(t *testing.T) {
	for target, ok := range map[string]bool{
		"b": true, "sub/../b": true, "../x": true, "../../x": false, "/etc": false, "": false,
		".abhed": false, "sub/../.ABHED/config.json": false, "a/.abhed/x": false,
	} {
		if err := Link("a/l", target); (err == nil) != ok {
			t.Errorf("Link(a/l, %q): %v", target, err)
		}
	}
}
