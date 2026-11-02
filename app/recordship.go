package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/orgpolicy"
	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/recordship"
	"github.com/zybuu-ai/abhed/store/local"
)

// shipStatePath is where the shipper keeps how far each session reached the
// org server: under ~/.abhed, which the agent cannot reach.
var shipStatePath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "state", "record-ship.json"), nil
}

// shipTarget is the org server the record goes to: the enrolled one.
type shipTarget struct {
	url, profile, profilesFile string
}

// shipTargetFor is the server record.ship sends to, or why there is none.
func shipTargetFor(cfg config.Config) (shipTarget, error) {
	if !cfg.Record.Ship.Enabled || !cfg.ManagedSets("record.ship") {
		return shipTarget{}, errShipOff
	}
	path, err := orgpolicy.Path()
	if err != nil {
		return shipTarget{}, err
	}
	st, err := orgpolicy.LoadState(path)
	if err != nil {
		return shipTarget{}, err
	}
	if st == nil {
		return shipTarget{}, errors.New("record.ship is on, but this machine is not signed in to an org server; the record is kept here and shipped once it is (abhed login URL)")
	}
	if u := cfg.Org.URL; u != "" && strings.TrimRight(u, "/") != st.URL {
		return shipTarget{}, fmt.Errorf("record.ship is on, but the org policy is taken only from %s and this machine is signed in to %s", u, st.URL)
	}
	pf, err := profiles.DefaultPath()
	if err != nil {
		return shipTarget{}, err
	}
	return shipTarget{url: st.URL, profile: st.Profile, profilesFile: pf}, nil
}

var errShipOff = errors.New("record.ship is off")

// doer sends requests to the target with its profile's token, keeping a
// refreshed token, and reading the profile again when the file changed.
func (t shipTarget) doer() recordship.Doer {
	var mu sync.Mutex
	return func(ctx context.Context, method, path, ctype string, body []byte) (int, []byte, error) {
		mu.Lock()
		defer mu.Unlock()
		reg, err := profiles.Load(t.profilesFile)
		if err != nil {
			return 0, nil, err
		}
		p, ok := reg.Profiles[t.profile]
		if !ok || p.URL != t.url {
			return 0, nil, fmt.Errorf("the profile %s for %s is gone; run abhed login %s", t.profile, t.url, t.url)
		}
		client, err := p.HTTPClient()
		if err != nil {
			return 0, nil, err
		}
		resp, err := p.Do(ctx, client, func() (*http.Request, error) {
			req, err := p.NewRequest(method, path, strings.NewReader(string(body)))
			if err == nil && ctype != "" {
				req.Header.Set("Content-Type", ctype)
			}
			return req, err
		}, func(q *profiles.Profile) error { return saveRefreshed(t.profilesFile, t.profile, q) })
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, b, err
	}
}

