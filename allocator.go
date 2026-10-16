package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
	"github.com/eenchev/prometheus-universal-exporter/internal/allocclient"
	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Allocator mode: --allocator.url has the exporter take its configuration
// from the target allocator (cmd/pue-allocator) rather than from files of
// its own. The allocator serves each replica a bundle, the configuration
// with its collector files and the replica's share of the static targets;
// the exporter writes it into --allocator.cache-dir and loads it there with
// the same loader and manager as mounted files (internal/allocclient). A
// restart resumes from the cache directory, and a replica that has no
// bundle yet serves its endpoints, unready, until the first arrives.

// allocatorFlags are the allocator mode's flags as given.
type allocatorFlags struct {
	url, cacheDir, member, advertise, tokenFile string
}

// allocatorMode is allocator mode as the flags set it up.
type allocatorMode struct {
	url, cacheDir, member, advertise, tokenFile string
}

// configFile and staticTargetsFile are the files of the bundle in the cache
// directory, which the manager reads.
func (a *allocatorMode) configFile() string { return filepath.Join(a.cacheDir, allocapi.ConfigFile) }
func (a *allocatorMode) staticTargetsFile() string {
	return filepath.Join(a.cacheDir, allocapi.StaticTargetsFile)
}

// awaitingSentence is what /probe answers before the first bundle.
func (a *allocatorMode) awaitingSentence() string {
	return "no configuration yet: waiting for the allocator at " + a.url
}

// fileFlags are the flags that name the exporter's own configuration files,
// which allocator mode leaves to the allocator.
var fileFlags = []string{"config.file", "static-targets-file"}

// allocatorOnlyFlags are the flags read only in allocator mode.
var allocatorOnlyFlags = []string{"allocator.cache-dir", "allocator.member", "allocator.advertise", "allocator.token-file"}

// setUp checks the allocator mode's flags and works out their defaults: nil
// without --allocator.url. Allocator mode is refused with a flag that names
// a configuration file, given explicitly, and the flags of allocator mode
// without --allocator.url: either would be left unread without a word.
func (f allocatorFlags) setUp(flags *flag.FlagSet, listenAddress string) (*allocatorMode, error) {
	given := map[string]bool{}
	flags.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
	if f.url == "" {
		for _, name := range allocatorOnlyFlags {
			if given[name] {
				return nil, fmt.Errorf("--%s is only read in allocator mode; give --allocator.url too, or leave it out", name)
			}
		}
		return nil, nil
	}
	for _, name := range fileFlags {
		if given[name] {
			return nil, fmt.Errorf("--allocator.url and --%s cannot be given together: in allocator mode the configuration and the static targets come from the allocator; leave out --%s", name, name)
		}
	}
	if _, err := allocclient.ParseURL(f.url); err != nil {
		return nil, err
	}
	if f.cacheDir == "" {
		return nil, errors.New("--allocator.url needs --allocator.cache-dir, the directory the bundle from the allocator is kept in")
	}
	a := &allocatorMode{url: f.url, cacheDir: f.cacheDir, member: f.member, advertise: f.advertise, tokenFile: f.tokenFile}
	if a.member == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			return nil, errors.New("the host name is unknown; set --allocator.member")
		}
		a.member = host
	}
	if a.advertise == "" {
		advertise, err := defaultAdvertise(listenAddress)
		if err != nil {
			return nil, err
		}
		a.advertise = advertise
	} else if host, port, err := net.SplitHostPort(a.advertise); err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("--allocator.advertise %q must be host:port, the address Prometheus sends this exporter's probes to", a.advertise)
	}
	return a, nil
}

// interfaceAddrs lists the host's addresses; a variable for the tests.
var interfaceAddrs = net.InterfaceAddrs

// defaultAdvertise is --allocator.advertise left out: the listen address's
// host when it is one address that is not a loopback one, or else the
// host's first address that is not, IPv4 before IPv6, with the listen port.
func defaultAdvertise(listenAddress string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
		return net.JoinHostPort(host, port), nil
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("listing the host's addresses for --allocator.advertise: %w", err)
	}
	var v6 string
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsUnspecified() {
			continue
		}
		if ipNet.IP.To4() != nil {
			return net.JoinHostPort(ipNet.IP.String(), port), nil
		}
		if v6 == "" {
			v6 = ipNet.IP.String()
		}
	}
	if v6 != "" {
		return net.JoinHostPort(v6, port), nil
	}
	return "", errors.New("the host has no address other than a loopback one; set --allocator.advertise")
}

