package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// choice is why Select chose a sandbox, kept on the sandbox for doctor and
// the session's capability note.
type choice struct{ why string }

// ChoiceReason is why this sandbox's tier was chosen; "" for one built
// directly rather than by Select.
func (c *choice) ChoiceReason() string { return c.why }

func (c *choice) setChoice(why string) { c.why = why }

// ChoiceReason is why Select chose sb, or "" when it did not.
func ChoiceReason(sb Sandbox) string {
	if c, ok := sb.(interface{ ChoiceReason() string }); ok {
		return c.ChoiceReason()
	}
	return ""
}

func noteChoice(sb Sandbox, why string) Sandbox {
	if c, ok := sb.(interface{ setChoice(string) }); ok {
		c.setChoice(why)
	}
	return sb
}

// BasicTools are the programs a person's own workspace is expected to have
// at hand: an image without git is not one to work in by default.
var BasicTools = []string{"git", "curl", "ping", "node", "python3"}

// imageLookWait bounds the look in an image, which runs a container.
var imageLookWait = 10 * time.Second

// ImageMissing reports which of programs the container's image lacks; tests
// replace it so no engine is asked.
var ImageMissing = func(c *Container, programs []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), imageLookWait)
	defer cancel()
	return c.MissingPrograms(ctx, programs)
}

// imageLook is what one look in an image found, a failure included, so a
// look that failed is not paid for again by every session.
type imageLook struct {
	missing []string
	err     error
}

// imageLooks keeps what each image was found to lack, per process: looking
// runs a container, which a podman machine pays for.
var imageLooks sync.Map // image -> []string

// MissingTools are the BasicTools the container tier's image lacks, and
// whether that is known: the default image is known to lack them all, and
// another is known once Select or doctor has looked in it.
func MissingTools(sb Sandbox) ([]string, bool) {
	if _, ok := sb.(*Container); !ok {
		return nil, false
	}
	img := SandboxImage()
	if img == DefaultImage {
		return slices.Clone(BasicTools), true
	}
	if v, ok := imageLooks.Load(img); ok && v.(imageLook).err == nil {
		return slices.Clone(v.(imageLook).missing), true
	}
	return nil, false
}

// lookImage is the BasicTools c's image lacks, asked once per image.
func lookImage(c *Container) ([]string, error) {
	img := SandboxImage()
	if v, ok := imageLooks.Load(img); ok {
		return v.(imageLook).missing, v.(imageLook).err
	}
	missing, err := ImageMissing(c, BasicTools)
	imageLooks.Store(img, imageLook{missing: missing, err: err})
	return missing, err
}

// lacksGit reports a container tier whose image has no git, with what else
// it lacks, which makes it no place for a person's own workspace when they
// did not ask for it; why is set when the image could not be looked in.
func lacksGit(c *Container) (lacks bool, missing []string, why string) {
	missing, err := lookImage(c)
	if err != nil {
		return false, nil, fmt.Sprintf("the image %s could not be looked in (%v), so the container tier is kept", SandboxImage(), err)
	}
	return slices.Contains(missing, "git"), missing, ""
}

// selectChosen builds the one tier the configuration chose, or refuses
// naming why: never another tier in its place.
func selectChosen(p Policy) (Sandbox, error) {
	var sb Sandbox
	switch p.Tier {
	case TierFence:
		if TierFence.Strength() < p.MinTier.Strength() {
			return nil, fmt.Errorf("sandbox.tier is fence, which counts as tier %q, weaker than the required minimum %q", TierProcess, p.MinTier)
		}
		f := NewFence(p)
		if ok, why := f.Available(); !ok {
			return nil, fmt.Errorf("sandbox.tier is fence, but commands cannot be fenced here, and no other tier is used in its place: %s", why)
		}
		return noteChoice(f, "chosen by sandbox.tier (fence)"), nil
	case TierProcess:
		sb = NewProcess(p)
	case TierContainer:
		sb = NewContainer(p)
	case TierVM:
		sb = NewGVisor(p)
	default:
		return nil, fmt.Errorf("sandbox.tier %q cannot be chosen; choose process, container, vm or fence", p.Tier)
	}
	if p.Tier.Strength() < p.MinTier.Strength() {
		return nil, fmt.Errorf("sandbox.tier is %s, weaker than the required minimum %q (sandbox.min_tier); choose %s or stronger", p.Tier, p.MinTier, p.MinTier)
	}
	// The proxy for registries where this tier can hold one, as Select does.
	if p.OptionalEgress != nil && p.Egress == nil {
		q := p
		q.Egress = p.OptionalEgress
		var withProxy Sandbox
		switch p.Tier {
		case TierProcess:
			withProxy = NewProcess(q)
		case TierContainer:
			withProxy = NewContainer(q)
		case TierVM:
			withProxy = NewGVisor(q)
		}
		if ok, _ := withProxy.Available(); ok {
			return noteChoice(withProxy, fmt.Sprintf("chosen by sandbox.tier (%s)", p.Tier)), nil
		}
	}
	if ok, why := sb.Available(); !ok {
		return nil, fmt.Errorf("sandbox.tier is %s, but that tier is not available here, and no other tier is used in its place: %s", p.Tier, why)
	}
	return noteChoice(sb, fmt.Sprintf("chosen by sandbox.tier (%s)", p.Tier)), nil
}

// ownToolsNote says why a container tier was passed over for the process tier.
func ownToolsNote(c *Container, missing []string) string {
	return fmt.Sprintf("process tier preferred for your own workspace: the %s tier's image %s has no %s "+
		"(set sandbox.tier to %s to use it anyway, or sandbox.image to an image that has them)",
		c.Tier(), SandboxImage(), strings.Join(missing, ", "), c.Tier())
}
