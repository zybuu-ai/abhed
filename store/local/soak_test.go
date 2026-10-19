package local

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// The local record's soak, as the Postgres one: ABHED_SOAK_SECONDS of
// sessions written through their Recorders, synced at a turn boundary every
// ABHED_SOAK_TURN events, then every line counted and every chain verified.
//
//	ABHED_SOAK_SECONDS=30 go test ./store/local -run TestLocalChainSoak -v
func TestLocalChainSoak(t *testing.T) {
	secs, _ := strconv.Atoi(os.Getenv("ABHED_SOAK_SECONDS"))
	if secs <= 0 {
		t.Skip("set ABHED_SOAK_SECONDS to run the soak")
	}
	sessions, turn := soakInt("ABHED_SOAK_SESSIONS", 32), soakInt("ABHED_SOAK_TURN", 20)
	s := openTest(t, t.TempDir())
	ids := make([]string, sessions)
	recorded := make([]int64, sessions)
	errs := make([]error, sessions)
	stop := time.Now().Add(time.Duration(secs) * time.Second)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range sessions {
		ids[i] = fmt.Sprintf("s-soak-%d", i)
		if err := s.CreateSession(context.Background(), store.SessionRecord{ID: ids[i], User: "tester", Workspace: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := agent.NewRecorder(s, ids[i], "")
			for n := 1; time.Now().Before(stop); n++ {
				if _, err := rec.Record(agent.EvAgentMessage, agent.ActorAgent, agent.Trusted,
					agent.Message{Text: fmt.Sprintf("event %d of session %d", n, i)}); err != nil {
					errs[i] = err
					return
				}
				recorded[i]++
				if n%turn == 0 {
					if err := s.Sync(ids[i]); err != nil {
						errs[i] = err
						return
					}
				}
			}
			errs[i] = s.Sync(ids[i])
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	var total, dropped int64
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("%s: %v", id, errs[i])
		}
		total += recorded[i]
		rep, err := s.Verify(id)
		if err != nil || !rep.OK {
			t.Fatalf("%s: %+v %v", id, rep, err)
		}
		if rep.Events != recorded[i] {
			dropped += recorded[i] - rep.Events
			t.Errorf("%s: %d appended, %d in the record", id, recorded[i], rep.Events)
		}
	}
	t.Logf("local soak: %d sessions, %d events in %s = %.0f events/s sustained, synced every %d; %d dropped, every chain verified",
		sessions, total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), turn, dropped)
}

func soakInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
