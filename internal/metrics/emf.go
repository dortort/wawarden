package metrics

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	EMFNamespace = "WaWarden"

	unitCount = "Count"
	unitNone  = "None"
)

type emfMetric struct {
	name   string
	family string
	gauge  bool
}

var emfMetrics = []emfMetric{
	{name: "Paired", family: "wawarden_paired", gauge: true},
	{name: "Connected", family: "wawarden_connected", gauge: true},
	{name: "MessagesIngested", family: "wawarden_messages_ingested_total"},
	{name: "PolicyDenials", family: "wawarden_policy_denials_total"},
	{name: "Panics", family: "wawarden_panics_total"},
	{name: "SendsRejected", family: "wawarden_sends_rejected_total"},
	{name: "AuthFailures", family: "wawarden_auth_failures_total"},
	{name: "AdminAuthFailures", family: "wawarden_admin_auth_failures_total"},
}

type emfMetricDefinition struct {
	Name string `json:"Name"`
	Unit string `json:"Unit"`
}

type emfDirective struct {
	Namespace  string                `json:"Namespace"`
	Dimensions [][]string            `json:"Dimensions"`
	Metrics    []emfMetricDefinition `json:"Metrics"`
}

type emfMetadata struct {
	Timestamp         int64          `json:"Timestamp"`
	CloudWatchMetrics []emfDirective `json:"CloudWatchMetrics"`
}

type EMF struct {
	reg  *Registry
	out  io.Writer
	now  func() time.Time
	mu   sync.Mutex
	last map[string]float64
}

func NewEMF(reg *Registry, out io.Writer, now func() time.Time) *EMF {
	if reg == nil || out == nil {
		panic("metrics: NewEMF needs a registry and an output")
	}
	if now == nil {
		now = time.Now
	}
	return &EMF{reg: reg, out: out, now: now, last: map[string]float64{}}
}

func (e *EMF) Emit() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	at := e.now().UnixMilli()
	total := map[string]any{}
	definitions := make([]emfMetricDefinition, 0, len(emfMetrics))
	var lines [][]byte
	for _, m := range emfMetrics {
		labels, readings := e.reg.read(m.family)
		unit := unitCount
		if m.gauge {
			unit = unitNone
		}
		definitions = append(definitions, emfMetricDefinition{Name: m.name, Unit: unit})
		sum := 0.0
		for _, r := range readings {
			if m.gauge {
				sum += r.value
				continue
			}
			delta := e.delta(m.family+"\xff"+strings.Join(r.labelValues, "\xff"), r.value)
			sum += delta
			if len(labels) == 0 || delta <= 0 {
				continue
			}
			root := map[string]any{m.name: delta}
			for i, l := range labels {
				root[l] = r.labelValues[i]
			}
			line, err := emfLine(at, [][]string{labels}, []emfMetricDefinition{{Name: m.name, Unit: unit}}, root)
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
		total[m.name] = sum
	}
	line, err := emfLine(at, [][]string{{}}, definitions, total)
	if err != nil {
		return err
	}
	for _, l := range append([][]byte{line}, lines...) {
		if _, err := e.out.Write(l); err != nil {
			return err
		}
	}
	return nil
}

func (e *EMF) delta(key string, value float64) float64 {
	d := value - e.last[key]
	e.last[key] = value
	return d
}

func emfLine(at int64, dimensions [][]string, definitions []emfMetricDefinition, root map[string]any) ([]byte, error) {
	root["_aws"] = emfMetadata{
		Timestamp:         at,
		CloudWatchMetrics: []emfDirective{{Namespace: EMFNamespace, Dimensions: dimensions, Metrics: definitions}},
	}
	b, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
