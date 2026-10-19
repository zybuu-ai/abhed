// Package metrics is a small Prometheus exporter: counters, gauges and
// histograms with fixed label names, written in the text exposition format.
// It holds no user content by construction: every label value is clipped,
// and a family takes a bounded number of label sets, after which new ones
// are counted under "other".
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxSeries is how many label sets one family keeps before folding the rest
// into one whose every label is Other.
const MaxSeries = 500

// Other is the label value of what a bounded label does not name.
const Other = "other"

// maxValue is the longest label value kept, in bytes.
const maxValue = 64

// Registry holds the families an exporter writes, in the order they were made.
type Registry struct {
	mu       sync.Mutex
	families []*family
	names    map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

type kind int

const (
	kindCounter kind = iota
	kindGauge
	kindHistogram
)

func (k kind) String() string {
	switch k {
	case kindGauge:
		return "gauge"
	case kindHistogram:
		return "histogram"
	}
	return "counter"
}

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	fn         func() float64 // a gauge read when written

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	value  float64  // counter or gauge
	counts []uint64 // histogram, one per bucket, not cumulative
	sum    float64  // histogram
	count  uint64   // histogram
}

func (r *Registry) add(f *family) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[f.name] {
		panic("metrics: " + f.name + " registered twice")
	}
	if !validName(f.name) {
		panic("metrics: invalid name " + f.name)
	}
	for _, l := range f.labels {
		if !validName(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic("metrics: invalid label " + l + " on " + f.name)
		}
	}
	r.names[f.name] = true
	f.series = map[string]*series{}
	r.families = append(r.families, f)
	return f
}

// Counter is a family of counters.
type Counter struct{ f *family }

// Counter registers a counter family with the named labels.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.add(&family{name: name, help: help, kind: kindCounter, labels: labels})}
}

// Add adds v, which must not be negative, to the series of the label values.
func (c *Counter) Add(v float64, values ...string) {
	if v < 0 || math.IsNaN(v) {
		return
	}
	c.f.with(values, func(s *series) { s.value += v })
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Value is the series' value, for tests.
func (c *Counter) Value(values ...string) float64 { return c.f.value(values) }

// Gauge is a family of gauges.
type Gauge struct{ f *family }

// Gauge registers a gauge family with the named labels.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.add(&family{name: name, help: help, kind: kindGauge, labels: labels})}
}

// Set sets the series of the label values to v.
func (g *Gauge) Set(v float64, values ...string) {
	g.f.with(values, func(s *series) { s.value = v })
}

// Add adds v, which may be negative.
func (g *Gauge) Add(v float64, values ...string) {
	g.f.with(values, func(s *series) { s.value += v })
}

// Value is the series' value, for tests.
func (g *Gauge) Value(values ...string) float64 { return g.f.value(values) }

// GaugeFunc registers an unlabelled gauge whose value fn gives as it is written.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: kindGauge, fn: fn})
}

// Histogram is a family of histograms.
type Histogram struct{ f *family }

// Histogram registers a histogram family with the given upper bounds, which
// must rise; +Inf is implied.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if !slices.IsSorted(buckets) || len(buckets) == 0 {
		panic("metrics: buckets of " + name + " must rise")
	}
	return &Histogram{r.add(&family{name: name, help: help, kind: kindHistogram, labels: labels, buckets: slices.Clone(buckets)})}
}

// Observe adds one observation of v.
func (h *Histogram) Observe(v float64, values ...string) {
	if math.IsNaN(v) {
		return
	}
	h.f.with(values, func(s *series) {
		if s.counts == nil {
			s.counts = make([]uint64, len(h.f.buckets))
		}
		if i := sort.SearchFloat64s(h.f.buckets, v); i < len(h.f.buckets) {
			s.counts[i]++
		}
		s.sum += v
		s.count++
	})
}

// Count is how many observations the series has, for tests.
func (h *Histogram) Count(values ...string) uint64 {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if s := h.f.series[key(h.f.norm(values))]; s != nil {
		return s.count
	}
	return 0
}

// DurationBuckets suit latencies from a millisecond to two minutes, in seconds.
var DurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// norm clips each value and fills missing ones, so a call site that passes
// the wrong number of values cannot make an invalid series.
func (f *family) norm(values []string) []string {
	out := make([]string, len(f.labels))
	for i := range out {
		if i < len(values) {
			out[i] = clip(values[i])
		}
	}
	return out
}

func (f *family) with(values []string, do func(*series)) {
	vs := f.norm(values)
	k := key(vs)
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.series[k]
	if s == nil {
		if len(f.series) >= MaxSeries {
			for i := range vs {
				vs[i] = Other
			}
			k = key(vs)
			s = f.series[k]
		}
		if s == nil {
			s = &series{values: vs}
			f.series[k] = s
		}
	}
	do(s)
}

func (f *family) value(values []string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.series[key(f.norm(values))]; s != nil {
		return s.value
	}
	return 0
}

func key(vs []string) string { return strings.Join(vs, "\xff") }

// clip keeps a label value short and valid UTF-8.
func clip(v string) string {
	if !utf8.ValidString(v) {
		v = strings.ToValidUTF8(v, "?")
	}
	if len(v) > maxValue {
		cut := maxValue
		for cut > 0 && !utf8.RuneStart(v[cut]) {
			cut--
		}
		v = v[:cut]
	}
	return v
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// ContentType is the text exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Write writes every family in the text exposition format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	fams := slices.Clone(r.families)
	r.mu.Unlock()
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		f.write(bw)
	}
	return bw.Flush()
}

// Handler serves the registry.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		_ = r.Write(w)
	})
}

func (f *family) write(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	if f.fn != nil {
		fmt.Fprintf(w, "%s %s\n", f.name, num(f.fn()))
		return
	}
	f.mu.Lock()
	all := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		cp := *s
		cp.counts = slices.Clone(s.counts)
		all = append(all, &cp)
	}
	f.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return key(all[i].values) < key(all[j].values) })
	for _, s := range all {
		labels := f.pairs(s.values)
		if f.kind != kindHistogram {
			fmt.Fprintf(w, "%s%s %s\n", f.name, braces(labels), num(s.value))
			continue
		}
		var cum uint64
		for i, b := range f.buckets {
			if s.counts != nil {
				cum += s.counts[i]
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, braces(append(slices.Clone(labels), `le="`+num(b)+`"`)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, braces(append(slices.Clone(labels), `le="+Inf"`)), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", f.name, braces(labels), num(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", f.name, braces(labels), s.count)
	}
}

func (f *family) pairs(values []string) []string {
	out := make([]string, len(f.labels))
	for i, l := range f.labels {
		out[i] = l + `="` + escapeValue(values[i]) + `"`
	}
	return out
}

func braces(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

func num(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var valueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeValue(s string) string { return valueEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
