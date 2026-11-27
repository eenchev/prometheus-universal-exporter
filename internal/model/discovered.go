package model

// The discovered_targets of a static target file: static targets written
// once for every target their discovery finds. The target allocator
// (internal/allocator) reads every kind of source. The exporter reads an
// entry whose sources are all dns_sd ones and asks them itself
// (config/staticdiscovery.go); these are the entries it reads.

// DNSSDConfig looks names up in the DNS as Prometheus's dns_sd_configs
// does: SRV records, each a target at its target host and port, or A or
// AAAA records, each a target at its address and Port (internal/discovery).
type DNSSDConfig struct {
	// Names are looked up as absolute names, so the search domains of the
	// system's resolver are never appended to them.
	Names []string `yaml:"names"`
	// Type is SRV, the default, A or AAAA.
	Type string `yaml:"type"`
	// Port is the port of an A or AAAA source's targets; an SRV record
	// carries its own.
	Port int `yaml:"port"`
	// Server, host:port, is asked instead of the system's nameservers.
	Server          string   `yaml:"server"`
	RefreshInterval Duration `yaml:"refresh_interval"`
	Timeout         Duration `yaml:"timeout"`
}

// DiscoverySource is a source of an entry the exporter reads: a dns_sd one,
// with the keys every source has. FileSD, HTTPSD and Kubernetes are the
// allocator's other kinds, read only so that one is refused by its name.
type DiscoverySource struct {
	Name       string            `yaml:"name"`
	DNSSD      *DNSSDConfig      `yaml:"dns_sd"`
	Labels     map[string]string `yaml:"labels"`
	MaxTargets int               `yaml:"max_targets"`
	FileSD     any               `yaml:"file_sd" json:"-"`
	HTTPSD     any               `yaml:"http_sd" json:"-"`
	Kubernetes any               `yaml:"kubernetes" json:"-"`
}

// DiscoveredEntry is a discovered_targets entry as the exporter reads it.
type DiscoveredEntry struct {
	// Name begins the name of every target it makes, <name>_<address>.
	Name string
	// Template makes a target of each discovered address; "" takes the
	// address as it is.
	Template string
	Sources  []DiscoverySource
	// Keys are the entry's other keys and their values, each target's own,
	// and Head the file's top-level keys but targets, those it is read
	// with, each a YAML mapping as text. Sample is the entry loaded as a
	// target at a sample address, which the checks against the
	// configuration read.
	Keys, Head string
	Sample     StaticTarget
}