// withShipping hooks opts to ship the record when record.ship is on; start runs
// once it is open, each line nudges a pass, and closing makes a last one.
func withShipping(cfg config.Config, opts *local.Options) (start func(rec *local.Store)) {
	t, err := shipTargetFor(cfg)
	if errors.Is(err, errShipOff) {
		return func(*local.Store) {}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return func(*local.Store) {}
	}
	var sh *recordship.Shipper
	var shMu sync.Mutex
	get := func() *recordship.Shipper { shMu.Lock(); defer shMu.Unlock(); return sh }
	appendBefore := opts.OnAppend
	opts.OnAppend = func(l local.Sealed) {
		if appendBefore != nil {
			appendBefore(l)
		}
		if s := get(); s != nil {
			s.Nudge()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	closeBefore := opts.OnClose
	opts.OnClose = func() {
		cancel()
		if s := get(); s != nil {
			<-done
			last, stop := context.WithTimeout(context.Background(), shipCloseWait)
			if err := s.Pass(last); err != nil && !errors.Is(err, recordship.ErrBusy) {
				fmt.Fprintf(os.Stderr, "abhed: record.ship: %v; the rest is shipped next time\n", err)
			}
			stop()
		}
		if closeBefore != nil {
			closeBefore()
		}
	}
	redact := opts.Redact
	return func(rec *local.Store) {
		path, err := shipStatePath()
		if err == nil {
			var s *recordship.Shipper
			s, err = recordship.New(recordship.Options{Store: rec, Do: t.doer(), URL: t.url, StatePath: path,
				Redact: redact, MaxBacklog: cfg.Record.Ship.MaxBacklog(),
				OnEvent: func(e recordship.Event) {
					fmt.Fprintf(os.Stderr, "abhed: record.ship: %s%s: %s\n", e.Kind, sessionNote(e.Session), e.Detail)
				}})
			if err == nil {
				shMu.Lock()
				sh = s
				shMu.Unlock()
				go func() { defer close(done); s.Run(ctx, shipPoll) }()
				s.Nudge()
				return
			}
		}
		close(done)
		fmt.Fprintf(os.Stderr, "abhed: record.ship: %v\n", err)
	}
}

func sessionNote(id string) string {
	if id == "" {
		return ""
	}
	return " (session " + id + ")"
}

// How the shipper paces itself in a running Abhed.
var (
	shipPoll      = 15 * time.Second
	shipCloseWait = 5 * time.Second
)

// recordShip is `abhed record ship`: status, or a pass now.
func (c recordCtx) ship(args []string) int {
	fs := flag.NewFlagSet("record ship", flag.ContinueOnError)
	fs.SetOutput(c.err)
	if fs.Parse(args) != nil {
		return 2
	}
	verb := "status"
	if fs.NArg() > 0 {
		verb = fs.Arg(0)
	}
	path, err := shipStatePath()
	if err != nil {
		return c.fail("%v", err)
	}
	switch verb {
	case "status":
	case "now":
		t, err := shipTargetFor(c.cfg)
		if err != nil {
			return c.fail("%v", err)
		}
		s, err := recordship.New(recordship.Options{Store: c.rec, Do: t.doer(), URL: t.url, StatePath: path,
			Redact: openVault().Session(), MaxBacklog: c.cfg.Record.Ship.MaxBacklog()})
		if err != nil {
			return c.fail("%v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.Pass(ctx); err != nil {
			return c.fail("%v", err)
		}
	default:
		fmt.Fprintln(c.err, "usage: abhed record ship [status|now]")
		return 2
	}
	st, err := recordship.LoadState(path)
	if err != nil {
		return c.fail("%v", err)
	}
	on := c.cfg.Record.Ship.Enabled && c.cfg.ManagedSets("record.ship")
	fmt.Fprintf(c.out, "record.ship: %s\n", map[bool]string{true: "on", false: "off"}[on])
	if st == nil {
		fmt.Fprintln(c.out, "  nothing shipped yet")
		return 0
	}
	fmt.Fprintf(c.out, "  server:  %s\n  since:   %s (sessions started before are not shipped)\n  waiting: %d bytes\n",
		st.URL, st.Since.Local().Format("2 Jan 2006 15:04"), st.Backlog)
	if !st.LastOK.IsZero() {
		fmt.Fprintf(c.out, "  reached: %s\n", st.LastOK.Local().Format("2 Jan 15:04:05"))
	}
	if st.LastError != "" {
		fmt.Fprintf(c.out, "  failing: %s\n", st.LastError)
	}
	ids := make([]string, 0, len(st.Sessions))
	for id := range st.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cur := st.Sessions[id]
		line := fmt.Sprintf("  %s  %d line(s)", id, cur.Lines)
		if cur.Blocked != "" {
			line += "  stopped: " + cur.Blocked
		}
		fmt.Fprintln(c.out, line)
	}
	for _, e := range st.Events {
		fmt.Fprintf(c.out, "  event %s %s%s: %s\n", e.At.Local().Format("2 Jan 15:04"), e.Kind, sessionNote(e.Session), e.Detail)
	}
	return 0
}
