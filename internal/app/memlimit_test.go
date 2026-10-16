package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dortort/wawarden/internal/logx"
)

func init() {
	processMemory = hostMemory{
		lookupEnv: func(string) (string, bool) { return "", false },
		sys:       fstest.MapFS{},
		limit: func(limit int64) int64 {
			if limit >= 0 {
				panic("an app test tried to change the soft memory limit of the test process")
			}
			return math.MaxInt64
		},
	}
}

type memLimitRuntime struct {
	current int64
	set     []int64
}

func (r *memLimitRuntime) limit(limit int64) int64 {
	previous := r.current
	if limit >= 0 {
		r.set = append(r.set, limit)
		r.current = limit
	}
	return previous
}

func memLimitEnv(value string, set bool) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name != memoryLimitEnv {
			return "", false
		}
		return value, set
	}
}

func memLimitFile(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

func memLimitEvents(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatalf("decode log line: %v in %q", err, out)
		}
		if rec["event"] == "memory_limit" {
			events = append(events, rec)
		}
	}
}

func TestMemoryLimitSource(t *testing.T) {
	const (
		mib     = int64(1) << 20
		garbage = "synthetic-garbage-limit"
	)
	nearSentinel := int64(cgroupUnlimited) - 1
	cases := []struct {
		name     string
		env      string
		envSet   bool
		current  int64
		sys      fstest.MapFS
		wantSet  []int64
		source   string
		wantSoft int64
	}{
		{name: "cgroup v2 max sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("max\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "cgroup v2 max does not fall back to v1",
			sys:    fstest.MapFS{cgroupV2Memory: memLimitFile("max\n"), cgroupV1Memory: memLimitFile("536870912\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a cgroup v2 number sets 80 percent of it", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("536870912\n")},
			wantSet: []int64{429496730}, source: memorySourceCgroup, wantSoft: 429496730},
		{name: "cgroup v2 wins over v1",
			sys:     fstest.MapFS{cgroupV2Memory: memLimitFile("536870912\n"), cgroupV1Memory: memLimitFile("1073741824\n")},
			wantSet: []int64{429496730}, source: memorySourceCgroup, wantSoft: 429496730},
		{name: "a cgroup v1 value sets 80 percent of it", sys: fstest.MapFS{cgroupV1Memory: memLimitFile("1073741824\n")},
			wantSet: []int64{858993460}, source: memorySourceCgroup, wantSoft: 858993460},
		{name: "the cgroup v1 unlimited value sets nothing", sys: fstest.MapFS{cgroupV1Memory: memLimitFile("9223372036854771712\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a limit just below the unlimited bound is scaled without overflow",
			sys:     fstest.MapFS{cgroupV2Memory: memLimitFile(strconv.FormatInt(nearSentinel, 10))},
			wantSet: []int64{nearSentinel - nearSentinel/5}, source: memorySourceCgroup, wantSoft: nearSentinel - nearSentinel/5},
		{name: "cgroup v2 garbage sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile(garbage)},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "cgroup v1 garbage sets nothing", sys: fstest.MapFS{cgroupV1Memory: memLimitFile(garbage + "\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "an empty cgroup file sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a zero cgroup limit sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("0\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a negative cgroup limit sets nothing", sys: fstest.MapFS{cgroupV1Memory: memLimitFile("-1\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "an out of range cgroup limit sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("99999999999999999999\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a fractional cgroup limit sets nothing", sys: fstest.MapFS{cgroupV2Memory: memLimitFile("536870912.5\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "an unreadable cgroup v2 file does not fall back to v1",
			sys:    fstest.MapFS{cgroupV2Memory + "/entry": memLimitFile("536870912\n"), cgroupV1Memory: memLimitFile("536870912\n")},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "missing cgroup files set nothing", sys: fstest.MapFS{},
			source: memorySourceNone, wantSoft: math.MaxInt64},
		{name: "a finite GOMEMLIMIT is left alone", env: "512MiB", envSet: true, current: 512 * mib,
			sys:    fstest.MapFS{cgroupV2Memory: memLimitFile("1073741824\n")},
			source: memorySourceEnv, wantSoft: 512 * mib},
		{name: "GOMEMLIMIT off is left alone", env: "off", envSet: true,
			sys:    fstest.MapFS{cgroupV2Memory: memLimitFile("1073741824\n")},
			source: memorySourceEnv, wantSoft: math.MaxInt64},
		{name: "an empty GOMEMLIMIT is unset", env: "", envSet: true,
			sys:     fstest.MapFS{cgroupV2Memory: memLimitFile("536870912\n")},
			wantSet: []int64{429496730}, source: memorySourceCgroup, wantSoft: 429496730},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := tc.current
			if current == 0 {
				current = math.MaxInt64
			}
			fake := &memLimitRuntime{current: current}
			var out bytes.Buffer
			logger := logx.New(logx.NewWriter(&out), slog.LevelInfo)

			hostMemory{lookupEnv: memLimitEnv(tc.env, tc.envSet), sys: tc.sys, limit: fake.limit}.apply(logger)

			if !slices.Equal(fake.set, tc.wantSet) {
				t.Fatalf("SetMemoryLimit calls = %v, want %v", fake.set, tc.wantSet)
			}
			events := memLimitEvents(t, out.String())
			if len(events) != 1 {
				t.Fatalf("memory_limit events = %v, want one", events)
			}
			if got := events[0]["source"]; got != tc.source {
				t.Fatalf("source = %v, want %s", got, tc.source)
			}
			if got, want := events[0]["soft_limit_bytes"], json.Number(strconv.FormatInt(tc.wantSoft, 10)); got != want {
				t.Fatalf("soft_limit_bytes = %v, want %v", got, want)
			}
			if strings.Contains(out.String(), garbage) {
				t.Fatalf("the log repeats the cgroup file: %s", out.String())
			}
		})
	}
}

func TestMemoryLimitIsSetAtStart(t *testing.T) {
	fake := &memLimitRuntime{current: math.MaxInt64}
	saved := processMemory
	processMemory = hostMemory{
		lookupEnv: memLimitEnv("", false),
		sys:       fstest.MapFS{cgroupV2Memory: memLimitFile("536870912\n")},
		limit:     fake.limit,
	}
	t.Cleanup(func() { processMemory = saved })

	_, logs, stop := start(t, testConfig(t, ""), noClients{})
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	if !slices.Equal(fake.set, []int64{429496730}) {
		t.Fatalf("SetMemoryLimit calls = %v, want one of 429496730", fake.set)
	}
	events := logs.find("memory_limit")
	if len(events) != 1 || events[0]["source"] != memorySourceCgroup || events[0]["soft_limit_bytes"] != float64(429496730) {
		t.Fatalf("memory_limit events = %v, want one from the cgroup of 429496730 bytes", events)
	}
}
