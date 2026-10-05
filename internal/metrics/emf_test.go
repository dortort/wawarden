package metrics

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

var emfUnits = []string{"Seconds", "Microseconds", "Milliseconds", "Bytes", "Kilobytes", "Megabytes", "Gigabytes", "Terabytes", "Bits",
	"Kilobits", "Megabits", "Gigabits", "Terabits", "Percent", "Count", "Bytes/Second", "Kilobytes/Second", "Megabytes/Second",
	"Gigabytes/Second", "Terabytes/Second", "Bits/Second", "Kilobits/Second", "Megabits/Second", "Gigabits/Second", "Terabits/Second",
	"Count/Second", "None"}

type emfRecord struct {
	root       map[string]any
	dimensions [][]string
	metrics    map[string]string
}

func parseEMF(t *testing.T, line string, at time.Time) emfRecord {
	t.Helper()
	var root map[string]any
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("not a JSON object: %q", line)
	}
	if len(line) > 1<<20 {
		t.Fatalf("an event of %d bytes exceeds 1 MB", len(line))
	}
	var meta struct {
		Timestamp         json.Number `json:"Timestamp"`
		CloudWatchMetrics []struct {
			Namespace  string     `json:"Namespace"`
			Dimensions [][]string `json:"Dimensions"`
			Metrics    []struct {
				Name string `json:"Name"`
				Unit string `json:"Unit"`
			} `json:"Metrics"`
		} `json:"CloudWatchMetrics"`
	}
	raw, err := json.Marshal(root["_aws"])
	if err != nil || json.Unmarshal(raw, &meta) != nil {
		t.Fatalf("_aws is not the metadata object: %q", line)
	}
	if ms, err := meta.Timestamp.Int64(); err != nil || ms != at.UnixMilli() {
		t.Fatalf("Timestamp %q, want the integer milliseconds %d", meta.Timestamp, at.UnixMilli())
	}
	if len(meta.CloudWatchMetrics) != 1 {
		t.Fatalf("%d directives, want one", len(meta.CloudWatchMetrics))
	}
	d := meta.CloudWatchMetrics[0]
	if d.Namespace != "WaWarden" || len(d.Dimensions) == 0 || len(d.Metrics) == 0 || len(d.Metrics) > 100 {
		t.Fatalf("directive %+v: namespace WaWarden, at least one dimension set and 1 to 100 metrics", d)
	}
	rec := emfRecord{root: root, dimensions: d.Dimensions, metrics: map[string]string{}}
	for _, set := range d.Dimensions {
		if set == nil || len(set) > 30 {
			t.Fatalf("dimension set %v: present, at most 30 keys", set)
		}
		for _, key := range set {
			if v, ok := root[key].(string); !ok || v == "" || len(v) > 1024 {
				t.Fatalf("dimension %q is %#v at the root, want a string of 1 to 1024 characters", key, root[key])
			}
		}
	}
	for _, m := range d.Metrics {
		if m.Name == "" || len(m.Name) > 255 || !slices.Contains(emfUnits, m.Unit) {
			t.Fatalf("metric definition %+v", m)
		}
		n, ok := root[m.Name].(json.Number)
		if !ok {
			t.Fatalf("metric %s is %#v at the root, want a number", m.Name, root[m.Name])
		}
		if f, err := n.Float64(); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			t.Fatalf("metric %s = %v", m.Name, n)
		}
		rec.metrics[m.Name] = n.String()
	}
	return rec
}

