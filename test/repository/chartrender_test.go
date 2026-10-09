package repository

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The chart's options, rendered with helm: what each renders, and each value
// that must fail rendering does, with its message. CI installs helm for any
// change to the chart; without helm on the PATH, as on a developer machine
// without it, these tests are skipped and the text tests in charts_test.go
// still run.

func requireHelm(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on the PATH")
	}
	return helm
}

// helmTemplate renders the chart at dir with args, and returns what helm
// printed and whether it succeeded.
func helmTemplate(t *testing.T, helm, dir string, args ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(helm, append([]string{"template", "test", dir}, args...)...) // #nosec G204 -- the test's own arguments
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err == nil
}

// staticTargetsValues writes a values file with static targets, one of them
// exported over OTLP, merged from an x- anchor.
func staticTargetsValues(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	values := `staticTargets:
  enabled: true
  monitor:
    enabled: false
  data: |
    x-shared: &shared
      collector: example
      target: http://x
    interval: 1m
    targets:
      - <<: *shared
        name: exported
        export_via_otlp: true
      - <<: *shared
        name: kept
config:
  data:
    config.yaml: |
      otlp:
        enabled: true
        endpoint: http://otel:4318/v1/metrics
      collectors:
        - name: example
          request: {type: http}
          transform: {type: regex}
          metrics:
            - name: v
              expression: 'v=(\d+)'
` + extra
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChartRendersItsOptions(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		args        []string
		want, avoid []string
	}{
		"defaults": {
			want:  []string{"replicas: 1\n", "runAsUser: 65532", `"--runtime.memory-limit-ratio=0.8"`, "timeoutSeconds: 3", "failureThreshold: 5", "failureThreshold: 3"},
			avoid: []string{"kind: HorizontalPodAutoscaler", "kind: PodDisruptionBudget", "--probe.max-concurrent", "--python.max-workers", "GOMEMLIMIT", "terminationGracePeriodSeconds", "sessionAffinity", "--web.enable-probe-debug", "kind: ServiceMonitor", "kind: NetworkPolicy"},
		},
		"limits": {
			args: []string{"--set", "server.probeMaxConcurrent=64", "--set", "server.pythonMaxWorkers=8", "--set", "goMemLimit.ratio=0.6"},
			want: []string{`"--probe.max-concurrent=64"`, `"--python.max-workers=8"`, `"--runtime.memory-limit-ratio=0.6"`},
		},
		"no memory limit ratio": {
			args:  []string{"--set", "goMemLimit.enabled=false"},
			avoid: []string{"--runtime.memory-limit-ratio"},
		},
		"probe timings": {
			args: []string{"--set", "livenessProbe.timeoutSeconds=7", "--set", "readinessProbe.periodSeconds=4"},
			want: []string{"timeoutSeconds: 7", "periodSeconds: 4", "path: /health", "path: /ready"},
		},
		"disruption budget": {
			args: []string{"--set", "podDisruptionBudget.enabled=true"},
			want: []string{"kind: PodDisruptionBudget", "maxUnavailable: 1\n", "app.kubernetes.io/instance: test"},
		},
		"disruption budget by availability": {
			args:  []string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.minAvailable=50%"},
			want:  []string{"minAvailable: 50%"},
			avoid: []string{"maxUnavailable: 1\n"},
		},
		"autoscaling": {
			args:  []string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.maxReplicas=6", "--set", "autoscaling.targetMemoryUtilizationPercentage=70"},
			want:  []string{"kind: HorizontalPodAutoscaler", "maxReplicas: 6", "averageUtilization: 80", "averageUtilization: 70", "kind: Deployment\n    name: test-prometheus-universal-exporter\n"},
			avoid: []string{"replicas: 1\n"},
		},
		"network policy alone": {
			args:  []string{"--set", "networkPolicy.enabled=true"},
			want:  []string{"kind: NetworkPolicy", "policyTypes:\n    - Ingress\n  ingress:\n    - ports:\n        - port: http\n          protocol: TCP"},
			avoid: []string{"- Egress", "port: 53"},
		},
		"network policy with egress rules": {
			args: []string{"--set", "networkPolicy.enabled=true", "--set-json", `networkPolicy.egress=[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"}}]}]`},
			want: []string{"- Egress", "cidr: 10.0.0.0/8", "- port: 53\n          protocol: UDP", "- port: 53\n          protocol: TCP"},
		},
		"network policy with egress rules and no DNS": {
			args:  []string{"--set", "networkPolicy.enabled=true", "--set", "networkPolicy.allowDNS=false", "--set-json", `networkPolicy.egress=[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"}}]}]`},
			want:  []string{"- Egress"},
			avoid: []string{"port: 53"},
		},
		"a probe monitor, and the self monitor with it": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"}]`},
			want: []string{"replacement: test-prometheus-universal-exporter.default.svc:8080", "kind: ServiceMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
		},
		"the self monitor as a pod monitor": {
			args:  []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"}]`, "--set", "selfMetrics.type=pod"},
			want:  []string{"kind: PodMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
			avoid: []string{"kind: ServiceMonitor\nmetadata:\n  name: test-prometheus-universal-exporter-self"},
		},
		"the self monitor on a cluster that serves it": {
			args: []string{"--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"},
			want: []string{"name: test-prometheus-universal-exporter-self"},
		},
		"a namespace override in the monitors' address": {
			args: []string{"--set", "namespaceOverride=monitoring", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`},
			want: []string{"replacement: test-prometheus-universal-exporter.monitoring.svc:8080"},
		},
		"a compound watch interval": {
			args: []string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=1m30s"},
			want: []string{`"--config.watch-interval=1m30s"`},
		},
		"a disruption budget of no eviction": {
			args: []string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.maxUnavailable=0"},
			want: []string{"maxUnavailable: 0\n"},
		},
		"no token mounted on the default account": {
			args: []string{"--set", "serviceAccount.create=false"},
			want: []string{"serviceAccountName: default\n      automountServiceAccountToken: false"},
		},
		"probe debug": {
			args: []string{"--set", "server.probeDebug=true"},
			want: []string{"- --web.enable-probe-debug\n"},
		},
		"session affinity": {
			args: []string{"--set", "service.sessionAffinity=ClientIP"},
			want: []string{"sessionAffinity: ClientIP"},
		},
		"pod scheduling and metadata": {
			args: []string{"--set", "priorityClassName=monitoring",
				"--set", "topologySpreadConstraints[0].maxSkew=1", "--set", "topologySpreadConstraints[0].topologyKey=topology.kubernetes.io/zone", "--set", "topologySpreadConstraints[0].whenUnsatisfiable=ScheduleAnyway",
				"--set", "topologySpreadConstraints[1].maxSkew=2", "--set", "topologySpreadConstraints[1].topologyKey=kubernetes.io/hostname", "--set", "topologySpreadConstraints[1].whenUnsatisfiable=DoNotSchedule", "--set", "topologySpreadConstraints[1].labelSelector.matchLabels.own=yes",
				"--set", "podLabels.team=obs", "--set", "podAnnotations.note=pod", "--set", "podAnnotations.checksum/config=mine"},
			want: []string{`priorityClassName: "monitoring"`, "topologyKey: topology.kubernetes.io/zone", "- labelSelector:\n            matchLabels:\n              app.kubernetes.io/instance: test\n              app.kubernetes.io/name: prometheus-universal-exporter\n          maxSkew: 1",
				"matchLabels:\n              own: \"yes\"\n          maxSkew: 2", "team: obs", "note: pod"},
			avoid: []string{"checksum/config: mine"},
		},
		"a longer shutdown raises the grace period": {
			args: []string{"--set", "server.shutdownTimeout=1m"},
			want: []string{"terminationGracePeriodSeconds: 75"},
		},
		"an IPv6 wildcard listen address": {
			args: []string{"--set", "server.listenAddress=[::]:9115"},
			want: []string{`"--web.listen-address=[::]:9115"`, "containerPort: 9115"},
		},
		// Only the readiness probe refuses a grace period of its own.
		"a liveness probe's grace period": {
			args: []string{"--set", "livenessProbe.terminationGracePeriodSeconds=10"},
			want: []string{"terminationGracePeriodSeconds: 10"},
		},
		"Prometheus durations on the monitors": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1m30s","scrapeTimeout":"1500ms"}]`, "--set", "selfMetrics.interval=1d"},
			want: []string{"interval: 1m30s", "scrapeTimeout: 1500ms", "interval: 1d"},
		},
		// The delay counts too: 20s, the default 15s timeout and 10s more.
		"a longer shutdown delay raises the grace period": {
			args: []string{"--set", "server.shutdownDelay=20s"},
			want: []string{`"--web.shutdown-delay=20s"`, "terminationGracePeriodSeconds: 45"},
		},
		"the default rolling update": {
			want: []string{"  strategy:\n    type: RollingUpdate\n    rollingUpdate:\n      maxSurge: 1\n      maxUnavailable: 0\n  selector:"},
		},
		// Kubernetes refuses rollingUpdate beside Recreate, and the default
		// values carry it.
		"the Recreate strategy alone": {
			args:  []string{"--set", "strategy.type=Recreate"},
			want:  []string{"  strategy:\n    type: Recreate\n  selector:"},
			avoid: []string{"rollingUpdate", "maxSurge"},
		},
		"a rolling update of its own": {
			args: []string{"--set", "strategy.rollingUpdate.maxSurge=25%", "--set", "strategy.rollingUpdate.maxUnavailable=1"},
			want: []string{"    type: RollingUpdate\n    rollingUpdate:\n      maxSurge: 25%\n      maxUnavailable: 1\n"},
		},
		// A scrape timeout may be as long as its interval, in whatever units.
		"a scrape timeout as long as its interval": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1m","scrapeTimeout":"60s"}]`, "--set", "selfMetrics.interval=1d", "--set", "selfMetrics.scrapeTimeout=24h"},
			want: []string{"interval: 1m\n      scrapeTimeout: 60s", "interval: 1d\n      scrapeTimeout: 24h"},
		},
		// One of the two alone is compared with nothing: Prometheus's own
		// default stands for the other, and the chart does not know it.
		"a scrape timeout without an interval": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","scrapeTimeout":"5m"}]`},
			want: []string{"scrapeTimeout: 5m"},
		},
		// What is not rendered is not checked.
		"timings of monitors that are off": {
			args: []string{"--set", "selfMetrics.enabled=false", "--set", "selfMetrics.interval=10s", "--set", "selfMetrics.scrapeTimeout=5m",
				"--set", "staticTargets.monitor.interval=10s", "--set", "staticTargets.monitor.scrapeTimeout=5m",
				"--set-json", `monitors=[{"name":"apps","enabled":false,"type":"service","collector":"example","interval":"10s","scrapeTimeout":"5m"}]`},
			avoid: []string{"kind: ServiceMonitor"},
		},
		"an Ingress to the Service": {
			args: []string{"--set", "ingress.enabled=true"},
			want: []string{"kind: Ingress", "service:\n                name: test-prometheus-universal-exporter\n"},
		},
		"credential mounts at paths of their own": {
			args: []string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=web", "--set", "webAuth.mountPath=/secrets/web/",
				"--set", "targetAuth.enabled=true", "--set", "targetAuth.secretName=target", "--set", "targetAuth.mountPath=/secrets/target",
				"--set-json", `extraVolumes=[{"name":"ca","secret":{"secretName":"ca"}}]`, "--set-json", `extraVolumeMounts=[{"name":"ca","mountPath":"/secrets"}]`},
			want: []string{`mountPath: "/secrets/web/"`, `mountPath: "/secrets/target"`, "mountPath: /secrets\n"},
		},
		// A disabled auth needs neither a type nor a Secret, and renders
		// nothing.
		"a monitor's auth that is off": {
			args:  []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":false}},{"name":"pods","enabled":true,"type":"pod","collector":"example","auth":{"enabled":false,"secretName":""}}]`},
			want:  []string{"name: test-prometheus-universal-exporter-apps", "name: test-prometheus-universal-exporter-pods"},
			avoid: []string{"authorization:", "basicAuth:"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if !ok {
				t.Fatalf("rendering failed:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in the rendered chart", want)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(out, avoid) {
					t.Errorf("%q is rendered", avoid)
				}
			}
		})
	}
}

