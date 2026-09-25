package exporter

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// The Go runtime collects garbage harder as its heap nears GOMEMLIMIT, instead
// of growing past the container's memory limit into an OOM kill. Setting
// GOMEMLIMIT to the whole container limit leaves nothing for what the Go
// heap does not count: the Python workers, which are processes of their
// own, and the runtime's own overhead. --runtime.memory-limit-ratio sets the
// limit to a share of the container's memory limit, read from its cgroup at
// startup, so the rest is left to them. GOMEMLIMIT set in the environment
// wins: it is the operator's own choice.

// cgroupUnlimited is the least value cgroup v1 writes for no limit: close
// to the largest int64, rounded down to a page.
const cgroupUnlimited = 1 << 60

// ValidateMemoryLimitRatio refuses a --runtime.memory-limit-ratio outside
// 0 to 1: 0 turns it off, and more than the whole limit would let the heap
// grow past it.
func ValidateMemoryLimitRatio(ratio float64) error {
	if math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return fmt.Errorf("--runtime.memory-limit-ratio must be from 0, for off, to 1, got %v", ratio)
	}
	return nil
}

// A container's limit is on its own cgroup, which /proc/self/cgroup names:
// "0::/kubepods/.../container" under cgroup v2, and a line naming the memory
// controller, such as "4:memory:/docker/abc", under v1. With a cgroup
// namespace, as recent Kubernetes and Docker give a container, that path is
// "/", and the container's cgroup is mounted at /sys/fs/cgroup itself;
// without one, the limit is only in the cgroup the path names, while the
// root holds the host's "max". So the limit is read along that path, from
// the process's cgroup up to the root, and the smallest found is the one in
// force: a pod's limit above a container without one counts too.

// cgroupPaths reads the process's cgroup v2 path and cgroup v1 memory path
// from /proc/self/cgroup under root; each is "" when there is none.
func cgroupPaths(root string) (v2, v1 string) {
	raw, err := os.ReadFile(filepath.Join(root, "proc/self/cgroup"))
	if err != nil {
		return "/", "/"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			v2 = parts[2]
		case slices.Contains(strings.Split(parts[1], ","), "memory"):
			v1 = parts[2]
		}
	}
	return v2, v1
}

// containerMemoryLimit reads the container's memory limit under root, the
// file system root; ok is false when there is none.
func containerMemoryLimit(root string) (limit int64, source string, ok bool) {
	v2, v1 := cgroupPaths(root)
	consider := func(mount, dir, file string) {
		for {
			rel := filepath.Join(mount, dir, file)
			if raw, err := os.ReadFile(filepath.Join(root, rel)); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && n > 0 && n < cgroupUnlimited && (!ok || n < limit) {
					limit, source, ok = n, "/"+rel, true
				}
			}
			if dir == "/" || dir == "." || dir == "" {
				return
			}
			dir = filepath.Dir(dir)
		}
	}
	if v2 != "" {
		consider("sys/fs/cgroup", v2, "memory.max")
	}
	if v1 != "" {
		consider("sys/fs/cgroup/memory", v1, "memory.limit_in_bytes")
	}
	return limit, source, ok
}

// ApplyMemoryLimitRatio sets the Go memory limit to ratio of the container's
// memory limit, unless ratio is 0 or GOMEMLIMIT is set, and logs what it did.
func ApplyMemoryLimitRatio(ratio float64, logger *slog.Logger) {
	// Without a container limit the Go default stays, as logged: not an
	// error for the exporter.
	_, _ = applyMemoryLimitRatio(ratio, "/", os.Getenv("GOMEMLIMIT"), debug.SetMemoryLimit, logger)
}

var errNoMemoryLimit = errors.New("no container memory limit was found")

func applyMemoryLimitRatio(ratio float64, root, fromEnv string, set func(int64) int64, logger *slog.Logger) (int64, error) {
	if ratio <= 0 {
		return 0, nil
	}
	if fromEnv != "" {
		logger.Info("GOMEMLIMIT is set in the environment, so --runtime.memory-limit-ratio is not applied", "gomemlimit", fromEnv)
		return 0, nil
	}
	limit, source, ok := containerMemoryLimit(root)
	if !ok {
		logger.Info("--runtime.memory-limit-ratio has no container memory limit to take a share of; the Go runtime keeps its default", "ratio", ratio)
		return 0, errNoMemoryLimit
	}
	goLimit := int64(float64(limit) * ratio)
	set(goLimit)
	logger.Info("Go memory limit set from the container's", "go_memory_limit_bytes", goLimit, "container_memory_limit_bytes", limit, "ratio", ratio, "source", source)
	return goLimit, nil
}