func emitLines(t *testing.T, e *EMF, out *bytes.Buffer) []string {
	t.Helper()
	out.Reset()
	if err := e.Emit(); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	var lines []string
	for line := range strings.Lines(out.String()) {
		if !strings.HasSuffix(line, "\n") {
			t.Fatalf("line %q is not terminated", line)
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	return lines
}

func TestEMFLinesFollowTheSpecification(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	paired, connected := reg.Gauge("wawarden_paired", "x"), reg.Gauge("wawarden_connected", "x")
	ingested := reg.Counter("wawarden_messages_ingested_total", "x")
	reg.Counter("wawarden_policy_denials_total", "x")
	reg.Counter("wawarden_sends_rejected_total", "x")
	auth := reg.Counter("wawarden_auth_failures_total", "x")
	panics := reg.CounterVec("wawarden_panics_total", "x", "name")
	paired.Set(1)
	connected.Set(1)
	for range 42 {
		ingested.Inc()
	}
	auth.Inc()
	panics.With("engine.ingest").Inc()
	panics.With("engine.ingest").Inc()
	panics.With("listeners.client").Inc()

	var out bytes.Buffer
	e := NewEMF(reg, &out, func() time.Time { return at })
	lines := emitLines(t, e, &out)
	if len(lines) != 3 {
		t.Fatalf("%d lines, want the dimensionless line and one per panic name:\n%s", len(lines), out.String())
	}
	want := `{"AdminAuthFailures":0,"AuthFailures":1,"Connected":1,"MessagesIngested":42,"Paired":1,"Panics":3,"PolicyDenials":0,"SendsRejected":0,` +
		`"_aws":{"Timestamp":1791201600000,"CloudWatchMetrics":[{"Namespace":"WaWarden","Dimensions":[[]],"Metrics":[` +
		`{"Name":"Paired","Unit":"None"},{"Name":"Connected","Unit":"None"},{"Name":"MessagesIngested","Unit":"Count"},{"Name":"PolicyDenials","Unit":"Count"},` +
		`{"Name":"Panics","Unit":"Count"},{"Name":"SendsRejected","Unit":"Count"},{"Name":"AuthFailures","Unit":"Count"},{"Name":"AdminAuthFailures","Unit":"Count"}]}]}}`
	if lines[0] != want {
		t.Fatalf("dimensionless line\n got %s\nwant %s", lines[0], want)
	}
	total := parseEMF(t, lines[0], at)
	if !slices.EqualFunc(total.dimensions, [][]string{{}}, slices.Equal) || len(total.metrics) != 8 {
		t.Fatalf("dimensionless line has dimensions %v and %d metrics", total.dimensions, len(total.metrics))
	}
	byName := map[string]string{}
	for _, line := range lines[1:] {
		rec := parseEMF(t, line, at)
		if !slices.EqualFunc(rec.dimensions, [][]string{{"name"}}, slices.Equal) || len(rec.metrics) != 1 || rec.metrics["Panics"] == "" {
			t.Fatalf("per-name line %s: one dimension set with name only, and Panics only", line)
		}
		byName[rec.root["name"].(string)] = rec.metrics["Panics"]
	}
	if byName["engine.ingest"] != "2" || byName["listeners.client"] != "1" {
		t.Fatalf("per-name Panics %v", byName)
	}

	connected.Set(0)
	ingested.Inc()
	lines = emitLines(t, e, &out)
	if len(lines) != 1 {
		t.Fatalf("%d lines without a new panic, want only the dimensionless line", len(lines))
	}
	again := parseEMF(t, lines[0], at)
	if again.metrics["Connected"] != "0" || again.metrics["Paired"] != "1" || again.metrics["MessagesIngested"] != "1" ||
		again.metrics["Panics"] != "0" || again.metrics["AuthFailures"] != "0" {
		t.Fatalf("second line %v: gauges are current values and counters deltas, zero when unchanged", again.metrics)
	}
	panics.With("listeners.client").Inc()
	lines = emitLines(t, e, &out)
	if len(lines) != 2 || parseEMF(t, lines[0], at).metrics["Panics"] != "1" || parseEMF(t, lines[1], at).root["name"] != "listeners.client" {
		t.Fatalf("lines after one more panic: %q", lines)
	}
}

func TestEMFWithoutTheFamiliesWritesZeros(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	lines := emitLines(t, NewEMF(NewRegistry(), &out, func() time.Time { return at }), &out)
	if len(lines) != 1 {
		t.Fatalf("lines %q", lines)
	}
	for name, v := range parseEMF(t, lines[0], at).metrics {
		if v != "0" {
			t.Fatalf("%s = %s in an empty registry", name, v)
		}
	}
}

func TestEMFRefusesValuesTheFormatCannotCarry(t *testing.T) {
	reg := NewRegistry()
	reg.Gauge("wawarden_paired", "x").Set(math.NaN())
	var out bytes.Buffer
	if err := NewEMF(reg, &out, nil).Emit(); err == nil || out.Len() != 0 {
		t.Fatalf("Emit with a NaN gauge = %v, wrote %q", err, out.String())
	}
}

func TestNewEMFNeedsARegistryAndAnOutput(t *testing.T) {
	for _, f := range []func(){
		func() { NewEMF(nil, &bytes.Buffer{}, nil) },
		func() { NewEMF(NewRegistry(), nil, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			f()
		}()
	}
}