func TestChartRefusesInvalidOptions(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"negative probe limit":                 {[]string{"--set", "server.probeMaxConcurrent=-1"}, "probeMaxConcurrent"},
		"fractional worker limit":              {[]string{"--set", "server.pythonMaxWorkers=1.5"}, "pythonMaxWorkers"},
		"memory ratio above 1":                 {[]string{"--set", "goMemLimit.ratio=1.5"}, "ratio"},
		"memory ratio of 0":                    {[]string{"--set", "goMemLimit.ratio=0"}, "ratio"},
		"a probe's own check":                  {[]string{"--set", "livenessProbe.httpGet.path=/x"}, "livenessProbe"},
		"both disruption budgets":              {[]string{"--set", "podDisruptionBudget.enabled=true", "--set", "podDisruptionBudget.minAvailable=1", "--set", "podDisruptionBudget.maxUnavailable=1"}, "sets both minAvailable and maxUnavailable"},
		"fewer maximum than minimum":           {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.minReplicas=5"}, "is below autoscaling.minReplicas"},
		"autoscaling on nothing":               {[]string{"--set", "autoscaling.enabled=true", "--set", "autoscaling.targetCPUUtilizationPercentage=null"}, "needs something to scale on"},
		"a flag the chart manages":             {[]string{"--set", "extraArgs[0]=--probe.max-concurrent=3"}, "server.probeMaxConcurrent"},
		"the memory ratio as an extra":         {[]string{"--set", "extraArgs[0]=--runtime.memory-limit-ratio=0.5"}, "goMemLimit.ratio"},
		"a probe monitor without the Service":  {[]string{"--set", "service.enabled=false", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`}, "service.enabled=false leaves out"},
		"a monitor without a collector":        {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service"}]`}, "names no collector"},
		"a monitor of an unknown collector":    {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"nope"}]`}, `names collector "nope", which config.data does not define`},
		"the self monitor without the Service": {[]string{"--set", "service.enabled=false", "--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"}, "selfMetrics.type service"},
		"a zero watch interval":                {[]string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=0s"}, "watchConfigInterval"},
		"a zero compound watch interval":       {[]string{"--set", "server.watchConfig=true", "--set", "server.watchConfigInterval=0m0s"}, "watchConfigInterval"},
		"probe debug as an extra":              {[]string{"--set", "extraArgs[0]=--web.enable-probe-debug"}, "server.probeDebug"},
		"a pod label the chart sets":           {[]string{"--set", "podLabels.app\\.kubernetes\\.io/name=x"}, "podLabels sets app.kubernetes.io/name"},
		"a spread without a topology key":      {[]string{"--set", "topologySpreadConstraints[0].maxSkew=1"}, "topologyKey"},
		// The chart renders the collector and target parameters itself; a
		// params entry of either would be a second key of the same name.
		"a monitor's params.collector": {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","params":{"collector":["other"]}}]`}, "sets params.collector; the chart renders the collector parameter itself, so set the entry's .collector instead"},
		"a monitor's params.target":    {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","params":{"target":["http://x"]}}]`}, "sets params.target"},
		// Neither the kubelet's probes nor the Service reach a loopback host.
		"a loopback IPv4 listen address": {[]string{"--set", "server.listenAddress=127.0.0.1:8080"}, "listens on a loopback address"},
		"a loopback name listen address": {[]string{"--set", "server.listenAddress=localhost:8080"}, "listens on a loopback address"},
		"a loopback IPv6 listen address": {[]string{"--set", "server.listenAddress=[::1]:8080"}, "listens on a loopback address"},
		// A monitor is named <fullname>-<name>: a DNS-1123 label, of its own.
		"an upper-case monitor name":                   {[]string{"--set-json", `monitors=[{"name":"Apps","enabled":true,"type":"service","collector":"example"}]`}, "monitors.0.name"},
		"a monitor name with an underscore":            {[]string{"--set-json", `monitors=[{"name":"my_apps","enabled":true,"type":"service","collector":"example"}]`}, "monitors.0.name"},
		"two monitors of one name":                     {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"apps","enabled":true,"type":"pod","collector":"example"}]`}, `both named "apps"`},
		"a monitor named after the self one":           {[]string{"--set-json", `monitors=[{"name":"self","enabled":true,"type":"service","collector":"example"}]`}, "the self-metrics monitor"},
		"a monitor named after the static targets one": {[]string{"--set-json", `monitors=[{"name":"static-targets","enabled":true,"type":"pod","collector":"example"}]`}, "the static targets monitor"},
		// A Service of type ExternalName has no endpoints to probe through.
		"an ExternalName Service": {[]string{"--set", "service.type=ExternalName"}, "service.type"},
		// Kubernetes refuses terminationGracePeriodSeconds on a readiness probe.
		"a readiness probe's grace period": {[]string{"--set", "readinessProbe.terminationGracePeriodSeconds=10"}, "terminationGracePeriodSeconds"},
		// The Prometheus Operator takes Prometheus durations: whole numbers,
		// down to milliseconds.
		"a fractional monitor interval":              {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1.5m"}]`}, "monitors.0.interval"},
		"a monitor scrape timeout in microseconds":   {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","scrapeTimeout":"500us"}]`}, "monitors.0.scrapeTimeout"},
		"a self monitor interval in nanoseconds":     {[]string{"--set", "selfMetrics.interval=30000000000ns"}, "selfMetrics.interval"},
		"a fractional static targets scrape timeout": {[]string{"--set", "staticTargets.monitor.scrapeTimeout=0.5s"}, "staticTargets.monitor.scrapeTimeout"},
		// The schema requires a monitor's name, so no template names one
		// after its place in the list.
		"a monitor without a name": {[]string{"--set-json", `monitors=[{"enabled":true,"type":"service","collector":"example"}]`}, "monitors.0"},
		// An enabled auth without a type rendered no credential, or the
		// exporter's own, and one without a Secret an empty name.
		"a service monitor's auth without a type":   {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"secretName":"token"}}]`}, "monitors.0.auth"},
		"a pod monitor's auth without a type":       {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"secretName":"token"}}]`}, "monitors.0.auth"},
		"a monitor's bearer auth without a Secret":  {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"bearer"}}]`}, "monitors.0.auth"},
		"a monitor's basic auth of an empty Secret": {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"type":"basic","secretName":""}}]`}, "monitors.0.auth.secretName"},
		// A monitor's port is a port's name, never its number, and its
		// namespaceSelector the Prometheus Operator's: any or matchNames.
		"a monitor's empty port":                          {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","port":""}]`}, "monitors.0.port"},
		"a monitor's port number":                         {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","port":8080}]`}, "monitors.0.port"},
		"a monitor's port number as a string":             {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","port":"8080"}]`}, "monitors.0.port"},
		"a monitor's namespace selector of labels":        {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","namespaceSelector":{"matchLabels":{"team":"a"}}}]`}, "monitors.0.namespaceSelector"},
		"a monitor's namespace selector naming no string": {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"pod","collector":"example","namespaceSelector":{"matchNames":[""]}}]`}, "monitors.0.namespaceSelector.matchNames.0"},
		// Prometheus refuses a scrape timeout longer than its interval, in
		// every unit a Prometheus duration has.
		"a monitor's scrape timeout over its interval":        {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"10s","scrapeTimeout":"30s"}]`}, "monitors entry apps has scrapeTimeout 30s, longer than its interval 10s"},
		"a pod monitor's scrape timeout over its interval":    {[]string{"--set-json", `monitors=[{"name":"pods","enabled":true,"type":"pod","collector":"example","interval":"1m","scrapeTimeout":"61s"}]`}, "monitors entry pods has scrapeTimeout 61s, longer than its interval 1m"},
		"a scrape timeout over its interval in milliseconds":  {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1s500ms","scrapeTimeout":"1501ms"}]`}, "scrapeTimeout 1501ms, longer than its interval 1s500ms"},
		"a scrape timeout over its interval in hours":         {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"90m","scrapeTimeout":"2h"}]`}, "scrapeTimeout 2h, longer than its interval 90m"},
		"a scrape timeout over its interval in days":          {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1d","scrapeTimeout":"25h"}]`}, "scrapeTimeout 25h, longer than its interval 1d"},
		"a scrape timeout over its interval in weeks":         {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"1w","scrapeTimeout":"8d"}]`}, "scrapeTimeout 8d, longer than its interval 1w"},
		"a scrape timeout over its interval in years":         {[]string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","interval":"52w","scrapeTimeout":"1y"}]`}, "scrapeTimeout 1y, longer than its interval 52w"},
		"the self monitor's scrape timeout over its interval": {[]string{"--set", "selfMetrics.interval=10s", "--set", "selfMetrics.scrapeTimeout=5m"}, "selfMetrics has scrapeTimeout 5m, longer than its interval 10s"},
		// The Ingress routes to the Service.
		"an Ingress without the Service": {[]string{"--set", "ingress.enabled=true", "--set", "service.enabled=false"}, "ingress.enabled routes to the exporter's Service, which service.enabled=false leaves out"},
		// A pod cannot mount two volumes at one path, and a mount at the
		// configuration directory would hide config.yaml; a trailing slash
		// hides no collision.
		"the exporter credential over the configuration":     {[]string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=s", "--set", "webAuth.mountPath=/etc/prometheus-universal-exporter"}, `webAuth.mountPath "/etc/prometheus-universal-exporter" is also the configuration directory`},
		"the exporter credential over the configuration, /":  {[]string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=s", "--set", "webAuth.mountPath=/etc/prometheus-universal-exporter/"}, `webAuth.mountPath "/etc/prometheus-universal-exporter/" is also the configuration directory`},
		"the target credential over the configuration":       {[]string{"--set", "targetAuth.enabled=true", "--set", "targetAuth.secretName=s", "--set", "targetAuth.mountPath=/etc/prometheus-universal-exporter/"}, `targetAuth.mountPath "/etc/prometheus-universal-exporter/" is also the configuration directory`},
		"both credentials at one path":                       {[]string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=s", "--set", "webAuth.mountPath=/secrets/", "--set", "targetAuth.enabled=true", "--set", "targetAuth.secretName=s", "--set", "targetAuth.mountPath=/secrets"}, `webAuth.mountPath "/secrets/" is also targetAuth.mountPath`},
		"an extra mount over the target credential":          {[]string{"--set", "targetAuth.enabled=true", "--set", "targetAuth.secretName=s", "--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/var/run/prometheus-universal-exporter/target-auth/"}]`}, "which the chart already mounts as targetAuth.mountPath"},
		"an extra mount over the target credential's slash":  {[]string{"--set", "targetAuth.enabled=true", "--set", "targetAuth.secretName=s", "--set", "targetAuth.mountPath=/secrets/target/", "--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/secrets/target"}]`}, "which the chart already mounts as targetAuth.mountPath"},
		"an extra mount over the exporter credential":        {[]string{"--set", "webAuth.enabled=true", "--set", "webAuth.secretName=s", "--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/var/run/prometheus-universal-exporter/web-auth"}]`}, "which the chart already mounts as webAuth.mountPath"},
		"an extra mount over the configuration with a slash": {[]string{"--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/etc/prometheus-universal-exporter/"}]`}, "which the chart already mounts as the configuration directory"},
		"two extra mounts at one path":                       {[]string{"--set-json", `extraVolumeMounts=[{"name":"x","mountPath":"/data"},{"name":"y","mountPath":"/data/"}]`}, `extraVolumeMounts uses mountPath "/data/" twice`},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, tc.args...)
			if ok || !namesInHelmError(out, tc.want) {
				t.Fatalf("ok=%v, want an error naming %q:\n%s", ok, tc.want, out)
			}
		})
	}
}

