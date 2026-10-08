package exporter

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

func writeCgroup(t *testing.T, name, content string) string {
	t.Helper()
	return writeCgroupFiles(t, map[string]string{name: content})
}

func writeCgroupFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The Go memory limit is the ratio of the container's, from cgroup v2 or v1;
// none is set without a container limit, with a ratio of 0, or with
// GOMEMLIMIT in the environment.
func TestTheMemoryLimitRatio(t *testing.T) {
	logger := testutil.QuietLogger(t)
	var set int64
	record := func(n int64) int64 { set = n; return 0 }
	for name, tc := range map[string]struct {
		root, env string
		ratio     float64
		want      int64
		err       error
	}{
		"cgroup v2":          {root: writeCgroup(t, "sys/fs/cgroup/memory.max", "1073741824\n"), ratio: 0.8, want: 858993459},
		"cgroup v1":          {root: writeCgroup(t, "sys/fs/cgroup/memory/memory.limit_in_bytes", "536870912"), ratio: 0.5, want: 268435456},
		"v2 without a limit": {root: writeCgroup(t, "sys/fs/cgroup/memory.max", "max\n"), ratio: 0.8, err: errNoMemoryLimit},
		"v1 without a limit": {root: writeCgroup(t, "sys/fs/cgroup/memory/memory.limit_in_bytes", "9223372036854771712"), ratio: 0.8, err: errNoMemoryLimit},
		"no cgroup":          {root: t.TempDir(), ratio: 0.8, err: errNoMemoryLimit},
		// Without a cgroup namespace the root holds the host's max, and the
		// limit is on the container's own cgroup, which /proc/self/cgroup
		// names.
		"v2 without a namespace": {root: writeCgroupFiles(t, map[string]string{
			"proc/self/cgroup":                          "0::/kubepods/pod1/c1\n",
			"sys/fs/cgroup/memory.max":                  "max\n",
			"sys/fs/cgroup/kubepods/pod1/c1/memory.max": "1073741824\n",
			"sys/fs/cgroup/kubepods/pod1/memory.max":    "max\n",
		}), ratio: 0.5, want: 536870912},
		"a pod limit above the container's": {root: writeCgroupFiles(t, map[string]string{
			"proc/self/cgroup":                          "0::/kubepods/pod1/c1\n",
			"sys/fs/cgroup/kubepods/pod1/c1/memory.max": "max\n",
			"sys/fs/cgroup/kubepods/pod1/memory.max":    "268435456\n",
		}), ratio: 1, want: 268435456},
		"v2 in a namespace": {root: writeCgroupFiles(t, map[string]string{
			"proc/self/cgroup":         "0::/\n",
			"sys/fs/cgroup/memory.max": "1073741824\n",
		}), ratio: 0.25, want: 268435456},
		"v1 without a namespace": {root: writeCgroupFiles(t, map[string]string{
			"proc/self/cgroup":                                      "12:cpu,cpuacct:/docker/abc\n4:memory:/docker/abc\n1:name=systemd:/docker/abc\n",
			"sys/fs/cgroup/memory/memory.limit_in_bytes":            "9223372036854771712",
			"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": "536870912",
		}), ratio: 0.5, want: 268435456},
		"off":                  {root: writeCgroup(t, "sys/fs/cgroup/memory.max", "1073741824"), ratio: 0},
		"GOMEMLIMIT is chosen": {root: writeCgroup(t, "sys/fs/cgroup/memory.max", "1073741824"), ratio: 0.8, env: "300MiB"},
	} {
		t.Run(name, func(t *testing.T) {
			set = 0
			got, err := applyMemoryLimitRatio(tc.ratio, tc.root, tc.env, record, logger)
			if got != tc.want || set != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("got %d, set %d, err %v; want %d, %v", got, set, err, tc.want, tc.err)
			}
		})
	}
	for _, bad := range []float64{-0.1, 1.5} {
		if ValidateMemoryLimitRatio(bad) == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if ValidateMemoryLimitRatio(0) != nil || ValidateMemoryLimitRatio(1) != nil {
		t.Error("0 and 1 refused")
	}
}

// oldValidateMemoryLimitRatio is ValidateMemoryLimitRatio as it was before
// the floor, the oracle of the test below.
func oldValidateMemoryLimitRatio(ratio float64) error {
	if math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return fmt.Errorf("--runtime.memory-limit-ratio must be from 0, for off, to 1, got %v", ratio)
	}
	return nil
}

// A ratio above 0 and below 0.1 sets a Go memory limit of next to nothing,
// so the runtime would collect garbage all the time: it is refused, with a
// message saying the floor and why. Every other ratio gives what it gave
// before, the same error or none, over a table of boundaries and generated
// ratios.
func TestAMemoryLimitRatioBelowATenthIsRefused(t *testing.T) {
	for _, small := range []float64{1e-300, 1e-5, 0.0001, 0.05, 0.0999999, math.Nextafter(0.1, 0), math.SmallestNonzeroFloat64} {
		err := ValidateMemoryLimitRatio(small)
		if err == nil || !strings.Contains(err.Error(), "or at least 0.1") || !strings.Contains(err.Error(), "spends its time collecting garbage") {
			t.Errorf("%v: %v, want it refused under the floor", small, err)
		}
	}
	ratios := []float64{0, 0.1, 0.10000001, math.Nextafter(0.1, 1), 0.5, 0.8, 0.95, 1, math.Nextafter(1, 2), 1.5, -0.1, -1e-300, math.Copysign(0, -1), math.NaN(), math.Inf(1), math.Inf(-1)}
	random := rand.New(rand.NewPCG(48, 49))
	for range 2000 {
		ratios = append(ratios, 0.1+random.Float64()*0.9, random.Float64()*4-2)
	}
	for _, ratio := range ratios {
		if !(ratio > 0 && ratio < 0.1) {
			now, before := ValidateMemoryLimitRatio(ratio), oldValidateMemoryLimitRatio(ratio)
			if fmt.Sprint(now) != fmt.Sprint(before) {
				t.Errorf("%v: %v, before %v", ratio, now, before)
			}
		}
	}
}
