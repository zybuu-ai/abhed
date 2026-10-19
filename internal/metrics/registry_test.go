package metrics

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTextExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Things.\nCounted.", "kind")
	c.Inc("a")
	c.Add(2, "b\"\n\\")
	r.GaugeFunc("x_up", "Up.", func() float64 { return 1 })
	h := r.Histogram("x_seconds", "Time.", []float64{0.1, 1}, "op")
	h.Observe(0.05, "get")
	h.Observe(0.5, "get")
	h.Observe(5, "get")
	var b bytes.Buffer
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP x_total Things.\nCounted.
# TYPE x_total counter
x_total{kind="a"} 1
x_total{kind="b\"\n\\"} 2
# HELP x_up Up.
# TYPE x_up gauge
x_up 1
# HELP x_seconds Time.
# TYPE x_seconds histogram
x_seconds_bucket{op="get",le="0.1"} 1
x_seconds_bucket{op="get",le="1"} 2
x_seconds_bucket{op="get",le="+Inf"} 3
x_seconds_sum{op="get"} 5.55
x_seconds_count{op="get"} 3
`
	if b.String() != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestSeriesAreBounded(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("y_total", "Y.", "v")
	for i := 0; i < MaxSeries+50; i++ {
		c.Inc(fmt.Sprint(i))
	}
	if got := len(c.f.series); got != MaxSeries+1 {
		t.Fatalf("%d series, want %d and one for the rest", got, MaxSeries+1)
	}
	if c.Value(Other) != 50 {
		t.Fatalf("other = %v, want 50", c.Value(Other))
	}
}

func TestLabelValuesAreClipped(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("z_total", "Z.", "v")
	c.Inc(strings.Repeat("é", 100) + "\xff")
	var b bytes.Buffer
	_ = r.Write(&b)
	line := strings.Split(b.String(), "\n")[2]
	val := line[strings.Index(line, `"`)+1 : strings.LastIndex(line, `"`)]
	if len(val) > maxValue || strings.Contains(val, "\xff") {
		t.Fatalf("value not clipped: %q", val)
	}
}

func TestHandlerContentType(t *testing.T) {
	r := NewRegistry()
	r.Counter("a_total", "A.").Inc()
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Header().Get("Content-Type") != ContentType || !strings.Contains(w.Body.String(), "a_total 1") {
		t.Fatalf("%q %q", w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestDuplicateAndInvalidNamesPanic(t *testing.T) {
	for name, f := range map[string]func(r *Registry){
		"twice":    func(r *Registry) { r.Counter("a", ""); r.Counter("a", "") },
		"bad name": func(r *Registry) { r.Counter("a-b", "") },
		"le label": func(r *Registry) { r.Histogram("h", "", []float64{1}, "le") },
		"unsorted": func(r *Registry) { r.Histogram("h", "", []float64{2, 1}) },
		"reserved": func(r *Registry) { r.Counter("c", "", "__x") },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			f(NewRegistry())
		})
	}
}
