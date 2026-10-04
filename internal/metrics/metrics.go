// Package metrics is a small registry of counters and gauges with Prometheus text exposition.
package metrics

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	metricName   = regexp.MustCompile(`^wawarden_[a-zA-Z0-9_:]+$`)
	labelName    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	valueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

type Counter struct{ n atomic.Uint64 }

func (c *Counter) Inc()           { c.n.Add(1) }
func (c *Counter) Value() uint64  { return c.n.Load() }
func (c *Counter) format() string { return strconv.FormatUint(c.Value(), 10) }

type Gauge struct{ bits atomic.Uint64 }

func (g *Gauge) Set(v float64)  { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }
func (g *Gauge) format() string { return strconv.FormatFloat(g.Value(), 'g', -1, 64) }

type CounterVec struct{ f *family }

func (v *CounterVec) With(labelValues ...string) *Counter { return v.f.with(labelValues).(*Counter) }

type GaugeVec struct{ f *family }

func (v *GaugeVec) With(labelValues ...string) *Gauge { return v.f.with(labelValues).(*Gauge) }

func (r *Registry) Counter(name, help string) *Counter {
	return r.register(name, help, "counter", nil, newCounter).with(nil).(*Counter)
}

func (r *Registry) CounterVec(name, help string, labels ...string) *CounterVec {
	return &CounterVec{r.register(name, help, "counter", requireLabels(name, labels), newCounter)}
}

func (r *Registry) Gauge(name, help string) *Gauge {
	return r.register(name, help, "gauge", nil, newGauge).with(nil).(*Gauge)
}

func (r *Registry) GaugeVec(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{r.register(name, help, "gauge", requireLabels(name, labels), newGauge)}
}

func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	families := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		families = append(families, f)
	}
	r.mu.Unlock()
	slices.SortFunc(families, func(a, b *family) int { return strings.Compare(a.name, b.name) })

	var b strings.Builder
	for _, f := range families {
		f.write(&b)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

type sample interface{ format() string }

func newCounter() sample { return new(Counter) }
func newGauge() sample   { return new(Gauge) }

type series struct {
	labelValues []string
	sample      sample
}

type family struct {
	name      string
	help      string
	kind      string
	labels    []string
	newSample func() sample

	mu     sync.Mutex
	series map[string]*series
}

func requireLabels(name string, labels []string) []string {
	if len(labels) == 0 {
		panic(fmt.Sprintf("metrics: %s is declared as a vector without labels", name))
	}
	return labels
}

func (r *Registry) register(name, help, kind string, labels []string, newSample func() sample) *family {
	if !metricName.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", name))
	}
	for i, l := range labels {
		if !labelName.MatchString(l) || strings.HasPrefix(l, "__") || slices.Contains(labels[:i], l) {
			panic(fmt.Sprintf("metrics: invalid or duplicate label %q on %s", l, name))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic(fmt.Sprintf("metrics: %s is already registered", name))
	}
	f := &family{
		name:      name,
		help:      help,
		kind:      kind,
		labels:    slices.Clone(labels),
		newSample: newSample,
		series:    make(map[string]*series),
	}
	r.families[name] = f
	return f
}

func (f *family) with(labelValues []string) sample {
	if len(labelValues) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(labelValues)))
	}
	values := make([]string, len(labelValues))
	for i, v := range labelValues {
		values[i] = strings.ToValidUTF8(v, "�")
	}
	// 0xff never occurs in valid UTF-8, so the joined key is unambiguous.
	key := strings.Join(values, "\xff")

	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[key]
	if !ok {
		s = &series{labelValues: values, sample: f.newSample()}
		f.series[key] = s
	}
	return s.sample
}

func (f *family) write(b *strings.Builder) {
	f.mu.Lock()
	all := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		all = append(all, s)
	}
	f.mu.Unlock()
	if len(all) == 0 {
		return
	}
	slices.SortFunc(all, func(a, b *series) int { return slices.Compare(a.labelValues, b.labelValues) })

	fmt.Fprintf(b, "# HELP %s %s\n", f.name, helpEscaper.Replace(f.help))
	fmt.Fprintf(b, "# TYPE %s %s\n", f.name, f.kind)
	for _, s := range all {
		b.WriteString(f.name)
		if len(f.labels) > 0 {
			b.WriteByte('{')
			for i, l := range f.labels {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(l)
				b.WriteString(`="`)
				b.WriteString(valueEscaper.Replace(s.labelValues[i]))
				b.WriteByte('"')
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(s.sample.format())
		b.WriteByte('\n')
	}
}