// namesInHelmError reports whether helm's error out names want. Helm writes
// the path of a value the schema refuses as monitors.0.name up to 3.16 and as
// '/monitors/0/name' since, so a dotted path is also looked for in that form.
func namesInHelmError(out, want string) bool {
	if strings.Contains(out, want) {
		return true
	}
	return !strings.ContainsAny(want, " /") && strings.Contains(out, "'/"+strings.ReplaceAll(want, ".", "/")+"'")
}

// Both ways helm has reported a value the schema refuses: up to 3.16, and
// since (these lines are from helm 3.22 in CI).
func TestNamesInHelmError(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		named     bool
	}{
		{"- monitors.0.name: Does not match pattern", "monitors.0.name", true},
		{"- at '/monitors/0/name': 'Apps' does not match pattern", "monitors.0.name", true},
		{"- at '/staticTargets/monitor/scrapeTimeout': '0.5s' does not match pattern", "staticTargets.monitor.scrapeTimeout", true},
		{"- at '/service/type': value must be one of 'ClusterIP', 'NodePort', 'LoadBalancer'", "service.type", true},
		{"- at '/readinessProbe': additional properties 'terminationGracePeriodSeconds' not allowed", "terminationGracePeriodSeconds", true},
		{"- at '/monitors/0/interval': '1.5m' does not match pattern", "monitors.0.name", false},
		{"- at '/monitors/0/nameX': ...", "monitors.0.name", false},
	} {
		if got := namesInHelmError(tc.out, tc.want); got != tc.named {
			t.Errorf("namesInHelmError(%q, %q) = %v, want %v", tc.out, tc.want, got, tc.named)
		}
	}
}

