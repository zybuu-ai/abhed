package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store/local"
)

// The handshake: what this engine is and which of the Studio contract's
// areas it serves (docs/architecture/studio-acp-contract.md §1.4).

// acpAPILevel is the contract level this engine speaks.
const acpAPILevel = 1

// acpBuild names the binary for the handshake and `abhed version --json`.
type acpBuild struct {
	Version string
	Edition string // "ce" or "ee"
	Commit  string
}

// buildOf reads the edition from its display name and the commit from the
// build's own VCS stamp, "" when the build has none.
func buildOf(version, edition string) acpBuild {
	b := acpBuild{Version: strings.TrimPrefix(version, "v"), Edition: "ce"}
	if strings.Contains(strings.ToLower(edition), "enterprise") {
		b.Edition = "ee"
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.revision" {
				b.Commit = kv.Value
			}
		}
	}
	return b
}

// acpFeatures are the contract areas this engine serves. The record's areas
// are listed only when a durable record is open.
func (c *acpConn) acpFeatures() []string {
	f := []string{}
	if c.durable() {
		f = append(f, recordFeatures...)
	}
	return append(f, liveFeatures...)
}

// recordFeatures are the areas served from the durable record, and
// liveFeatures those that need none; each section adds its own.
var recordFeatures, liveFeatures []string

// agentMeta is the zybuu.ai/abhed block of initialize's agentCapabilities.
func (c *acpConn) agentMeta() map[string]any {
	store := map[string]any{"store": "memory"}
	if c.durable() {
		store["store"], store["dir"] = "local", c.recDir
	}
	return map[string]any{
		"apiLevel": acpAPILevel, "edition": c.build.Edition, "version": c.build.Version,
		"commit": c.build.Commit, "features": c.acpFeatures(), "record": store,
		"managed": c.managed(),
	}
}

// managed reports whether a managed configuration is in force.
func (c *acpConn) managed() bool {
	cfg, err := config.LoadManaged()
	return err == nil && cfg.Managed
}

func (c *acpConn) initialize(msg rpcMessage) {
	caps := map[string]any{
		"loadSession":        c.durable(),
		"promptCapabilities": map[string]any{"image": false, "audio": false, "embeddedContext": true},
		"mcpCapabilities":    map[string]any{"http": false, "sse": false},
		"_meta":              map[string]any{acpMetaKey: c.agentMeta()},
	}
	// Deletion is never advertised: records leave only through abhed record prune.
	if c.durable() {
		caps["sessionCapabilities"] = map[string]any{"list": map[string]any{}, "resume": map[string]any{}, "close": map[string]any{}}
	}
	c.reply(msg.ID, map[string]any{
		"protocolVersion":   acpProtocolVersion,
		"agentCapabilities": caps,
		"agentInfo":         map[string]any{"name": "abhed", "title": "Abhed", "version": c.version},
		"authMethods":       []any{},
	}, nil)
}

// versionJSON prints the handshake's block without starting a session, so
// Studio can check an engine before it spawns it.
func versionJSON(w io.Writer, build acpBuild, workspace string, trust config.TrustChoice) error {
	c := &acpConn{build: build, base: workspace, trust: trust}
	c.useRecord(workspace, trust)
	b, err := json.MarshalIndent(c.agentMeta(), "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// durable reports whether sessions are kept in the local record, where they
// can be listed, loaded, verified and exported.
func (c *acpConn) durable() bool { return c.openRecord != nil }

// useRecord makes the connection keep its sessions in the local record the
// configuration names, opened on first use. A Postgres configuration is the
// server's; sessions here then stay in memory, as the handshake says.
func (c *acpConn) useRecord(workspace string, trust config.TrustChoice) {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
	if err != nil || cfg.Storage.Driver == "postgres" {
		return
	}
	c.recDir = cfg.Record.Dir
	if c.recDir == "" {
		if d, err := local.DefaultDir(); err == nil {
			c.recDir = d
		}
	}
	c.recUser, c.recTenant = cliUser(), cliTenant(cfg)
	c.openRecord = func() (*local.Store, error) { return openRecord(cfg) }
}

// record opens the local record once; every session of the connection shares it.
func (c *acpConn) record() (*local.Store, error) {
	c.recOnce.Do(func() {
		if c.openRecord == nil {
			c.recErr = errors.New("this engine keeps no durable record")
			return
		}
		c.rec, c.recErr = c.openRecord()
	})
	return c.rec, c.recErr
}

// closeRecord releases every session this process holds in the record.
func (c *acpConn) closeRecord() {
	if c.rec != nil {
		_ = c.rec.Close()
	}
}

// firstSentence keeps a description to what fits in a list row.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 && i < 220 {
		return s[:i+1]
	}
	if len(s) > 220 {
		cut := 220
		for cut > 0 && s[cut]&0xC0 == 0x80 {
			cut--
		}
		return s[:cut] + "…"
	}
	return s
}
