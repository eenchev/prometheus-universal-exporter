package repository

import (
	"os"
	"slices"
	"testing"
	"unsafe"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A collector's definition is over a kilobyte, and a loop that takes each
// collector as a value copies it: the read of the self-metrics did, for
// every collector, with the lock held that every probe takes to find its
// collector's statistics. Such a copy allocates nothing, so no bound on
// allocations shows it; the lint does (gocritic's rangeValCopy), while the
// check is on and the size it reports from is no more than a collector's.
// This holds the two together: a threshold raised past a collector's size,
// or the check dropped, would leave the copy unreported.
func TestTheLintReportsALoopThatCopiesACollector(t *testing.T) {
	text, err := os.ReadFile(".golangci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var lint struct {
		Linters struct {
			Enable   []string `yaml:"enable"`
			Settings struct {
				Gocritic struct {
					EnabledChecks []string `yaml:"enabled-checks"`
					Settings      struct {
						RangeValCopy struct {
							SizeThreshold int `yaml:"sizeThreshold"`
						} `yaml:"rangeValCopy"`
					} `yaml:"settings"`
				} `yaml:"gocritic"`
			} `yaml:"settings"`
		} `yaml:"linters"`
	}
	if err := yaml.Unmarshal(text, &lint); err != nil {
		t.Fatal(err)
	}
	gocritic := lint.Linters.Settings.Gocritic
	if !slices.Contains(lint.Linters.Enable, "gocritic") || !slices.Contains(gocritic.EnabledChecks, "rangeValCopy") {
		t.Fatalf(".golangci.yml enables %v with gocritic's %v: gocritic's rangeValCopy is not among them, and a loop that copies each collector is not reported", lint.Linters.Enable, gocritic.EnabledChecks)
	}
	threshold, collector := gocritic.Settings.RangeValCopy.SizeThreshold, int(unsafe.Sizeof(model.Collector{}))
	if threshold <= 0 || threshold > collector {
		t.Errorf("rangeValCopy reports elements of %d bytes and more, and a collector is %d: set linters.settings.gocritic.settings.rangeValCopy.sizeThreshold in .golangci.yml to no more than that", threshold, collector)
	}
}