// The notes warn when static targets are rendered with more than one replica
// or with autoscaling, naming the targets exported over OTLP. helm template
// does not render NOTES.txt, so a copy of the chart renders it as a manifest.
func TestChartNotesWarnAboutReplicatedStaticTargets(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "kind: Notes\nnotes: {{ include \"test.notes\" . | toJson }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	render := func(extra string) string {
		out, ok := helmTemplate(t, helm, dir, "-f", staticTargetsValues(t, extra), "--show-only", "templates/notes.yaml")
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		return out
	}
	if out := render(""); strings.Contains(out, "WARNING") {
		t.Fatalf("one replica is warned about:\n%s", out)
	}
	for extra, want := range map[string]string{
		"replicaCount: 2\n": "replicaCount 2",
		"autoscaling:\n  enabled: true\n  maxReplicas: 4\n": "autoscaling up to 4 replicas",
	} {
		out := render(extra)
		for _, fragment := range []string{"WARNING", want, "export_via_otlp (exported)", "duplicate series"} {
			if !strings.Contains(out, fragment) {
				t.Errorf("%q: no %q in:\n%s", extra, fragment, out)
			}
		}
		if strings.Contains(out, "kept") {
			t.Errorf("a target not exported over OTLP is named:\n%s", out)
		}
	}
}

// The release's objects are named after the release, so two releases in one
// namespace do not collide: <release>-<chart>, the release name alone when
// it holds the chart's, and fullnameOverride outright.
func TestChartNamesFollowTheRelease(t *testing.T) {
	helm := requireHelm(t)
	for release, want := range map[string]string{
		"blue":                               "name: blue-prometheus-universal-exporter\n",
		"green":                              "name: green-prometheus-universal-exporter\n",
		"prometheus-universal-exporter":      "name: prometheus-universal-exporter\n",
		"prod-prometheus-universal-exporter": "name: prod-prometheus-universal-exporter\n",
	} {
		cmd := exec.Command(helm, "template", release, chartDir, "--show-only", "templates/deployment.yaml") // #nosec G204 -- the test's own arguments
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("release %s: want %q in\n%s (%v)", release, want, out, err)
		}
	}
	out, ok := helmTemplate(t, helm, chartDir, "--set", "fullnameOverride=exporter", "--show-only", "templates/deployment.yaml")
	if !ok || !strings.Contains(out, "name: exporter\n") {
		t.Errorf("fullnameOverride: %s", out)
	}
	// A static-targets-only release gets its self monitor too.
	out, ok = helmTemplate(t, helm, chartDir, "-f", staticTargetsValues(t, ""), "--set", "staticTargets.monitor.enabled=true")
	if !ok || !strings.Contains(out, "name: test-prometheus-universal-exporter-self") {
		t.Errorf("a static-targets-only release has no self monitor:\n%s", out)
	}
}

// The notes warn about a disruption budget that allows no eviction.
// A chart version deploys the exporter release it was validated against:
// image.tag defaults to empty, which renders Chart.yaml's appVersion, never a
// moving tag such as latest. A tag set in the values wins.
func TestChartImageDefaultsToTheAppVersion(t *testing.T) {
	helm := requireHelm(t)
	chart := readChartMetadata(t)
	dir := chartDir
	out, ok := helmTemplate(t, helm, dir)
	if !ok {
		t.Fatalf("render failed:\n%s", out)
	}
	if want := `image: "ghcr.io/eenchev/prometheus-universal-exporter:` + chart.AppVersion + `"`; !strings.Contains(out, want) {
		t.Errorf("the default image is not the appVersion; want %s", want)
	}
	out, ok = helmTemplate(t, helm, dir, "--set", "image.tag=9.9.9")
	if !ok || !strings.Contains(out, `image: "ghcr.io/eenchev/prometheus-universal-exporter:9.9.9"`) {
		t.Errorf("image.tag=9.9.9 was not rendered as given:\n%s", out)
	}
}

