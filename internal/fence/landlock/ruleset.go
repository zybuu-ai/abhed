package landlock

import (
	"fmt"
	"os"
	"strings"
)

// Ruleset is a validated Spec bound to the ABI it was checked against. Build
// it with New; apply it with Restrict.
type Ruleset struct {
	spec Spec
	abi  ABI
}

// New validates spec for abi and refuses, wrapping ErrUnsupported, ErrRoot
// or ErrSpec, when it cannot be applied in full.
func New(spec Spec, abi ABI) (*Ruleset, error) {
	return newRuleset(spec, abi, os.Getuid(), os.Geteuid())
}

func newRuleset(spec Spec, abi ABI, uid, euid int) (*Ruleset, error) {
	if err := refuseRoot(uid, euid); err != nil {
		return nil, err
	}
	if err := abi.Check(); err != nil {
		return nil, err
	}
	if spec.DenyTCP && !abi.Network() {
		return nil, fmt.Errorf("%w: refusing TCP needs Landlock %s or later, and this kernel's is ABI %d",
			ErrUnsupported, networkABI.named(), int(abi))
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &Ruleset{spec: spec, abi: abi}, nil
}

// refuseRoot refuses a real or effective uid of 0: root can leave the domain
// by other means, and the fence does not yet drop to another user.
func refuseRoot(uid, euid int) error {
	if uid == 0 || euid == 0 {
		return fmt.Errorf("%w: the fence confines commands only for a user other than root", ErrRoot)
	}
	return nil
}

// ABI is the ABI the ruleset was validated against and applies.
func (r *Ruleset) ABI() ABI { return r.abi }

// Describe is the one-line account of the domain for abhed doctor and the
// record.
func (r *Ruleset) Describe() string {
	var parts []string
	add := func(label string, paths []string) {
		if len(paths) > 0 {
			parts = append(parts, label+" "+strings.Join(paths, ", "))
		}
	}
	add("run and read", r.spec.Exec)
	add("read", r.spec.Read)
	add("read and write", r.spec.Write)
	add("devices", r.spec.Devices)
	add("denied", r.spec.Deny)
	if r.spec.DenyTCP {
		parts = append(parts, "TCP connect and bind refused")
	} else {
		parts = append(parts, "TCP not restricted by Landlock")
	}
	if r.abi.scopes() != 0 {
		parts = append(parts, "signals and abstract sockets scoped")
	}
	return fmt.Sprintf("Landlock ABI %d allowlist: %s", int(r.abi), strings.Join(parts, "; "))
}
