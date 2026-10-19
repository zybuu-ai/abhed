// Command abhed-drift runs the supervisor's drift suite and reports how many
// drift cases it stopped, how often it got in the way of benign work, and
// how long a judgement took.
//
// By default it measures the rules judge, on the suite's fixed PATH, so the
// numbers are the same on every machine. With -endpoint and -model it
// measures the rules followed by a model judge behind an OpenAI-compatible
// endpoint, which is what judge "model" runs.
//
//	abhed-drift
//	abhed-drift -endpoint http://127.0.0.1:8000/v1 -model granite-guardian -api-key-env JUDGE_KEY
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/zybuu-ai/abhed/monitor"
)

func main() {
	endpoint := flag.String("endpoint", "", "OpenAI-compatible base URL of a model judge")
	modelName := flag.String("model", "", "the model judge's model")
	keyEnv := flag.String("api-key-env", "", "variable holding the endpoint's key")
	timeout := flag.Duration("timeout", 10*time.Second, "bound on one judgement")
	asJSON := flag.Bool("json", false, "print the report as JSON")
	flag.Parse()

	rules := monitor.Rules{LookPath: func(p string) bool { return monitor.DriftPATH[p] }, Declared: monitor.DriftDeclared, Hosts: monitor.DriftHosts, Home: monitor.DriftHome}
	judge, name := monitor.Monitor(rules), monitor.RulesVersion
	if *endpoint != "" || *modelName != "" {
		key := ""
		if *keyEnv != "" {
			key = os.Getenv(*keyEnv)
		}
		judge = monitor.Chain{rules, monitor.Model{Endpoint: *endpoint, Model: *modelName, APIKey: key}}
		name = monitor.RulesVersion + " + " + monitor.PromptVersion + "/" + *modelName
	}
	rep := monitor.RunDrift(context.Background(), name, func() *monitor.Guard {
		return &monitor.Guard{Monitor: judge, Timeout: *timeout, Honeytokens: []string{monitor.DriftHoneytoken}}
	})
	if *asJSON {
		out := map[string]any{"judge": rep.Judge, "drift": rep.Drift, "caught": rep.Caught, "benign": rep.Benign,
			"by_policy": rep.ByPolicy, "benign_denied": rep.BenignDenied, "benign_asked": rep.BenignAsked, "catch_rate": rep.CatchRate(),
			"false_deny_rate": rep.FalseDenyRate(), "false_ask_rate": rep.FalseAskRate(),
			"p50_ms": rep.P50MS, "p95_ms": rep.P95MS, "max_ms": rep.MaxMS}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}
	rep.Write(os.Stdout)
}
