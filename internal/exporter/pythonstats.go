package exporter

import (
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/transform"
)

// The worker pool counts, per collector, what the collector's Python
// workers do (transform.PythonStats): the workers started, stopped and
// alive, and how the runs ended. It keeps the counts under the collector's
// name and knows nothing of configurations. So the counts are held to the
// collector's other statistics, and through them follow a reload as those
// do (reconcile.go):
//
//   - A reload that removes a collector retires its worker statistics where
//     it retires the collector's own, under statsMu (followLocked). They are
//     no longer shown, and a collector added again under the name starts
//     from zero. A collector whose definition changed keeps them, and an
//     unchanged one keeps everything, as each keeps its counters.
//   - A trip takes the worker statistics its scripts count in under the same
//     lock. It finds them by its own statistics, which it was given for the
//     configuration it read (statsSince). It gets the collector's while its
//     own are the collector's, and the pool's departed when those are
//     retired, whether that was when the trip took them or before it ran its
//     first script (pythonStatsOf). So a trip that read a collector a reload
//     has since removed is counted under no collector's name, however far it
//     had come at the reload. It makes no statistics under the name for a
//     collector added again to take over. It hands them to its runs
//     (WithScriptTimer). Each run holds them from its start to its end, as
//     the worker it runs in does: a reload meanwhile retires them where they
//     are held.
//
//   - A reader shows a collector's worker statistics with the collector's
//     own, which it read first, and finds them under the same lock by those
//     (pythonStatsShown). Two reloads may come between the two readings, one
//     that removes the collector and one that adds it again. The reader then
//     does not show what the re-added collector has counted since with the
//     creation time of the one it read.
//
// What is retired is not lost to the pool: it goes on counting for the pool
// as a whole, in the http_exporter_python_pool_ families. A worker belongs
// to the statistics it was started for, and to no others while it lives.
// The pool gives it to no run that counts elsewhere, and stops it when its
// run ends once they are retired (transform.PythonPool). So a collector's
// series show the workers the collector started, and a collector added
// again starts its own.

// runsPython reports whether a collector runs Python, in its transform or in
// a pre-script: whether it has workers, and their series.
func runsPython(c *model.Collector) bool {
	return c.Transform.Type == "python" || c.Transform.PreScript != ""
}

// pythonStatsOf is the worker statistics the scripts of a trip count in,
// when the trip itself counts in stats, the statistics it was given for the
// collector name. They are those kept under the name while stats are the
// collector's, and the pool's departed once a reload has retired them. It
// is asked under statsMu, under which a reload retires both. So the
// statistics kept under the name are those of the collector stats belong
// to, not of one a reload has brought back since.
func (s *Server) pythonStatsOf(stats *serverStats, name string) *transform.PythonStats {
	pool := transform.PythonWorkers()
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if stats.retired.Load() {
		return pool.Departed()
	}
	return pool.Stats(name)
}

// pythonStats is the worker statistics the scripts of the trip j count in,
// or nil for a collector that runs no Python. They are those of the
// statistics the trip counts in. For a trip that counts in none, a debug
// probe's, they are the ones it names.
func (s *Server) pythonStats(j *collectJob) *transform.PythonStats {
	switch {
	case !runsPython(j.collector):
		return nil
	case j.rec.collector != nil:
		return s.pythonStatsOf(j.rec.collector, j.collector.Name)
	}
	return j.python
}

// pythonStatsSince is the worker statistics the scripts of a debug probe
// count in, for a probe that read its collector c in the configuration
// followed at generation. The probe itself is counted nowhere, but its
// scripts ran in the collector's workers, whose statistics count them. They
// are found by the statistics a probe that read the collector then would
// count in (statsSince). So a debug probe of a collector a reload has since
// removed is held to what a probe is.
func (s *Server) pythonStatsSince(generation uint64, c *model.Collector) *transform.PythonStats {
	if !runsPython(c) {
		return nil
	}
	return s.pythonStatsOf(s.statsSince(generation, c.Name), c.Name)
}

// pythonStatsShown is the worker statistics a reader shows with stats, the
// statistics it read for the collector name some time before. They are the
// ones kept under the name while stats are the collector's. They are nil,
// which have counted nothing, when none are kept, and when a reload has
// retired stats since the reader took them. What is kept under the name is
// then of a collector a later reload added again, or nothing. The reader
// shows the collector it read with no worker counts, as it shows one a
// reload has just removed, and not with another's. Asking makes no
// statistics and is done under statsMu, as a trip's asking is
// (pythonStatsOf).
func (s *Server) pythonStatsShown(stats *serverStats, name string) *transform.PythonStats {
	pool := transform.PythonWorkers()
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if stats.retired.Load() {
		return nil
	}
	return pool.Kept(name)
}
