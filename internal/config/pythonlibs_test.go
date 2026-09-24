package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

func TestSupportedPythonLibrariesValidate(t *testing.T) {
	for _, lib := range []string{"lxml", "PyYAML", "yaml", "python-dateutil", "dateutil"} {
		cfg := model.Config{Collectors: []model.Collector{pythonLibraryCollector(lib)}}
		if err := Validate(&cfg); err != nil {
			t.Errorf("%s: %v", lib, err)
		}
	}
}

func TestBeautifulSoupIsRejectedWithAPointerToLxml(t *testing.T) {
	for _, lib := range []string{"beautifulsoup4", "bs4"} {
		cfg := model.Config{Collectors: []model.Collector{pythonLibraryCollector("lxml", lib)}}
		err := Validate(&cfg)
		if err == nil || !strings.Contains(err.Error(), "no longer installs") || !strings.Contains(err.Error(), "lxml.html") {
			t.Errorf("%s: err=%v", lib, err)
		}
	}
	cfg := model.Config{Collectors: []model.Collector{pythonLibraryCollector("requests")}}
	if err := Validate(&cfg); err == nil || !strings.Contains(err.Error(), `unsupported Python library "requests"; the supported libraries are lxml, PyYAML and python-dateutil`) {
		t.Errorf("err=%v", err)
	}
}
