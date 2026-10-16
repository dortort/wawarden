package app

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	memoryLimitEnv  = "GOMEMLIMIT"
	cgroupV2Memory  = "fs/cgroup/memory.max"
	cgroupV1Memory  = "fs/cgroup/memory/memory.limit_in_bytes"
	cgroupUnlimited = 1 << 62 // cgroup v1 reports no limit as the int64 maximum rounded down to a page

	memorySourceEnv    = "env"
	memorySourceCgroup = "cgroup"
	memorySourceNone   = "none"
)

type hostMemory struct {
	lookupEnv func(string) (string, bool)
	sys       fs.FS
	limit     func(int64) int64
}

var processMemory = hostMemory{lookupEnv: os.LookupEnv, sys: os.DirFS("/sys"), limit: debug.SetMemoryLimit}

func (h hostMemory) apply(logger *slog.Logger) {
	source := memorySourceNone
	if value, set := h.lookupEnv(memoryLimitEnv); set && value != "" {
		source = memorySourceEnv
	} else if limit, ok := h.cgroupLimit(); ok {
		h.limit(limit - limit/5)
		source = memorySourceCgroup
	}
	logger.Info("soft memory limit", slog.String("event", "memory_limit"),
		slog.String("source", source), slog.Int64("soft_limit_bytes", h.limit(-1)))
}

func (h hostMemory) cgroupLimit() (int64, bool) {
	raw, err := fs.ReadFile(h.sys, cgroupV2Memory)
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = fs.ReadFile(h.sys, cgroupV1Memory)
	}
	if err != nil {
		return 0, false
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || limit <= 0 || limit >= cgroupUnlimited {
		return 0, false
	}
	return limit, true
}
