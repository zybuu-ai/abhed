package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// manifestPath is the registry path of a tag's manifest: library models
// under library/, others under their namespace.
func manifestPath(tag string) (string, error) {
	tag = normalTag(tag)
	i := strings.LastIndex(tag, ":")
	name, ver := tag[:max(i, 0)], tag[i+1:]
	if name == "" || ver == "" || strings.ContainsAny(name, " ?#\\") || strings.ContainsAny(ver, " ?#/\\") {
		return "", fmt.Errorf("%q is not a model tag", tag)
	}
	if strings.Count(name, "/") > 1 {
		return "", fmt.Errorf("%q names a registry Abhed does not read sizes from", tag)
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return "/v2/" + name + "/manifests/" + url.PathEscape(ver), nil
}

// Size is a tag's download size from the registry's manifest: the sum of
// its layers and config. It is a read-only HTTPS GET.
func (e Env) Size(ctx context.Context, tag string) (int64, error) {
	p, err := manifestPath(tag)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(e.Registry, "/")+p, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := e.web().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, errNotFound
	case resp.StatusCode != http.StatusOK:
		return 0, fmt.Errorf("the registry answered %s", resp.Status)
	}
	var m struct {
		Config struct {
			Size int64 `json:"size"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&m); err != nil {
		return 0, fmt.Errorf("the registry's manifest could not be read: %w", err)
	}
	total := m.Config.Size
	for _, l := range m.Layers {
		total += l.Size
	}
	if total <= 0 {
		return 0, errors.New("the registry's manifest lists no layers")
	}
	return total, nil
}

var errNotFound = errors.New("no such model in the registry")

// Fit is how a download fits this machine.
type Fit struct {
	Bytes int64 `json:"bytes"`
	// SizeKnown is false when the registry could not be asked.
	SizeKnown bool `json:"size_known"`
	// DiskFree is the free space where Ollama keeps models; 0 when unknown.
	DiskFree uint64 `json:"disk_free,omitempty"`
	// MemTotal is the system's memory; 0 when unknown.
	MemTotal uint64 `json:"mem_total,omitempty"`
	// RAMNeededGB is the memory the model wants, estimated for one not curated.
	RAMNeededGB int  `json:"ram_needed_gb,omitempty"`
	RAMEstimate bool `json:"ram_estimate,omitempty"`
	// Warnings are reasons to think twice; a Problem is reason not to start.
	Warnings []string `json:"warnings,omitempty"`
	// Smaller is a curated model that fits when this one may not.
	Smaller string `json:"smaller,omitempty"`
}

// diskMargin is room kept free beyond the download.
const diskMargin = 2 << 30

// LocalFit is what this machine says of a download without asking any
// host: the free disk where Ollama keeps models, the memory, and a curated
// model's size from Abhed's own list (SizeKnown stays false: the registry
// was not asked).
func (e Env) LocalFit(t Target) Fit {
	f := Fit{}
	if c, ok := Lookup(t.Model); ok {
		f.Bytes = c.Bytes
	}
	if e.DiskFree != nil {
		if free, err := e.DiskFree(e.ModelsDir); err == nil {
			f.DiskFree = free
		}
	}
	f.RAMNeededGB, f.RAMEstimate = RAMNeededGB(t.Model, f.Bytes)
	if e.MemTotal != nil {
		if mem, err := e.MemTotal(); err == nil {
			f.MemTotal = mem
		}
	}
	return f
}

// CheckFit asks the registry for the tag's size and compares it, and the
// memory the model wants, with this machine. Too little disk is a Problem;
// too little memory is a warning with a smaller curated model suggested.
func (e Env) CheckFit(ctx context.Context, t Target, st Status) (Fit, *Problem) {
	f := Fit{}
	size, err := e.Size(ctx, t.Model)
	switch {
	case err == nil:
		f.Bytes, f.SizeKnown = size, true
	case errors.Is(err, errNotFound):
		return f, &Problem{Kind: KindModelNotFound, Message: fmt.Sprintf("The Ollama library has no model %s. Check the name at https://ollama.com/library.", t.Model),
			Steps: []string{"Find the exact tag at https://ollama.com/library", "Set model in this provider's configuration to that tag", retryStep(t)}, Retry: true}
	case offline(err):
		return f, e.withNoNetwork(t, st, err)
	default:
		if c, ok := Lookup(t.Model); ok {
			f.Bytes = c.Bytes
		}
		f.Warnings = append(f.Warnings, "The download size could not be read from the registry ("+err.Error()+").")
	}
	if e.DiskFree != nil {
		if free, err := e.DiskFree(e.ModelsDir); err == nil {
			f.DiskFree = free
			if f.Bytes > 0 && free < uint64(f.Bytes)+diskMargin {
				p := e.problem(KindDiskShort, fmt.Sprintf("%s needs %s and leaves room to spare, but only %s is free where Ollama keeps models (%s).",
					t.Model, HumanBytes(f.Bytes), HumanBytes(int64(free)), e.ModelsDir), t, st)
				p.Steps = append([]string{"Free at least " + HumanBytes(f.Bytes+diskMargin-int64(free)) + " there, or choose a smaller model"}, p.Steps...)
				return f, p
			}
		} else {
			f.Warnings = append(f.Warnings, "Free disk space could not be read: "+err.Error())
		}
	}
	f.RAMNeededGB, f.RAMEstimate = RAMNeededGB(t.Model, f.Bytes)
	if e.MemTotal != nil {
		if mem, err := e.MemTotal(); err == nil {
			f.MemTotal = mem
			gb := int(mem / (1 << 30))
			if f.RAMNeededGB > 0 && gb < f.RAMNeededGB {
				w := fmt.Sprintf("%s wants about %d GB of memory and this machine has %d GB; it may be very slow or fail to load.", t.Model, f.RAMNeededGB, gb)
				if s, ok := Smaller(t.Model, gb); ok {
					f.Smaller = s.Tag
					w += fmt.Sprintf(" %s (%s, %s) fits.", s.Label, s.Tag, HumanBytes(s.Bytes))
				}
				f.Warnings = append(f.Warnings, w)
			}
		} else {
			f.Warnings = append(f.Warnings, "System memory could not be read, so it was not checked.")
		}
	}
	return f, nil
}

func (e Env) withNoNetwork(t Target, st Status, err error) *Problem {
	p := e.problem(KindNoNetwork, "The Ollama registry could not be reached ("+err.Error()+"); there may be no network here.", t, st)
	p.Steps = append([]string{"Check the network connection, or on a machine with one run ollama pull " + t.Model + " and copy ~/.ollama/models across"}, p.Steps...)
	return p
}

// offline reports whether err is the network, not the registry, failing.
func offline(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || errors.As(err, &op)
}

// Progress is one line of a pull's progress.
type Progress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

// Pull asks the local server to download t.Model, reporting progress as
// it streams, until it ends or ctx is cancelled. Ollama keeps what it has
// downloaded, so a pull that is cancelled resumes where it stopped.
func (e Env) Pull(ctx context.Context, t Target, st Status, progress func(Progress)) *Problem {
	if !t.Loopback {
		return &Problem{Kind: KindRemote, Message: fmt.Sprintf("%s is on another machine; Abhed pulls models only into the Ollama on this one.", t.API),
			Steps: []string{"On " + t.Host + ": ollama pull " + t.Model, retryStep(t)}, Retry: true}
	}
	body, _ := json.Marshal(map[string]any{"model": t.Model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.API+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return e.problem(KindPullFailed, "The download could not be asked for: "+err.Error(), t, st)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return e.cancelled(t, st)
		}
		if refused(err) {
			st.Running = false
			return e.problem(KindNotRunning, "Ollama stopped answering before the download began.", t, st)
		}
		return e.problem(KindPullFailed, "The download could not be started: "+err.Error(), t, st)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var b struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&b)
		return e.pullError(t, st, orStatus(b.Error, resp.Status))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	done := false
	for sc.Scan() {
		var line struct {
			Progress
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Error != "" {
			return e.pullError(t, st, line.Error)
		}
		if progress != nil {
			progress(line.Progress)
		}
		done = done || line.Status == "success"
	}
	if ctx.Err() != nil {
		return e.cancelled(t, st)
	}
	if err := sc.Err(); err != nil {
		return e.problem(KindPullFailed, "The download stopped: "+err.Error(), t, st)
	}
	if !done {
		return e.problem(KindPullFailed, "The download ended without Ollama saying it succeeded.", t, st)
	}
	return nil
}

func (e Env) cancelled(t Target, st Status) *Problem {
	return &Problem{Kind: KindCancelled, Message: "The download was stopped. Ollama keeps what it fetched, so pulling again resumes it.", Steps: e.Steps(t, st), Retry: true}
}

// pullError names what Ollama said went wrong.
func (e Env) pullError(t Target, st Status, msg string) *Problem {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "file does not exist") || strings.Contains(low, "not found") || strings.Contains(low, "manifest unknown"):
		return &Problem{Kind: KindModelNotFound, Message: fmt.Sprintf("Ollama could not find %s (%s). Check the name at https://ollama.com/library.", t.Model, msg),
			Steps: []string{"Find the exact tag at https://ollama.com/library", "Set model in this provider's configuration to that tag", retryStep(t)}, Retry: true}
	case strings.Contains(low, "no space"):
		return e.problem(KindDiskShort, "The disk filled up during the download ("+msg+").", t, st)
	case strings.Contains(low, "dial tcp") || strings.Contains(low, "no such host") || strings.Contains(low, "network"):
		return e.withNoNetwork(t, st, errors.New(msg))
	}
	return e.problem(KindPullFailed, "Ollama could not download "+t.Model+": "+msg, t, st)
}

func orStatus(msg, status string) string {
	if msg != "" {
		return msg
	}
	return status
}

// HumanBytes is n as a person reads it: 4.7 GB.
func HumanBytes(n int64) string {
	const k = 1000
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.0f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d bytes", n)
}