func TestChartNotesWarnAboutABudgetOfNoEviction(t *testing.T) {
	helm := requireHelm(t)
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes.yaml"), []byte("kind: Notes\nnotes: {{ include \"test.notes\" . | toJson }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for value, warned := range map[string]bool{"0": true, "0%": true, "1": false} {
		out, ok := helmTemplate(t, helm, dir, "--set", "podDisruptionBudget.enabled=true", "--set-string", "podDisruptionBudget.maxUnavailable="+value, "--show-only", "templates/notes.yaml")
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		if strings.Contains(out, "allows no voluntary eviction") != warned {
			t.Errorf("maxUnavailable %s: warned %v:\n%s", value, !warned, out)
		}
	}
}

// renderedDocuments renders the chart with args and parses every YAML
// document it prints, failing the test when rendering or parsing fails.
func renderedDocuments(t *testing.T, helm string, args ...string) []*yaml.Node {
	t.Helper()
	out, ok := helmTemplate(t, helm, chartDir, args...)
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	var docs []*yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(out))
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("the rendered chart does not parse: %v\n%s", err, out)
		}
		if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
			docs = append(docs, doc.Content[0])
		}
	}
}

// child returns the value of key in the mapping node, or nil.
func child(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// path follows keys, and list indexes written as numbers, from node.
func path(node *yaml.Node, keys ...string) *yaml.Node {
	for _, key := range keys {
		if node != nil && node.Kind == yaml.SequenceNode {
			index, err := strconv.Atoi(key)
			if err != nil || index >= len(node.Content) {
				return nil
			}
			node = node.Content[index]
			continue
		}
		node = child(node, key)
	}
	return node
}

// colonKeys lists the mapping keys under node holding a colon: what a
// template renders when a trimmed newline runs two keys together, as
// "selector:matchLabels:" parses as one key of that name.
func colonKeys(node *yaml.Node) []string {
	var found []string
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if strings.Contains(node.Content[i].Value, ":") {
				found = append(found, node.Content[i].Value)
			}
		}
	}
	for _, c := range node.Content {
		found = append(found, colonKeys(c)...)
	}
	return found
}

// Every monitor the chart renders is checked as the Prometheus Operator would
// read it, parsed rather than searched for text: a spec.selector that is a
// non-empty mapping, and no key that is two keys run together. A text search
// for "selector:" passes on "selector:matchLabels:", which parses as neither.
func TestChartMonitorsParseWithASelector(t *testing.T) {
	helm := requireHelm(t)
	targetsFile := staticTargetsValues(t, "")
	for name, tc := range map[string]struct {
		args []string
		// The monitors that must be rendered, by name, with their kind.
		want map[string]string
		// The labels a probe monitor's selector must match, by monitor name.
		selects map[string]map[string]string
	}{
		"service and pod monitors without a target selector": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"pods","enabled":true,"type":"pod","collector":"example"}]`},
			want: map[string]string{"test-prometheus-universal-exporter-apps": "ServiceMonitor", "test-prometheus-universal-exporter-pods": "PodMonitor", "test-prometheus-universal-exporter-self": "ServiceMonitor"},
			selects: map[string]map[string]string{
				"test-prometheus-universal-exporter-apps": {"app.kubernetes.io/name": "target"},
				"test-prometheus-universal-exporter-pods": {"app.kubernetes.io/name": "target"},
			},
		},
		"service and pod monitors with a target selector": {
			args: []string{"--set-json", `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example","targetSelector":{"matchLabels":{"team":"a"}}},{"name":"pods","enabled":true,"type":"pod","collector":"example","targetSelector":{"matchLabels":{"team":"b"}}}]`, "--set", "selfMetrics.type=pod"},
			want: map[string]string{"test-prometheus-universal-exporter-apps": "ServiceMonitor", "test-prometheus-universal-exporter-pods": "PodMonitor", "test-prometheus-universal-exporter-self": "PodMonitor"},
			selects: map[string]map[string]string{
				"test-prometheus-universal-exporter-apps": {"team": "a"},
				"test-prometheus-universal-exporter-pods": {"team": "b"},
			},
		},
		"the static targets and self monitors as services": {
			args: []string{"-f", targetsFile, "--set", "staticTargets.monitor.enabled=true"},
			want: map[string]string{"test-prometheus-universal-exporter-static-targets": "ServiceMonitor", "test-prometheus-universal-exporter-self": "ServiceMonitor"},
		},
		"the static targets and self monitors as pods": {
			args: []string{"-f", targetsFile, "--set", "staticTargets.monitor.enabled=true", "--set", "staticTargets.monitor.type=pod", "--set", "selfMetrics.type=pod"},
			want: map[string]string{"test-prometheus-universal-exporter-static-targets": "PodMonitor", "test-prometheus-universal-exporter-self": "PodMonitor"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := map[string]string{}
			for _, doc := range renderedDocuments(t, helm, tc.args...) {
				kind := child(doc, "kind").Value
				if keys := colonKeys(doc); len(keys) > 0 {
					t.Errorf("%s has keys run together: %q", kind, keys)
				}
				if kind != "ServiceMonitor" && kind != "PodMonitor" {
					continue
				}
				name := path(doc, "metadata", "name").Value
				got[name] = kind
				selector := path(doc, "spec", "selector")
				if selector == nil || selector.Kind != yaml.MappingNode || len(selector.Content) == 0 {
					t.Errorf("%s %s has no spec.selector", kind, name)
					continue
				}
				labels := child(selector, "matchLabels")
				if labels == nil || labels.Kind != yaml.MappingNode || len(labels.Content) == 0 {
					t.Errorf("%s %s has no spec.selector.matchLabels", kind, name)
				}
				for key, value := range tc.selects[name] {
					if v := child(labels, key); v == nil || v.Value != value {
						t.Errorf("%s %s does not select %s=%s", kind, name, key, value)
					}
				}
			}
			for name, kind := range tc.want {
				if got[name] != kind {
					t.Errorf("want %s %s, got %q", kind, name, got[name])
				}
			}
		})
	}
}

// With webAuth.enabled, the exporter's Basic Auth protects /probe as well as
// its own metrics, so a probe monitor without auth of its own presents the
// webAuth Secret, as the self-metrics and static targets monitors do;
// without, every scrape it makes is a 401. A monitor with its own auth keeps
// it.
func TestChartProbeMonitorsPresentTheExporterCredential(t *testing.T) {
	helm := requireHelm(t)
	monitors := `monitors=[{"name":"apps","enabled":true,"type":"service","collector":"example"},{"name":"pods","enabled":true,"type":"pod","collector":"example"},{"name":"own","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"bearer","secretName":"own-token"}}]`
	endpoint := func(doc *yaml.Node) *yaml.Node {
		if child(doc, "kind").Value == "PodMonitor" {
			return path(doc, "spec", "podMetricsEndpoints", "0")
		}
		return path(doc, "spec", "endpoints", "0")
	}
	probes := func(docs []*yaml.Node) map[string]*yaml.Node {
		found := map[string]*yaml.Node{}
		for _, doc := range docs {
			if kind := child(doc, "kind").Value; kind == "ServiceMonitor" || kind == "PodMonitor" {
				if name := path(doc, "metadata", "name").Value; strings.HasSuffix(name, "-apps") || strings.HasSuffix(name, "-pods") || strings.HasSuffix(name, "-own") {
					found[name] = endpoint(doc)
				}
			}
		}
		if len(found) != 3 {
			t.Fatalf("want 3 probe monitors, got %d", len(found))
		}
		return found
	}

	for name, ep := range probes(renderedDocuments(t, helm, "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth", "--set", "webAuth.usernameKey=user", "--set-json", monitors)) {
		if strings.HasSuffix(name, "-own") {
			if child(ep, "basicAuth") != nil || path(ep, "authorization", "credentials", "name").Value != "own-token" {
				t.Errorf("%s does not keep its own bearer credential", name)
			}
			continue
		}
		for _, want := range [][]string{{"username", "name", "exporter-auth"}, {"username", "key", "user"}, {"password", "name", "exporter-auth"}, {"password", "key", "password"}} {
			if v := path(ep, "basicAuth", want[0], want[1]); v == nil || v.Value != want[2] {
				t.Errorf("%s: basicAuth.%s.%s is not %q", name, want[0], want[1], want[2])
			}
		}
	}
	for name, ep := range probes(renderedDocuments(t, helm, "--set-json", monitors)) {
		if child(ep, "basicAuth") != nil {
			t.Errorf("%s presents a credential without webAuth", name)
		}
	}
}

