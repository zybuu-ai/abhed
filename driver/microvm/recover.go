package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// markerName is the file in each VM's jail folder, beside its chroot, that
// says which driver made it: its state folder and the sandbox id.
const markerName = "abhed-microvm.json"

type marker struct {
	StateDir string `json:"state_dir"`
	ID       string `json:"id"`
}

func writeMarker(jail, stateDir, id string) error {
	b, err := json.Marshal(marker{StateDir: stateDir, ID: id})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(jail, markerName), b, 0o600)
}

// readMarker reads a jail's marker, refusing one the driver's user did not
// write: not a plain file, or owned by someone else.
func readMarker(jail string) (marker, error) {
	var m marker
	p := filepath.Join(jail, markerName)
	fi, err := os.Lstat(p)
	if err != nil {
		return m, err
	}
	if !fi.Mode().IsRegular() || !ownedByMe(fi) || fi.Size() > 4096 {
		return m, errors.New("not a marker this driver wrote")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// Recover is run once when a driver process starts. It takes the state
// folder's lock, which it holds until Close, so no two drivers share one;
// then it ends and removes what a driver of the same state folder left
// when it was killed: VMs, their cgroups and chroots, and instance state.
// Only what carries this state folder's marker is touched. It returns what
// it cleaned up.
func (d *Driver) Recover(ctx context.Context) ([]string, error) {
	if err := os.MkdirAll(d.opts.StateDir, 0o711); err != nil {
		return nil, err
	}
	unlock, err := lockDir(d.opts.StateDir)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.unlock = unlock
	d.mu.Unlock()
	cleaned, err := d.be.recover(ctx)
	entries, rerr := os.ReadDir(d.opts.StateDir)
	if rerr != nil {
		return cleaned, errors.Join(err, rerr)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if _, tracked := d.insts[name]; tracked || !e.IsDir() {
			continue
		}
		if _, verr := vmID(name); verr != nil {
			continue
		}
		if rerr := os.RemoveAll(filepath.Join(d.opts.StateDir, name)); rerr != nil {
			err = errors.Join(err, rerr)
			continue
		}
		cleaned = append(cleaned, "state of "+name)
	}
	return cleaned, err
}

// recover ends the VMs a killed driver of this state folder left running
// and removes their chroots and cgroups.
func (f *firecracker) recover(context.Context) ([]string, error) {
	base := filepath.Join(f.fc.ChrootBase, filepath.Base(f.fc.Binary))
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cleaned []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		jail := filepath.Join(base, name)
		fi, err := os.Lstat(jail)
		if err != nil || !fi.IsDir() || !ownedByMe(fi) {
			continue
		}
		m, err := readMarker(jail)
		if err != nil || m.StateDir != f.o.StateDir {
			continue
		}
		if want, err := vmID(m.ID); err != nil || want != name {
			continue
		}
		err = errors.Join(f.cgKill(f.fc.ParentCgroup, name), f.cgRemove(f.fc.ParentCgroup, name), os.RemoveAll(jail))
		if err != nil {
			errs = append(errs, fmt.Errorf("cleaning up %s: %w", m.ID, err))
			continue
		}
		cleaned = append(cleaned, "VM "+m.ID)
	}
	return cleaned, errors.Join(errs...)
}

// recover has nothing to end: the helper's VM stops when the helper does.
func (h *helper) recover(context.Context) ([]string, error) { return nil, nil }
