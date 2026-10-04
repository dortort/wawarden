package metrics_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dortort/wawarden/internal/metrics"
)

func exposition(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestExposition(t *testing.T) {
	r := metrics.NewRegistry()
	build := r.GaugeVec("wawarden_build_info", "Build metadata.", "version", "revision", "dev")
	panics := r.CounterVec("wawarden_panics_total", "Recovered panics by name.", "name")
	auth := r.Counter("wawarden_auth_failures_total", `Failed client authentications, see C:\docs`+"\nsecond line.")
	r.CounterVec("wawarden_empty_total", "A vector with no series yet.", "name")
	temperature := r.Gauge("wawarden_temperature", "A float gauge.")

	panics.With("worker").Inc()
	for range 3 {
		panics.With("api").Inc()
	}
	panics.With("worker").Inc()
	build.With("v1.2.3", "abc123", "false").Set(1)
	for range 7 {
		auth.Inc()
	}
	temperature.Set(-0.25)
	temperature.Set(0.75)

	want := `# HELP wawarden_auth_failures_total Failed client authentications, see C:\\docs\nsecond line.
# TYPE wawarden_auth_failures_total counter
wawarden_auth_failures_total 7
# HELP wawarden_build_info Build metadata.
# TYPE wawarden_build_info gauge
wawarden_build_info{version="v1.2.3",revision="abc123",dev="false"} 1
# HELP wawarden_panics_total Recovered panics by name.
# TYPE wawarden_panics_total counter
wawarden_panics_total{name="api"} 3
wawarden_panics_total{name="worker"} 2
# HELP wawarden_temperature A float gauge.
# TYPE wawarden_temperature gauge
wawarden_temperature 0.75
`
	if got := exposition(t, r); got != want {
		t.Fatalf("exposition mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnlabelledMetricsAreExposedAtZero(t *testing.T) {
	r := metrics.NewRegistry()
	r.Counter("wawarden_admin_auth_failures_total", "Failed admin authentications.")
	r.Gauge("wawarden_ready", "Readiness.")

	want := `# HELP wawarden_admin_auth_failures_total Failed admin authentications.
# TYPE wawarden_admin_auth_failures_total counter
wawarden_admin_auth_failures_total 0
# HELP wawarden_ready Readiness.
# TYPE wawarden_ready gauge
wawarden_ready 0
`
	if got := exposition(t, r); got != want {
		t.Fatalf("exposition mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "worker", want: `{name="worker"}`},
		{name: "backslash", value: `C:\tmp`, want: `{name="C:\\tmp"}`},
		{name: "double quote", value: `say "hi"`, want: `{name="say \"hi\""}`},
		{name: "newline", value: "line1\nline2", want: `{name="line1\nline2"}`},
		{name: "all three", value: "\\\"\n", want: `{name="\\\"\n"}`},
		{name: "invalid utf-8", value: "a\xffb", want: "{name=\"a\uFFFDb\"}"},
		{name: "empty", value: "", want: `{name=""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := metrics.NewRegistry()
			r.CounterVec("wawarden_escape_total", "Escaping.", "name").With(tt.value).Inc()
			line := "wawarden_escape_total" + tt.want + " 1\n"
			if got := exposition(t, r); !strings.HasSuffix(got, line) || strings.Count(got, "\n") != 3 {
				t.Fatalf("exposition = %q, want last line %q", got, line)
			}
		})
	}
}

func TestInvalidUTF8ValuesShareOneSeries(t *testing.T) {
	r := metrics.NewRegistry()
	v := r.CounterVec("wawarden_utf8_total", "UTF-8.", "name")
	v.With("a\xfe").Inc()
	v.With("a\xff").Inc()
	if got := v.With("a\uFFFD").Value(); got != 2 {
		t.Fatalf("sanitised series value = %d, want 2", got)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	populate := func(order []string) string {
		r := metrics.NewRegistry()
		vecs := map[string]*metrics.CounterVec{}
		for _, name := range []string{"wawarden_b_total", "wawarden_a_total", "wawarden_c_total"} {
			vecs[name] = r.CounterVec(name, "Ordering.", "x", "y")
		}
		for _, name := range []string{"wawarden_c_total", "wawarden_a_total", "wawarden_b_total"} {
			for _, v := range order {
				vecs[name].With(v, "z").Inc()
				vecs[name].With("m", v).Inc()
			}
		}
		return exposition(t, r)
	}
	first := populate([]string{"q", "a", "z", "m", "b"})
	second := populate([]string{"b", "m", "z", "a", "q"})
	if first != second {
		t.Fatalf("exposition depends on insertion order\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	idxA := strings.Index(first, "# HELP wawarden_a_total")
	idxB := strings.Index(first, "# HELP wawarden_b_total")
	idxC := strings.Index(first, "# HELP wawarden_c_total")
	if idxA < 0 || idxA >= idxB || idxB >= idxC {
		t.Fatalf("families are not sorted by name:\n%s", first)
	}
	wantA := `wawarden_a_total{x="a",y="z"} 1
wawarden_a_total{x="b",y="z"} 1
wawarden_a_total{x="m",y="a"} 1
wawarden_a_total{x="m",y="b"} 1
wawarden_a_total{x="m",y="m"} 1
wawarden_a_total{x="m",y="q"} 1
wawarden_a_total{x="m",y="z"} 2
wawarden_a_total{x="q",y="z"} 1
wawarden_a_total{x="z",y="z"} 1
`
	if !strings.Contains(first, wantA) {
		t.Fatalf("series are not sorted by label values:\n%s", first)
	}
	if exposition(t, metrics.NewRegistry()) != "" {
		t.Fatal("an empty registry produced output")
	}
}

func TestConcurrentUpdates(t *testing.T) {
	const workers, perWorker = 16, 1000
	r := metrics.NewRegistry()
	total := r.Counter("wawarden_total", "Total.")
	byName := r.CounterVec("wawarden_by_name_total", "By name.", "name")
	level := r.Gauge("wawarden_level", "Level.")

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			name := fmt.Sprintf("w%d", w%4)
			for range perWorker {
				total.Inc()
				byName.With(name).Inc()
				level.Set(float64(w))
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			var b strings.Builder
			if err := r.WriteText(&b); err != nil {
				t.Errorf("WriteText: %v", err)
				return
			}
		}
	})
	wg.Wait()

	if got := total.Value(); got != workers*perWorker {
		t.Fatalf("total = %d, want %d", got, workers*perWorker)
	}
	for i := range 4 {
		name := fmt.Sprintf("w%d", i)
		if got := byName.With(name).Value(); got != workers/4*perWorker {
			t.Fatalf("%s = %d, want %d", name, got, workers/4*perWorker)
		}
	}
	if got := level.Value(); got != float64(int(got)) || got < 0 || got >= workers {
		t.Fatalf("level = %v, want one of the values the workers set", got)
	}
	if out := exposition(t, r); !strings.Contains(out, fmt.Sprintf("wawarden_total %d\n", workers*perWorker)) {
		t.Fatalf("exposition does not show the total:\n%s", out)
	}
}

func TestSamplesExposeOnlyTheMethodsInUse(t *testing.T) {
	tests := []struct {
		typ  reflect.Type
		want []string
	}{
		{typ: reflect.TypeFor[*metrics.Counter](), want: []string{"Inc", "Value"}},
		{typ: reflect.TypeFor[*metrics.Gauge](), want: []string{"Set", "Value"}},
	}
	for _, tt := range tests {
		var got []string
		for m := range tt.typ.Methods() {
			got = append(got, m.Name)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s exports %q, want %q: add a method together with its first caller", tt.typ, got, tt.want)
		}
	}
}

func TestRegistrationPanics(t *testing.T) {
	tests := []struct {
		name     string
		register func(r *metrics.Registry)
	}{
		{name: "missing prefix", register: func(r *metrics.Registry) { r.Counter("panics_total", "h") }},
		{name: "prefix only", register: func(r *metrics.Registry) { r.Counter("wawarden_", "h") }},
		{name: "invalid character", register: func(r *metrics.Registry) { r.Gauge("wawarden_bad-name", "h") }},
		{name: "invalid label", register: func(r *metrics.Registry) { r.CounterVec("wawarden_x_total", "h", "bad-label") }},
		{name: "reserved label", register: func(r *metrics.Registry) { r.GaugeVec("wawarden_x", "h", "__name") }},
		{name: "duplicate label", register: func(r *metrics.Registry) { r.CounterVec("wawarden_x_total", "h", "a", "a") }},
		{name: "vector without labels", register: func(r *metrics.Registry) { r.CounterVec("wawarden_x_total", "h") }},
		{name: "duplicate metric", register: func(r *metrics.Registry) {
			r.Counter("wawarden_x_total", "h")
			r.GaugeVec("wawarden_x_total", "h", "a")
		}},
		{name: "wrong label arity", register: func(r *metrics.Registry) {
			r.CounterVec("wawarden_x_total", "h", "a", "b").With("only-one")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			tt.register(metrics.NewRegistry())
		})
	}
}

type failingWriter struct{}

var errWrite = errors.New("write failed")

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestWriteTextReturnsWriterError(t *testing.T) {
	r := metrics.NewRegistry()
	r.Counter("wawarden_x_total", "h")
	if err := r.WriteText(failingWriter{}); !errors.Is(err, errWrite) {
		t.Fatalf("WriteText error = %v, want %v", err, errWrite)
	}
}
