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
	unlock, err := s.lockMigration(0)
	if err != nil {
		return MigrateResult{}, err
	}
	defer unlock()
	return migrateLocked(s, e, legacyUser)
}

// ErrMigrationBusy is another Abhed moving the data from before sign-in.
var ErrMigrationBusy = errors.New("another abhed is moving your earlier data into an account; start again once it has finished")

func (s *Store) lockMigration(wait time.Duration) (func(), error) {
	unlock, err := filelock.Lock(filepath.Join(s.dir, ".migrate.lock"), wait, "%s is held (%s)")
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrMigrationBusy, err)
	}
	return unlock, nil
}

// migrateStep is told each step of a move, so tests can stop it there as a
// crash would; an error ends the move at once, leaving what a crash leaves.
var migrateStep = func(string) error { return nil }

// validJournal refuses a journal that names anything but the items this
// release moves, or another account.
func validJournal(j journal, e Entry) error {
	if j.Account != e.ID {
		return fmt.Errorf("the move's journal names another account")
	}
	if !plainLegacyUser(j.LegacyUser) {
		return fmt.Errorf("the move's journal names %q as the earlier data's user, which no earlier version recorded as", j.LegacyUser)
	}
	for _, list := range [][]string{j.Items, j.Done} {
		for _, it := range list {
			if !slices.Contains(LegacyItems, it) {
				return fmt.Errorf("the move's journal names %q, which is not data from before sign-in", it)
			}
		}
	}
	return nil
}

// plainLegacyUser reports whether u is a user an earlier version recorded
// sessions as: $USER as it took it, never one shaped like an owner the store
// gives a meaning (a namespace, agent, anonymous, guest).
func plainLegacyUser(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	return l != "" && !strings.Contains(l, ":") && l != "agent" && l != "anonymous" && l != "guest"
}

// recordUsers are the users the sessions in a records folder were recorded
// as, subagents' aside.
func recordUsers(records string) map[string]bool {
	out := map[string]bool{}
	tenants, _ := os.ReadDir(records)
	for _, t := range tenants {
		if !t.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(records, t.Name(), "index.jsonl")); err != nil {
			continue
		}
		st, err := local.Open(local.Options{Dir: records, Tenant: t.Name(), User: "migration"})
		if err != nil {
			continue
		}
		entries, _ := st.Index().List(local.Filter{All: true})
		for _, e := range entries {
			if e.User != "" && e.User != "agent" {
				out[e.User] = true
			}
		}
		_ = st.Close()
	}
	return out
}

// safeParent makes the folders above rel under root, refusing any that is a
// link, so a planted link never takes the move elsewhere.
func safeParent(root, rel string) error {
	dir := root
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
			return fmt.Errorf("%s is a link or not a folder; nothing is moved into it", dir)
		}
	}
	return nil
}

// emptyDir reports whether p is a folder with nothing in it.
func emptyDir(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) == 0
}

func migrateLocked(s *Store, e Entry, legacyUser string) (MigrateResult, error) {
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
	} else if ok && j.State == stateMoving {
		if err := validJournal(j, e); err != nil {
			return MigrateResult{}, fmt.Errorf("%w; it is %s", err, s.journalPath(e))
		}
		// The user the earlier data was recorded as is the data's, not the journal's say.
		users := recordUsers(filepath.Join(m, "records"))
		for u := range recordUsers(filepath.Join(root, "records")) {
			users[u] = true
		}
		if len(users) > 0 && !users[j.LegacyUser] && !users["local:"+strings.ToLower(j.LegacyUser)] {
			return MigrateResult{}, fmt.Errorf("the move's journal names %q, who recorded none of the earlier sessions; it is %s", j.LegacyUser, s.journalPath(e))
		}
	} else {
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
		if err := migrateStep("journal"); err != nil {
			return MigrateResult{}, err
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
			// Something made it since (an empty folder a run created): take its
			// place; anything else in the way moves everything back.
			if emptyDir(to) {
				_ = os.Remove(to)
			} else {
				if _, rerr := moveBack(s, e, j); rerr != nil {
					return MigrateResult{}, fmt.Errorf("%s is already in the account, and moving back failed: %v", to, rerr)
				}
				_ = os.Remove(s.journalPath(e))
				return MigrateResult{}, fmt.Errorf("%s is already in the account, so nothing was moved: everything is back in ~/.abhed; move or remove it, then run abhed account adopt", to)
			}
		}
		if err := safeParent(root, it); err != nil {
			if _, rerr := moveBack(s, e, j); rerr != nil {
				return MigrateResult{}, fmt.Errorf("%w, and moving back failed: %v", err, rerr)
			}
			_ = os.Remove(s.journalPath(e))
			return MigrateResult{}, fmt.Errorf("%w: everything is back in ~/.abhed", err)
		}
		if err := os.Rename(from, to); err != nil {
			return MigrateResult{}, fmt.Errorf("move %s: %w", it, err)
		}
		if err := migrateStep("renamed:" + it); err != nil {
			return MigrateResult{}, err
		}
		j.Done = append(j.Done, it)
		if err := writeJSON(s.journalPath(e), j); err != nil {
			return MigrateResult{}, err
		}
		if err := migrateStep("noted:" + it); err != nil {
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
	if err := migrateStep("legacy"); err != nil {
		return MigrateResult{}, err
	}
	if err := s.SetLegacyUser(e.ID, j.LegacyUser); err != nil {
		return MigrateResult{}, err
	}
	if err := migrateStep("complete"); err != nil {
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
	unlock, err := s.lockMigration(0)
	if err != nil {
		return false, err
	}
	defer unlock()
	return resumeLocked(s)
}

// SettleMigration makes sure no move of the data from before sign-in is in
// progress before a start resolves who is working: it waits up to wait for
// one another Abhed is making, and finishes one a crash left. A start never
// runs on half the data; it gets ErrMigrationBusy instead.
func SettleMigration(s *Store, wait time.Duration) (bool, error) {
	unlock, err := s.lockMigration(wait)
	if err != nil {
		return false, err
	}
	defer unlock()
	return resumeLocked(s)
}

func resumeLocked(s *Store) (bool, error) {
	list, err := s.List()
	if err != nil {
		return false, err
	}
	for _, e := range list {
		var j journal
		if ok, err := readJSON(s.journalPath(e), &j); err != nil || !ok || j.State != stateMoving {
			continue
		}
		_, err := migrateLocked(s, e, j.LegacyUser)
		return true, err
	}
	return false, nil
}

// Unmigrate moves the data back to ~/.abhed, for going back to an earlier
// version. It returns how many items moved.
func Unmigrate(s *Store) (int, error) {
	unlock, err := s.lockMigration(0)
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
	if err := validJournal(j, e); err != nil {
		return 0, err
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
	before, err := recordHeads(filepath.Join(root, "records"))
	if err != nil {
		return 0, fmt.Errorf("verify the record before moving it back: %w", err)
	}
	n, err := moveBack(s, e, j)
	if err != nil {
		return n, err
	}
	m, err := home.Machine()
	if err != nil {
		return n, err
	}
	if after, err := recordHeads(filepath.Join(m, "records")); err != nil || !sameHeads(before, after) {
		return n, fmt.Errorf("the record's heads differ after moving it back (%v): %s; it is in ~/.abhed/records, check it with abhed record verify", err, diffHeads(before, after))
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
		if err := safeParent(m, it); err != nil {
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