// checkFiles are the files --dry-run checks in allocator mode: the bundle in
// the cache directory, which a restart would start with, and its static
// targets when it has any.
func (a *allocatorMode) checkFiles() (configFile, staticTargetsFile string) {
	if exists(a.staticTargetsFile()) {
		staticTargetsFile = a.staticTargetsFile()
	}
	return a.configFile(), staticTargetsFile
}

// startManager makes the manager of allocator mode. With a bundle in the
// cache directory that loads it starts with that, and returns its
// generation; with none, or one that does not load, the manager awaits the
// first bundle from the allocator (config.NewAwaitingManager).
func (a *allocatorMode) startManager(logger *slog.Logger, pythonPath string, loadOptions, staticTargetsLoadOptions []config.LoadOption, watch bool) (*config.Manager, string) {
	configFile, targetsFile := a.configFile(), a.staticTargetsFile()
	if err := os.MkdirAll(a.cacheDir, 0o700); err != nil {
		logger.Warn("the allocator's cache directory cannot be made; bundles cannot be kept until it can", "cache_dir", a.cacheDir, "error", err.Error())
	}
	generation := allocclient.CachedGeneration(a.cacheDir)
	var manager *config.Manager
	if conf, targets, stamp, err := loadCached(configFile, targetsFile, pythonPath, loadOptions, staticTargetsLoadOptions, watch); err == nil {
		config.LogNotices(logger, configFile, conf)
		manager = config.NewManager(conf, configFile, logger)
		manager.UseStamp(stamp)
		manager.SetTargets(targetsFile, targets)
		logger.Info("starting with the bundle in the allocator's cache directory", "cache_dir", a.cacheDir, "generation", generation)
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("the bundle in the allocator's cache directory does not load; waiting for the allocator", "cache_dir", a.cacheDir, "error", err.Error())
		}
		generation = ""
		manager = config.NewAwaitingManager(configFile, logger)
		manager.SetTargets(targetsFile, nil)
	}
	manager.AllowAbsentTargets()
	return manager, generation
}

// loadCached loads the bundle in the cache directory as startup loads files:
// the configuration, and the static targets when there are any. An error
// that is os.ErrNotExist says there is no bundle.
func loadCached(configFile, targetsFile, pythonPath string, loadOptions, staticTargetsLoadOptions []config.LoadOption, watch bool) (*model.Config, *model.StaticTargetFile, config.Stamp, error) {
	if _, err := os.Stat(configFile); err != nil {
		return nil, nil, config.Stamp{}, err
	}
	configStamp := config.TakeStamp(configFile, true, loadOptions...)
	load := config.Load
	if watch {
		load = func(path string, opts ...config.LoadOption) (*model.Config, error) {
			return config.LoadStamped(path, &configStamp, opts...)
		}
	}
	conf, err := load(configFile, loadOptions...)
	if err == nil {
		err = config.ValidatePythonScripts(pythonPath, conf)
	}
	if err != nil {
		return nil, nil, configStamp, fmt.Errorf("configuration %s: %w", configFile, noNotExist(err))
	}
	if !exists(targetsFile) {
		return conf, nil, configStamp, nil
	}
	targets, err := config.LoadStaticTargets(targetsFile, staticTargetsLoadOptions...)
	if err == nil {
		err = config.ValidateStaticTargets(targets)
	}
	if err == nil {
		err = config.ValidateStaticTargetsAgainst(targets, conf)
	}
	if err != nil {
		return nil, nil, configStamp, fmt.Errorf("static target file %s: %w", targetsFile, noNotExist(err))
	}
	return conf, targets, configStamp, nil
}

// exists reports whether there is a file at path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// noNotExist keeps a missing file a configuration names from reading as a
// cache directory without a bundle.
func noNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New(err.Error())
	}
	return err
}

// startClient makes the allocator client and runs it until ctx ends.
func (a *allocatorMode) startClient(ctx context.Context, manager *config.Manager, generation string, logger *slog.Logger) (*allocclient.Client, error) {
	client, err := allocclient.New(allocclient.Options{
		URL: a.url, CacheDir: a.cacheDir, Member: a.member, Advertise: a.advertise, TokenFile: a.tokenFile,
		Manager: manager, Logger: logger, Generation: generation,
	})
	if err != nil {
		return nil, err
	}
	go client.Run(ctx)
	return client, nil
}

// shutdownHeartbeatTimeout bounds the heartbeat that tells the allocator the
// exporter is shutting down, sent before --web.shutdown-delay begins.
const shutdownHeartbeatTimeout = 2 * time.Second