// chartWithNotesAsAManifest copies the chart and has the copy render
// NOTES.txt as a manifest, templates/notes.yaml, since helm template does not
// render the notes. It returns the copy's directory.
func chartWithNotesAsAManifest(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.CopyFS(dir, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes.yaml"), []byte("kind: Notes\nnotes: {{ include \"test.notes\" . | toJson }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A budget whose minAvailable is every replica allows no voluntary eviction
// either, as maxUnavailable: 0 does: a node drain waits for ever. The notes
// warn when minAvailable, a count or a percentage rounded up as Kubernetes
// rounds it, reaches replicaCount. With autoscaling the replica count is not
// the chart's to know, so only 100%, which blocks at any count, is warned
// about.
func TestChartNotesWarnAboutABudgetOfEveryReplica(t *testing.T) {
	helm := requireHelm(t)
	dir := chartWithNotesAsAManifest(t)
	for name, tc := range map[string]struct {
		args   []string
		warned bool
	}{
		"one of one replica":           {[]string{"--set", "podDisruptionBudget.minAvailable=1"}, true},
		"one of two replicas":          {[]string{"--set", "podDisruptionBudget.minAvailable=1", "--set", "replicaCount=2"}, false},
		"two of two replicas":          {[]string{"--set", "podDisruptionBudget.minAvailable=2", "--set", "replicaCount=2"}, true},
		"three of two replicas":        {[]string{"--set", "podDisruptionBudget.minAvailable=3", "--set", "replicaCount=2"}, true},
		"none of one replica":          {[]string{"--set", "podDisruptionBudget.minAvailable=0"}, false},
		"half of one replica":          {[]string{"--set-string", "podDisruptionBudget.minAvailable=50%"}, true},
		"half of two replicas":         {[]string{"--set-string", "podDisruptionBudget.minAvailable=50%", "--set", "replicaCount=2"}, false},
		"67% of three replicas":        {[]string{"--set-string", "podDisruptionBudget.minAvailable=67%", "--set", "replicaCount=3"}, true},
		"66% of three replicas":        {[]string{"--set-string", "podDisruptionBudget.minAvailable=66%", "--set", "replicaCount=3"}, false},
		"all of three replicas":        {[]string{"--set-string", "podDisruptionBudget.minAvailable=100%", "--set", "replicaCount=3"}, true},
		"one with autoscaling":         {[]string{"--set", "podDisruptionBudget.minAvailable=1", "--set", "autoscaling.enabled=true"}, false},
		"all with autoscaling":         {[]string{"--set-string", "podDisruptionBudget.minAvailable=100%", "--set", "autoscaling.enabled=true"}, true},
		"one of no replicas":           {[]string{"--set", "podDisruptionBudget.minAvailable=1", "--set", "replicaCount=0"}, false},
		"the default budget":           {nil, false},
		"one unavailable of one":       {[]string{"--set", "podDisruptionBudget.maxUnavailable=1"}, false},
		"a budget that is not enabled": {[]string{"--set", "podDisruptionBudget.enabled=false", "--set", "podDisruptionBudget.minAvailable=1"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--set", "podDisruptionBudget.enabled=true", "--show-only", "templates/notes.yaml"}, tc.args...)
			out, ok := helmTemplate(t, helm, dir, args...)
			if !ok {
				t.Fatalf("rendering failed:\n%s", out)
			}
			if warned := strings.Contains(out, "WARNING: podDisruptionBudget.minAvailable is") && strings.Contains(out, "leaves no pod that may be evicted"); warned != tc.warned {
				t.Errorf("warned %v, want %v:\n%s", warned, tc.warned, out)
			}
		})
	}
}

// probeMonitors renders the chart with args and returns each probe monitor,
// by the name its monitors entry gives it, with its one endpoint.
func probeMonitors(t *testing.T, helm string, args ...string) (monitors, endpoints map[string]*yaml.Node) {
	t.Helper()
	monitors, endpoints = map[string]*yaml.Node{}, map[string]*yaml.Node{}
	for _, doc := range renderedDocuments(t, helm, args...) {
		kind := child(doc, "kind").Value
		if kind != "ServiceMonitor" && kind != "PodMonitor" {
			continue
		}
		name := strings.TrimPrefix(path(doc, "metadata", "name").Value, "test-prometheus-universal-exporter-")
		if name == "self" || name == "static-targets" {
			continue
		}
		monitors[name] = doc
		list := "endpoints"
		if kind == "PodMonitor" {
			list = "podMetricsEndpoints"
		}
		if all := path(doc, "spec", list); all == nil || len(all.Content) != 1 {
			t.Fatalf("%s %s does not have one endpoint", kind, name)
		}
		endpoints[name] = path(doc, "spec", list, "0")
	}
	return monitors, endpoints
}

// A probe monitor reaches the targets its entry describes: on the port the
// entry names, where it rendered http whatever the targets called theirs, so
// a gRPC server's grpc port could not be probed; and in the namespaces its
// namespaceSelector gives, where it rendered none, so only targets in the
// monitor's own namespace were found. Without either, the port is http and
// no namespaceSelector is rendered, as before.
func TestChartProbeMonitorsTakeAPortAndNamespaces(t *testing.T) {
	helm := requireHelm(t)
	monitors, endpoints := probeMonitors(t, helm, "--set-json", `monitors=[
		{"name":"plain","enabled":true,"type":"service","collector":"example"},
		{"name":"plain-pods","enabled":true,"type":"pod","collector":"example"},
		{"name":"grpc","enabled":true,"type":"service","collector":"example","port":"grpc","namespaceSelector":{"matchNames":["payments","search"]}},
		{"name":"everywhere","enabled":true,"type":"pod","collector":"example","port":"metrics","namespaceSelector":{"any":true}},
		{"name":"odd-port","enabled":true,"type":"service","collector":"example","port":"on"}]`)
	if len(monitors) != 5 {
		t.Fatalf("%d probe monitors rendered, want 5", len(monitors))
	}
	for name, want := range map[string]string{"plain": "http", "plain-pods": "http", "grpc": "grpc", "everywhere": "metrics", "odd-port": "on"} {
		// The tag too: an unquoted on would be a boolean, not a port's name.
		if port := child(endpoints[name], "port"); port == nil || port.Value != want || port.Tag != "!!str" {
			t.Errorf("%s: the endpoint's port is %+v, want the string %q", name, port, want)
		}
		if got := child(endpoints[name], "path"); got == nil || got.Value != "/probe" {
			t.Errorf("%s: the endpoint does not scrape /probe", name)
		}
	}
	for _, name := range []string{"plain", "plain-pods", "odd-port"} {
		if selector := path(monitors[name], "spec", "namespaceSelector"); selector != nil {
			t.Errorf("%s: a namespaceSelector is rendered without one in the values", name)
		}
	}
	names := path(monitors["grpc"], "spec", "namespaceSelector", "matchNames")
	if names == nil || len(names.Content) != 2 || names.Content[0].Value != "payments" || names.Content[1].Value != "search" {
		t.Errorf("grpc: spec.namespaceSelector.matchNames is not [payments search]")
	}
	if selector := path(monitors["grpc"], "spec", "selector", "matchLabels"); selector == nil {
		t.Errorf("grpc: the namespaceSelector displaced spec.selector")
	}
	if all := path(monitors["everywhere"], "spec", "namespaceSelector", "any"); all == nil || all.Value != "true" || all.Tag != "!!bool" {
		t.Errorf("everywhere: spec.namespaceSelector.any is not true")
	}
}

// A probe monitor without interval or scrapeTimeout rendered interval: null
// and scrapeTimeout: null. Each is now left out when unset, so the
// Prometheus Operator's defaults apply, and rendered as before when set; the
// check of the timeout against the interval keeps its verdicts, comparing
// the two only when both are set.
func TestChartProbeMonitorsLeaveOutTimingsTheyAreNotGiven(t *testing.T) {
	helm := requireHelm(t)
	monitors, endpoints := probeMonitors(t, helm, "--set-json", `monitors=[
		{"name":"none","enabled":true,"type":"service","collector":"example"},
		{"name":"none-pods","enabled":true,"type":"pod","collector":"example"},
		{"name":"interval","enabled":true,"type":"service","collector":"example","interval":"30s"},
		{"name":"timeout","enabled":true,"type":"pod","collector":"example","scrapeTimeout":"5m"},
		{"name":"both","enabled":true,"type":"pod","collector":"example","interval":"1m","scrapeTimeout":"10s"}]`)
	if len(monitors) != 5 {
		t.Fatalf("%d probe monitors rendered, want 5", len(monitors))
	}
	for name, want := range map[string][2]string{"none": {}, "none-pods": {}, "interval": {"30s", ""}, "timeout": {"", "5m"}, "both": {"1m", "10s"}} {
		for i, key := range []string{"interval", "scrapeTimeout"} {
			got := child(endpoints[name], key)
			switch {
			case want[i] == "" && got != nil:
				t.Errorf("%s: %s is rendered as %q (%s) without one in the values", name, key, got.Value, got.Tag)
			case want[i] != "" && (got == nil || got.Value != want[i] || got.Tag != "!!str"):
				t.Errorf("%s: %s is %+v, want %q", name, key, got, want[i])
			}
		}
		if got := child(endpoints[name], "path"); got == nil || got.Value != "/probe" {
			t.Errorf("%s: the endpoint does not scrape /probe", name)
		}
	}
	out, ok := helmTemplate(t, helm, chartDir, "--set-json", `monitors=[{"name":"none","enabled":true,"type":"service","collector":"example"}]`)
	if !ok || strings.Contains(out, "null") {
		t.Errorf("ok=%v, want a monitor without timings rendered with no null:\n%s", ok, out)
	}
	if !strings.Contains(out, "\n      path: /probe\n      params:\n") {
		t.Errorf("the endpoint's params do not follow its path once the timings are left out:\n%s", out)
	}
}

// A monitor's port is a port's name, as the Prometheus Operator's port field
// is, and the chart rendered whatever string it was given: port: "9115"
// became a name no port has, so the monitor found no target and nothing said
// why. Now the values schema holds it to the grammar of the names it can
// match — a Service port's for type service, a DNS label of up to 63
// characters, and a container port's for type pod, up to 15 with no two
// hyphens in a row — with at least one letter in either, so a number is
// refused whether it is written as one or as a string.
func TestChartProbeMonitorsTakeAPortNameAndRefuseANumber(t *testing.T) {
	helm := requireHelm(t)
	monitor := func(kind, port string) string {
		return `monitors=[{"name":"apps","enabled":true,"type":"` + kind + `","collector":"example","port":` + port + `}]`
	}
	sixteen, sixtyFour := strings.Repeat("a", 16), strings.Repeat("a", 64)
	for _, kind := range []string{"service", "pod"} {
		accepted := []string{"http", "grpc", "metrics-2", "a", "9-a", strings.Repeat("a", 15)}
		refused := []string{"9115", `"9115"`, `"Http"`, `"-http"`, `"http-"`, `""`, `"1-2"`, `"my_port"`, `"` + sixtyFour + `"`, "true"}
		if kind == "pod" {
			refused = append(refused, `"a--b"`, `"`+sixteen+`"`)
		} else {
			// A Service port's name is a DNS label, which may be longer than
			// a container port's and hold two hyphens in a row.
			accepted = append(accepted, "a--b", sixteen, strings.Repeat("a", 63))
		}
		for _, port := range accepted {
			_, endpoints := probeMonitors(t, helm, "--set-json", monitor(kind, `"`+port+`"`))
			if got := child(endpoints["apps"], "port"); got == nil || got.Value != port || got.Tag != "!!str" {
				t.Errorf("type %s, port %q: the endpoint's port is %+v, want that name as a string", kind, port, got)
			}
		}
		for _, port := range refused {
			out, ok := helmTemplate(t, helm, chartDir, "--set-json", monitor(kind, port))
			if ok || !namesInHelmError(out, "monitors.0.port") {
				t.Errorf("type %s, port %s: ok=%v, want the values schema to refuse it naming monitors.0.port:\n%s", kind, port, ok, out)
			}
		}
	}
}

// The templates refuse such a port themselves, naming the monitor and saying
// what to give instead, as well as the values schema does: with the schema
// skipped a number was rendered as a name, quoted, and anything else as it
// was written. A number is told to be one, since the fix is to name the port
// on the Service or the pod; a null port is none, and the default.
func TestChartMonitorTemplatesRefuseAPortThatIsNoName(t *testing.T) {
	helm := requireHelm(t)
	if out, _ := helmTemplate(t, helm, chartDir, "--skip-schema-validation"); strings.Contains(out, "unknown flag") {
		t.Skip("this helm cannot skip the values schema")
	}
	const (
		serviceNumber = `monitors entry "apps" has port 9115, a port number; a monitor of type service takes the name of a Service port, so name the port in the selected Services' spec.ports and set port to that name`
		podNumber     = `monitors entry "apps" has port 9115, a port number; a monitor of type pod takes the name of a container port, so name the port in the selected pods' spec.containers[].ports and set port to that name`
		serviceName   = `; a monitor of type service takes the name of a Service port: 1 to 63 lower-case letters, digits and hyphens, with no hyphen first or last, and, by this chart's own rule, at least one letter, since it takes digits alone for a port number`
		podName       = `; a monitor of type pod takes the name of a container port: 1 to 15 lower-case letters, digits and hyphens, at least one of them a letter, with no hyphen first or last and no two in a row`
	)
	sixteen := strings.Repeat("a", 16)
	for _, tc := range []struct {
		kind, port, want string
	}{
		{"service", "9115", serviceNumber},
		{"service", `"9115"`, serviceNumber},
		{"pod", "9115", podNumber},
		{"pod", `"9115"`, podNumber},
		{"service", `"Http"`, `monitors entry "apps" has port "Http"` + serviceName},
		{"service", `"-http"`, `has port "-http"` + serviceName},
		{"service", `"http-"`, `has port "http-"` + serviceName},
		{"service", `""`, `has port ""` + serviceName},
		{"service", `"1-2"`, `has port "1-2"` + serviceName},
		{"service", `"` + strings.Repeat("a", 64) + `"`, serviceName},
		{"service", "true", `has port "true"` + serviceName},
		{"pod", `"Http"`, `monitors entry "apps" has port "Http"` + podName},
		{"pod", `"-http"`, `has port "-http"` + podName},
		{"pod", `"http-"`, `has port "http-"` + podName},
		{"pod", `"a--b"`, `has port "a--b"` + podName},
		{"pod", `"` + sixteen + `"`, `has port "` + sixteen + `"` + podName},
		{"pod", `""`, `has port ""` + podName},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"`+tc.kind+`","collector":"example","port":`+tc.port+`}]`)
		if ok || !strings.Contains(out, tc.want) {
			t.Errorf("type %s, port %s: ok=%v, want an error saying %q:\n%s", tc.kind, tc.port, ok, tc.want, out)
		}
	}
	for _, kind := range []string{"service", "pod"} {
		for port, want := range map[string]string{`"grpc"`: "grpc", `"metrics-2"`: "metrics-2", `"a"`: "a", "null": "http"} {
			_, endpoints := probeMonitors(t, helm, "--skip-schema-validation", "--set-json", `monitors=[{"name":"apps","enabled":true,"type":"`+kind+`","collector":"example","port":`+port+`}]`)
			if got := child(endpoints["apps"], "port"); got == nil || got.Value != want || got.Tag != "!!str" {
				t.Errorf("type %s, port %s, with the schema skipped: the endpoint's port is %+v, want %q", kind, port, got, want)
			}
		}
	}
	// An entry that is not enabled renders no monitor, so its port is not the
	// templates' to refuse.
	if out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set-json", `monitors=[{"name":"apps","enabled":false,"type":"service","collector":"example","port":9115}]`); !ok {
		t.Errorf("a monitor that is not enabled fails rendering over its port:\n%s", out)
	}
}

// A monitor's own credential renders complete Secret selectors from its type
// and Secret alone, the keys defaulting to token, username and password, for
// monitors of both types.
func TestChartProbeMonitorsRenderTheirOwnCredential(t *testing.T) {
	helm := requireHelm(t)
	_, endpoints := probeMonitors(t, helm, "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth", "--set-json", `monitors=[
		{"name":"bearer","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"bearer","secretName":"scrape-token"}},
		{"name":"bearer-pods","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"type":"bearer","secretName":"scrape-token","secretKey":"jwt","optional":true}},
		{"name":"basic","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"basic","secretName":"scrape-user","usernameKey":"user"}},
		{"name":"basic-pods","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"type":"basic","secretName":"scrape-user"}}]`)
	for name, want := range map[string]map[string]string{
		"bearer":      {"authorization.type": "Bearer", "authorization.credentials.name": "scrape-token", "authorization.credentials.key": "token", "authorization.credentials.optional": "false"},
		"bearer-pods": {"authorization.type": "Bearer", "authorization.credentials.name": "scrape-token", "authorization.credentials.key": "jwt", "authorization.credentials.optional": "true"},
		"basic":       {"basicAuth.username.name": "scrape-user", "basicAuth.username.key": "user", "basicAuth.password.name": "scrape-user", "basicAuth.password.key": "password", "basicAuth.password.optional": "false"},
		"basic-pods":  {"basicAuth.username.name": "scrape-user", "basicAuth.username.key": "username", "basicAuth.password.name": "scrape-user", "basicAuth.password.key": "password"},
	} {
		endpoint := endpoints[name]
		if endpoint == nil {
			t.Errorf("%s is not rendered", name)
			continue
		}
		for field, value := range want {
			if got := path(endpoint, strings.Split(field, ".")...); got == nil || got.Value != value {
				t.Errorf("%s: %s is %+v, want %q", name, field, got, value)
			}
		}
		// One credential, the entry's own: not the exporter's beside it.
		if other := map[bool]string{true: "basicAuth", false: "authorization"}[strings.HasPrefix(name, "bearer")]; child(endpoint, other) != nil {
			t.Errorf("%s also renders %s", name, other)
		}
		if relabelings := child(endpoint, "relabelings"); relabelings == nil || len(relabelings.Content) != 3 {
			t.Errorf("%s: the credential displaced the endpoint's relabelings", name)
		}
	}
}

// The templates refuse an enabled auth without a type or a Secret themselves,
// naming the monitor, as well as the values schema does: with the schema
// skipped, such an entry rendered no credential at all, or the exporter's
// own in place of the entry's, or a selector naming no Secret.
func TestChartMonitorTemplatesRefuseAnIncompleteCredential(t *testing.T) {
	helm := requireHelm(t)
	if out, _ := helmTemplate(t, helm, chartDir, "--skip-schema-validation"); strings.Contains(out, "unknown flag") {
		t.Skip("this helm cannot skip the values schema")
	}
	for name, tc := range map[string]struct {
		monitor, want string
	}{
		"a service monitor without a type":   {`{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"secretName":"token"}}`, `monitors entry "apps" enables auth without a type; set its auth.type to bearer or basic`},
		"a pod monitor without a type":       {`{"name":"pods","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"secretName":"token"}}`, `monitors entry "pods" enables auth without a type`},
		"a service monitor of an odd type":   {`{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"digest","secretName":"token"}}`, `monitors entry "apps" enables auth without a type`},
		"a service monitor without a Secret": {`{"name":"apps","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"type":"bearer"}}`, `monitors entry "apps" enables auth without a Secret; set its auth.secretName to the Secret holding the bearer credential`},
		"a pod monitor of an empty Secret":   {`{"name":"pods","enabled":true,"type":"pod","collector":"example","auth":{"enabled":true,"type":"basic","secretName":""}}`, `monitors entry "pods" enables auth without a Secret; set its auth.secretName to the Secret holding the basic credential`},
	} {
		t.Run(name, func(t *testing.T) {
			// With webAuth too: the exporter's credential must not stand in
			// for the one the entry asked for.
			out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "--set", "webAuth.enabled=true", "--set", "webAuth.secretName=exporter-auth", "--set-json", "monitors=["+tc.monitor+"]")
			if ok || !strings.Contains(out, tc.want) {
				t.Fatalf("ok=%v, want an error saying %q:\n%s", ok, tc.want, out)
			}
		})
	}
}

// The static targets monitor's scrape timeout may not be longer than its
// interval either, while the monitor is rendered.
func TestChartRefusesAStaticTargetsMonitorTimeoutOverItsInterval(t *testing.T) {
	helm := requireHelm(t)
	targets := staticTargetsValues(t, "")
	timings := []string{"--set", "staticTargets.monitor.interval=10s", "--set", "staticTargets.monitor.scrapeTimeout=11s"}
	out, ok := helmTemplate(t, helm, chartDir, append([]string{"-f", targets, "--set", "staticTargets.monitor.enabled=true"}, timings...)...)
	if want := "staticTargets.monitor has scrapeTimeout 11s, longer than its interval 10s"; ok || !strings.Contains(out, want) {
		t.Errorf("ok=%v, want an error saying %q:\n%s", ok, want, out)
	}
	if out, ok := helmTemplate(t, helm, chartDir, append([]string{"-f", targets}, timings...)...); !ok {
		t.Errorf("the timings of a static targets monitor that is off fail rendering:\n%s", out)
	}
	if out, ok := helmTemplate(t, helm, chartDir, "-f", targets, "--set", "staticTargets.monitor.enabled=true", "--set", "staticTargets.monitor.interval=10s", "--set", "staticTargets.monitor.scrapeTimeout=10s"); !ok {
		t.Errorf("a scrape timeout equal to the interval fails rendering:\n%s", out)
	}
}
