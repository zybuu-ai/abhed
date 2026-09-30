package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/hawkeye"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// parseEvents reads a record as /export writes it, one JSON array, or as
// -output-format json streams it, one event per line.
func parseEvents(data []byte) ([]agent.Event, error) {
	var events []agent.Event
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		return events, json.Unmarshal(trimmed, &events)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var ev agent.Event
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, err
		}
		if ev.Type == "" || ev.SessionID == "" {
			return nil, fmt.Errorf("event %d has no type or session", len(events)+1)
		}
		events = append(events, ev)
	}
}

// hawkeyeCmd reports on a finished session: from an exported events file, or
// by id from the durable store. It never needs a model or a network.
func hawkeyeCmd(workspace string, args []string, trust config.TrustChoice) int {
	fl := flag.NewFlagSet("hawkeye", flag.ExitOnError)
	out := fl.String("o", "", "write the report here (.html or .json); the summary still prints")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: abhed hawkeye [-o report.html] <events.json | session-id>")
	}
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		fl.Usage()
		return 2
	}
	target := fl.Arg(0)

	var events []agent.Event
	id := target
	if data, err := os.ReadFile(target); err == nil { //nolint:gosec // the operator names the file
		if events, err = parseEvents(data); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %s is not an events file (an /export array or -output-format json lines): %v\n", target, err)
			return 1
		}
		if len(events) > 0 {
			id = events[0].SessionID
		}
	} else {
		cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
		if err != nil {
			fail(err)
		}
		if cfg.Storage.Driver != "postgres" {
			fmt.Fprintf(os.Stderr, "abhed: %q is not a file, and there is no durable store to look it up in.\n"+
				"Export a session with /export session.json, or configure storage.driver.\n", target)
			return 1
		}
		st, closeStore, err := openStore(context.Background(), cfg)
		if err != nil {
			fail(err)
		}
		defer closeStore()
		if events, err = st.Events(target); err != nil {
			fail(err)
		}
	}
	if len(events) == 0 {
		fmt.Fprintf(os.Stderr, "abhed: no events for %s\n", target)
		return 1
	}

	rep := hawkeye.Analyze(id, events)
	fmt.Print(hawkeye.Text(rep))
	if *out != "" {
		if err := writeHawkeye(*out, rep); err != nil {
			fail(err)
		}
		fmt.Printf("\n  wrote %s\n", *out)
	}
	// A record with holes in it is an exit code a pipeline can act on.
	for _, f := range rep.Findings {
		if f.Severity == hawkeye.Critical {
			return 3
		}
	}
	return 0
}

func writeHawkeye(path string, rep hawkeye.Report) error {
	var data []byte
	var err error
	if strings.HasSuffix(path, ".json") {
		data, err = json.MarshalIndent(rep, "", "  ")
	} else {
		var page string
		page, err = hawkeye.HTML(rep)
		data = []byte(page)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600) // #nosec G703 -- the operator's -out path
}
