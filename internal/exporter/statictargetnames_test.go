package exporter

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A static target is told to be in force by its name where the following
// of its file noted the names of the file's targets
// (followedConfig.targetNames, reconcile.go), where the targets were gone
// through for every scrape that ended and every read of the static targets
// endpoint that named targets. The tests here hold that what is told is
// what going through them told.

// staticTargetInForceAsItWas is Server.staticTargetInForce as it was while
// it went through the targets in force for every name.
func staticTargetInForceAsItWas(s *Server, name string) bool {
	for _, target := range s.manager.StaticTargets() {
		if target.Name == name {
			return true
		}
	}
	return false
}

// requestedStaticTargetsAsItWas is Server.requestedStaticTargets as it was
// while it went through the targets in force for every read that named
// targets.
func requestedStaticTargetsAsItWas(s *Server, query url.Values) (map[string]bool, error) {
	values, given := query[staticTargetsParam]
	if !given {
		return nil, nil
	}
	names := map[string]bool{}
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names[name] = true
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the %s parameter names no static target; give one or more names, separated by commas, or leave it out to read every target", staticTargetsParam)
	}
	known := map[string]bool{}
	for _, target := range s.manager.StaticTargets() {
		known[target.Name] = true
	}
	var unknown []string
	for name := range names {
		if !known[name] {
			unknown = append(unknown, strconv.Quote(name))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("no static target is named %s", strings.Join(unknown, ", "))
	}
	return names, nil
}

// countTargetScans counts, until the test ends, the times the targets of a
// static target file are gone through, to tell whether one has a name or to
// note the names they have (targetsScannedHook).
func countTargetScans(t *testing.T) *atomic.Int64 {
	t.Helper()
	scans := &atomic.Int64{}
	hook := func() { scans.Add(1) }
	targetsScannedHook.Store(&hook)
	t.Cleanup(func() { targetsScannedHook.Store(nil) })
	return scans
}

// targetNameTestNames are the names the generated static target files draw
// from: names that differ only by case or by blanks, one of two words, the
// empty one, which no loaded file has, and at the end two no file is given.
var targetNameTestNames = []string{"a", "A", "a ", " a", "a b", "a  b", "b", "é", "", "t_c1", "never_configured", "\t"}

// targetNameTestFile is a static target file of count targets named at
// random, given to the server as it is and not read from a file: two
// targets may share a name, which no loaded file has
// (config.ValidateStaticTargets) and which shows that a name is told of the
// file when any target has it.
func targetNameTestFile(random *rand.Rand, count int) *model.StaticTargetFile {
	file := &model.StaticTargetFile{}
	names := targetNameTestNames[:len(targetNameTestNames)-2]
	for i := range count {
		file.Targets = append(file.Targets, model.StaticTarget{Name: names[random.IntN(len(names))], Collector: "x", Target: "http://127.0.0.1:9/" + strconv.Itoa(i)})
	}
	return file
}

// targetNameTestQuery is a query of a read of the static targets endpoint:
// mostly one that names targets, some of the file and some not, in one
// value or several, with blanks and empty names between the commas, and
// now and then one without the parameter or with one that names nothing.
func targetNameTestQuery(random *rand.Rand) url.Values {
	query := url.Values{"other": {"kept"}}
	switch random.IntN(8) {
	case 0:
		return query
	case 1:
		query[staticTargetsParam] = []string{[]string{"", " ", ",", " , ,"}[random.IntN(4)]}
		return query
	}
	for range 1 + random.IntN(2) {
		var names []string
		for range 1 + random.IntN(3) {
			name := targetNameTestNames[random.IntN(len(targetNameTestNames))]
			if random.IntN(3) == 0 {
				name = " " + name + " "
			}
			names = append(names, name)
		}
		query[staticTargetsParam] = append(query[staticTargetsParam], strings.Join(names, ","))
	}
	return query
}

