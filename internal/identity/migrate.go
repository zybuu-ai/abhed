package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/store/local"
)

// Moving the data from before sign-in into the first account. Each item
// moves by rename, which is atomic on one file system; a journal in the
// account's folder lists what is to move and what has, so a crash part way
// resumes at the next start. The record is verified before and after, and
// its heads must match; when they do not, everything moves back.

// MovedNote is the file left in ~/.abhed saying where the data went.
const MovedNote = "MOVED-TO-ACCOUNTS.txt"

const journalName = "migration.json"

// Journal states.
const (
	stateMoving   = "moving"
	stateComplete = "complete"
)

type journal struct {
	Account    string            `json:"account"`
	LegacyUser string            `json:"legacy_user"`
	State      string            `json:"state"`
	Items      []string          `json:"items"`
	Done       []string          `json:"done"`
	Heads      map[string]string `json:"heads"`
	At         time.Time         `json:"at"`
}

// MigrateResult says what moved.
type MigrateResult struct {
	Moved    int
	Sessions int
}

func (s *Store) journalPath(e Entry) string { return filepath.Join(s.dir, e.ID, journalName) }

// Migrate moves the data from before sign-in into e's folder. legacyUser is
// who the record wrote those sessions as, so they stay e's.
func Migrate(s *Store, e Entry, legacyUser string) (MigrateResult, error) {
	unlock, err := filelock.Lock(filepath.Join(s.dir, ".migrate.lock"), 0, "%s is held: another abhed is moving data (%s)")
	if err != nil {
		return MigrateResult{}, err
	}
	defer unlock()
	if owner, taken, err := s.LegacyOwner(); err != nil {
		return MigrateResult{}, err
	} else if taken && owner.ID != e.ID {
		return MigrateResult{}, fmt.Errorf("the data from before sign-in already belongs to %s", owner.Username)
	}
	m, err := home.Machine()
	if err != nil {
		return MigrateResult{}, err
	}
	root, err := s.Root(e)
	if err != nil {
		return MigrateResult{}, err
	}
	var j journal
	if ok, err := readJSON(s.journalPath(e), &j); err != nil {
		return MigrateResult{}, err
	} else if !ok || j.State == stateComplete {
		j = journal{Account: e.ID, LegacyUser: legacyUser, State: stateMoving, At: time.Now().UTC()}
		for _, it := range LegacyItems {
			if _, err := os.Lstat(filepath.Join(m, it)); err == nil {
				j.Items = append(j.Items, it)
			}
		}
	}
	release, err := holdSessions(filepath.Join(m, "records"))
	if err != nil {
		return MigrateResult{}, err
	}
	defer release()
	if j.Heads == nil {
		if j.Heads, err = recordHeads(filepath.Join(m, "records")); err != nil {
			return MigrateResult{}, fmt.Errorf("verify the record before moving it: %w", err)
		}
		if err := writeJSON(s.journalPath(e), j); err != nil {
			return MigrateResult{}, err
		}
	}
	for _, it := range j.Items {
		if slices.Contains(j.Done, it) {
			continue
		}
		from, to := filepath.Join(m, it), filepath.Join(root, it)
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			// Moved before a crash and not yet noted, or gone.
			if _, err := os.Lstat(to); err == nil {
				j.Done = append(j.Done, it)
			}
			continue
		}
		if _, err := os.Lstat(to); err == nil {
			return MigrateResult{}, fmt.Errorf("%s is already in the account; nothing more was moved", to)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return MigrateResult{}, err
		}
		if err := os.Rename(from, to); err != nil {
			return MigrateResult{}, fmt.Errorf("move %s: %w", it, err)
		}
		j.Done = append(j.Done, it)
		if err := writeJSON(s.journalPath(e), j); err != nil {
			return MigrateResult{}, err
		}
	}
	after, err := recordHeads(filepath.Join(root, "records"))
	if err == nil && !sameHeads(j.Heads, after) {
		err = fmt.Errorf("the record's heads differ after the move: %s", diffHeads(j.Heads, after))
	}
	if err != nil {
		if _, rerr := moveBack(s, e, j); rerr != nil {
			return MigrateResult{}, fmt.Errorf("%w; moving back also failed: %v", err, rerr)
		}
		return MigrateResult{}, fmt.Errorf("%w; everything was moved back", err)
	}
	if err := s.SetLegacyUser(e.ID, legacyUser); err != nil {
		return MigrateResult{}, err
	}
	j.State = stateComplete
	if err := writeJSON(s.journalPath(e), j); err != nil {
		return MigrateResult{}, err
	}
	note := fmt.Sprintf("Since Abhed 1.3.0 your sessions, history, memory, secrets and sign-ins live in\nthe account %s, in %s.\nabhed account unmigrate moves them back, before going back to an earlier version.\n", e.Username, root)
	_ = os.WriteFile(filepath.Join(m, MovedNote), []byte(note), 0o600)
	return MigrateResult{Moved: len(j.Done), Sessions: len(j.Heads)}, nil
}