// A static target told to be in force by its name is the one going through
// the targets in force found. Over 200 generated static target files (60
// under the race detector) of no target to 12, some of them no file at
// all, with targets that share a name, names that differ only by case or by
// blanks and the empty name, and for every name of theirs and two no file
// has: a scrape that ends is told what it was told, and a read of the
// static targets endpoint that names targets is given the same names or
// refused with the same words, over 12 generated queries each time — while
// the file in force is the one followed, once another file is in force
// that nothing has followed yet, which is then the one asked about, once
// that one is followed, and for a server that follows nothing. While the
// file in force is the one followed its targets are gone through for
// neither, where they were gone through once for each ask; for a file not
// followed they are gone through as they were, once for each. The
// following of a file with targets goes through them once, to note their
// names.
func TestAStaticTargetToldByItsNameIsTheOneGoingThroughThemFound(t *testing.T) {
	scans := countTargetScans(t)
	files := alloctest.UnlessRaced(200, 60)
	shared, inForce, absent, refused, given := 0, 0, 0, 0, 0
	for seed := range files {
		random := rand.New(rand.NewPCG(uint64(seed), 41))
		cfg := &model.Config{Collectors: []model.Collector{testutil.Collector("x", "text")}}
		manager := config.NewManager(cfg, "", slog.New(slog.DiscardHandler))
		// One file in ten is none at all, and one in ten has no target.
		var file *model.StaticTargetFile
		if seed%10 != 0 {
			count := 1 + random.IntN(12)
			if seed%10 == 1 {
				count = 0
			}
			file = targetNameTestFile(random, count)
			manager.SetTargets("", file)
		}
		server := NewServer(manager, "python3", slog.New(slog.DiscardHandler))
		seen := map[string]bool{}
		for _, target := range staticTargetsOf(file) {
			if seen[target.Name] {
				shared++
			}
			seen[target.Name] = true
		}
		// check asks about every name, and makes the generated reads, and
		// says how many times the targets were gone through for each ask:
		// perAsk of them.
		check := func(what string, perAsk int64) {
			t.Helper()
			for _, name := range targetNameTestNames {
				scans.Store(0)
				got, want := server.staticTargetInForce(name), staticTargetInForceAsItWas(server, name)
				if got != want {
					t.Fatalf("file %d, %s: a target named %q is in force %v, want %v", seed, what, name, got, want)
				}
				if gone := scans.Load(); gone != perAsk {
					t.Fatalf("file %d, %s: the targets were gone through %d times to tell whether one is named %q, want %d", seed, what, gone, name, perAsk)
				}
				if got {
					inForce++
				} else {
					absent++
				}
			}
			for range 12 {
				query := targetNameTestQuery(random)
				scans.Store(0)
				got, err := server.requestedStaticTargets(query)
				gone := scans.Load()
				want, wantErr := requestedStaticTargetsAsItWas(server, query)
				if !reflect.DeepEqual(got, want) || (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() {
					t.Fatalf("file %d, %s: the read %v is given the targets %v and refused with %v; want %v and %v", seed, what, query, got, err, want, wantErr)
				}
				// A read that names no target asks about none.
				asked := perAsk
				if want == nil && (wantErr == nil || strings.Contains(wantErr.Error(), "names no static target")) {
					asked = 0
				}
				if gone != asked {
					t.Fatalf("file %d, %s: the targets were gone through %d times for the read %v, want %d", seed, what, gone, query, asked)
				}
				if err != nil {
					refused++
				} else if got != nil {
					given++
				}
			}
		}
		check("with the file in force followed", 0)
		// Another file is put in force, as a reload puts it, and nothing has
		// followed it yet: it is the one asked about.
		other := targetNameTestFile(random, 1+random.IntN(12))
		manager.SetTargets("", other)
		if followed := server.followed.Load(); followed.targets != file || server.manager.StaticTargetFile() != other {
			t.Fatalf("file %d: the server follows another file than the one it started with, or the manager holds another than the one put in force", seed)
		}
		check("with a file in force that is not followed yet", 1)
		scans.Store(0)
		if followed := server.reconcile(); followed.targets != other {
			t.Fatalf("file %d: the file put in force is not followed", seed)
		}
		if gone := scans.Load(); gone != 1 {
			t.Fatalf("file %d: the following of a file with targets went through them %d times, want once, to note their names", seed, gone)
		}
		check("with that file followed", 0)
		server.followed.Store(nil)
		check("for a server that follows nothing", 1)
	}
	// What the files must have had to show anything.
	if floor := int64(files); int64(shared) < floor || int64(inForce) < 10*floor || int64(absent) < 10*floor || int64(refused) < 10*floor || int64(given) < floor {
		t.Errorf("the %d files had %d targets that share an earlier one's name, %d asks for a name in force and %d for one that is not, and %d reads refused and %d given their targets; too few of one of them to show anything", files, shared, inForce, absent, refused, given)
	}
}