// ResumeMigration finishes a move a crash interrupted, if there is one.
func ResumeMigration(s *Store) (bool, error) {
	list, err := s.List()
	if err != nil {
		return false, err
	}
	for _, e := range list {
		var j journal
		if ok, err := readJSON(s.journalPath(e), &j); err != nil || !ok || j.State != stateMoving {
			continue
		}
		_, err := Migrate(s, e, j.LegacyUser)
		return true, err
	}
	return false, nil
}

// Unmigrate moves the data back to ~/.abhed, for going back to an earlier
// version. It returns how many items moved.
func Unmigrate(s *Store) (int, error) {
	unlock, err := filelock.Lock(filepath.Join(s.dir, ".migrate.lock"), 0, "%s is held: another abhed is moving data (%s)")
	if err != nil {
		return 0, err
	}
	defer unlock()
	e, taken, err := s.LegacyOwner()
	if err != nil {
		return 0, err
	}
	if !taken {
		return 0, errors.New("no account holds data from before sign-in")
	}
	var j journal
	if ok, err := readJSON(s.journalPath(e), &j); err != nil {
		return 0, err
	} else if !ok {
		return 0, errors.New("the account has no record of what moved")
	}
	root, err := s.Root(e)
	if err != nil {
		return 0, err
	}
	release, err := holdSessions(filepath.Join(root, "records"))
	if err != nil {
		return 0, err
	}
	defer release()
	n, err := moveBack(s, e, j)
	if err != nil {
		return n, err
	}
	if err := s.SetLegacyUser(e.ID, ""); err != nil {
		return n, err
	}
	_ = os.Remove(s.journalPath(e))
	if m, err := home.Machine(); err == nil {
		_ = os.Remove(filepath.Join(m, MovedNote))
	}
	return n, nil
}

func moveBack(s *Store, e Entry, j journal) (int, error) {
	m, err := home.Machine()
	if err != nil {
		return 0, err
	}
	root := filepath.Join(s.dir, e.ID)
	n := 0
	for _, it := range j.Done {
		from, to := filepath.Join(root, it), filepath.Join(m, it)
		if _, err := os.Lstat(from); err != nil {
			continue
		}
		if _, err := os.Lstat(to); err == nil {
			return n, fmt.Errorf("%s is in the way; nothing more was moved back", to)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return n, err
		}
		if err := os.Rename(from, to); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// holdSessions takes every session's writer lock under the records folder,
// so no other Abhed writes while the record moves.
func holdSessions(records string) (func(), error) {
	locks, _ := filepath.Glob(filepath.Join(records, "*", "locks", "*.lock"))
	var held []func()
	release := func() {
		for _, u := range held {
			u()
		}
	}
	for _, l := range locks {
		u, err := filelock.Lock(l, 0, "%s is held (%s)")
		if err != nil {
			release()
			id := strings.TrimSuffix(filepath.Base(l), ".lock")
			return nil, fmt.Errorf("another abhed is writing session %s; end it, then start again", id)
		}
		held = append(held, u)
	}
	return release, nil
}

// recordHeads verifies every session in every tenant of the records folder
// and returns, per session, whether it verified and its head.
func recordHeads(records string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(records)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, t := range entries {
		if !t.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(records, t.Name(), "index.jsonl")); err != nil {
			continue
		}
		st, err := local.Open(local.Options{Dir: records, Tenant: t.Name(), User: "migration"})
		if err != nil {
			return nil, err
		}
		idx, reps, err := st.VerifyAll()
		_ = st.Close()
		if err != nil {
			return nil, err
		}
		out[t.Name()+"/index"] = fmt.Sprintf("%v %d %s", idx.OK, idx.Head.Seq, idx.Head.Hash)
		for _, r := range reps {
			out[t.Name()+"/"+r.ID] = fmt.Sprintf("%v %d %s", r.OK, r.Head.Seq, r.Head.Hash)
		}
	}
	return out, nil
}

func sameHeads(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func diffHeads(a, b map[string]string) string {
	var out []string
	for k, v := range a {
		if b[k] != v {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > 5 {
		out = append(out[:5], "…")
	}
	return strings.Join(out, ", ")
}
