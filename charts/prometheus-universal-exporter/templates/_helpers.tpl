{{- define "prometheus-universal-exporter.extraArgs" -}}
{{- /* The chart already renders the flags below from named values. Go's flag
       package keeps the last occurrence, so an extraArgs entry repeating one of
       them would win silently — and for --web.listen-address the container port
       and the probes would still follow server.listenAddress, leaving a pod
       that listens on one port while Kubernetes checks another. Rejecting the
       collision while rendering costs nothing; debugging it costs an
       afternoon. */ -}}
{{- $managed := dict
  "--config.file" "the chart renders the configuration itself, so change it through config.data, or set config.enabled to false and supply the ConfigMap yourself"
  "--web.listen-address" "set server.listenAddress instead"
  "--web.self-metrics-path" "set selfMetrics.path instead"
  "--python.path" "set server.pythonPath instead"
  "--config.watch" "set server.watchConfig instead"
  "--config.watch-interval" "set server.watchConfigInterval instead"
  "--static-targets-file" "set staticTargets.enabled and staticTargets.data instead"
  "--web.static-targets-path" "set staticTargets.path instead"
  "--config.expand-env" "set server.expandEnv instead"
  "--static-targets.expand-env" "set staticTargets.expandEnv instead"
  "--log.level" "set server.logLevel instead"
  "--probe.timeout-offset" "set server.probeTimeoutOffset instead"
  "--probe.default-timeout" "set server.probeDefaultTimeout instead"
  "--probe.max-concurrent" "set server.probeMaxConcurrent instead"
  "--python.max-workers" "set server.pythonMaxWorkers instead"
  "--runtime.memory-limit-ratio" "set goMemLimit.ratio instead"
  "--web.enable-lifecycle" "set server.enableLifecycle instead"
  "--web.enable-probe-debug" "set server.probeDebug instead"
  "--web.shutdown-timeout" "set server.shutdownTimeout instead"
  "--web.shutdown-delay" "set server.shutdownDelay instead" -}}
{{- /* These flags make the exporter print something and exit instead of
       serving, so a pod started with one would restart for ever. */ -}}
{{- $oneShot := dict
  "--dry-run" "would make the exporter validate its configuration and exit, so the pod would never serve; run --dry-run as a separate command, a Job or an init container instead"
  "--config.schema" "would make the exporter print the configuration schema and exit, so the pod would never serve; run it as a separate command instead"
  "--config.collector-file-schema" "would make the exporter print the collector file schema and exit, so the pod would never serve; run it as a separate command instead"
  "--static-targets-file-schema" "would make the exporter print the static target file schema and exit, so the pod would never serve; run it as a separate command instead"
  "--version" "would make the exporter print its version and exit, so the pod would never serve; the version is in the http_exporter_build_info self-metric"
  "--help" "would make the exporter print its usage and exit, so the pod would never serve"
  "--h" "would make the exporter print its usage and exit, so the pod would never serve" -}}
{{- range $arg := .Values.extraArgs -}}
{{- $text := $arg | toString -}}
{{- if not (hasPrefix "--" $text) -}}
{{- fail (printf "extraArgs entry %q must start with `--`, for example \"--some.new-flag=value\"" $text) -}}
{{- end -}}
{{- $name := $text | splitList "=" | first -}}
{{- if hasKey $oneShot $name -}}
{{- fail (printf "extraArgs entry %q %s" $text (get $oneShot $name)) -}}
{{- end -}}
{{- if hasKey $managed $name -}}
{{- fail (printf "extraArgs entry %q sets %s, which the chart already manages; %s" $text $name (get $managed $name)) -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateMounts" -}}
{{- /* A pod cannot mount two volumes at one path, and Kubernetes says so only
       once the Deployment is applied; a mount at the configuration directory
       would, where it is accepted, replace it, so the exporter starts with no
       config.yaml and crash-loops with an error that points at the file
       rather than at the mount that hid it. So every path the chart mounts,
       and every extraVolumeMounts entry, has to be a path of its own. Paths
       are compared cleaned, so a trailing slash does not hide a collision. */ -}}
{{- $taken := dict "/etc/prometheus-universal-exporter" "the configuration directory" -}}
{{- range $name := list "targetAuth" "webAuth" -}}
{{- $auth := index $.Values $name -}}
{{- if and $auth $auth.enabled -}}
{{- $path := $auth.mountPath | toString | clean -}}
{{- if hasKey $taken $path -}}
{{- fail (printf "%s.mountPath %q is also %s, and a pod cannot mount two volumes at one path; choose another path" $name (toString $auth.mountPath) (get $taken $path)) -}}
{{- end -}}
{{- $_ := set $taken $path (printf "%s.mountPath" $name) -}}
{{- end -}}
{{- end -}}
{{- $extra := dict -}}
{{- range $mount := .Values.extraVolumeMounts -}}
{{- $path := $mount.mountPath | toString | clean -}}
{{- if hasKey $taken $path -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q, which the chart already mounts as %s; choose another path" (toString $mount.mountPath) (get $taken $path)) -}}
{{- end -}}
{{- if hasKey $extra $path -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q twice, and a pod cannot mount two volumes at one path; give each entry a path of its own" (toString $mount.mountPath)) -}}
{{- end -}}
{{- $_ := set $extra $path true -}}
{{- end -}}
{{- /* The configuration directory and the credential directories are
       ConfigMap and Secret volumes, which the kubelet mounts read-only. A
       mount below one of them, a subPath file mount among them, needs its
       mount point created inside that volume, which fails, so the container
       is never started (CreateContainerError), with nothing said where the
       values were written; and so does the configuration directory below
       an enabled credential directory. The runtime mounts a parent before
       what is below it, whichever is written first. */ -}}
{{- $readOnly := list (dict "path" "/etc/prometheus-universal-exporter" "what" "the configuration directory /etc/prometheus-universal-exporter" "kind" "ConfigMap" "hint" "; to add a file to it, add the file to config.data instead") -}}
{{- range $name := list "targetAuth" "webAuth" -}}
{{- $auth := index $.Values $name -}}
{{- if and $auth $auth.enabled -}}
{{- $readOnly = append $readOnly (dict "path" ($auth.mountPath | toString | clean) "what" (printf "%s.mountPath %q" $name (toString $auth.mountPath)) "kind" "Secret" "hint" "" "name" $name) -}}
{{- end -}}
{{- end -}}
{{- range $inner := $readOnly -}}
{{- range $outer := $readOnly -}}
{{- if and (ne $inner.path $outer.path) (hasPrefix (printf "%s/" (trimSuffix "/" $outer.path)) $inner.path) -}}
{{- if $inner.name -}}
{{- fail (printf "%s is inside %s, a read-only %s volume in which the mount point cannot be created, so the container would never start; choose a path outside %s" $inner.what $outer.what $outer.kind $outer.path) -}}
{{- end -}}
{{- fail (printf "%s holds the configuration directory /etc/prometheus-universal-exporter, which cannot be mounted inside that read-only Secret volume, so the container would never start; choose a path that is not above /etc/prometheus-universal-exporter" $outer.what) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range $mount := .Values.extraVolumeMounts -}}
{{- $path := $mount.mountPath | toString | clean -}}
{{- range $outer := $readOnly -}}
{{- if and (ne $path $outer.path) (hasPrefix (printf "%s/" (trimSuffix "/" $outer.path)) $path) -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q, which is inside %s, a read-only %s volume in which the mount point cannot be created, so the container would never start; mount it outside %s%s" (toString $mount.mountPath) $outer.what $outer.kind $outer.path $outer.hint) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- /* The release's objects are named after the release, so two releases in
       one namespace do not claim the same names: <release>-<chart>, or the
       release name alone when it already holds the chart's name, as helm
       create's chart does. fullnameOverride names them outright. */ -}}
{{- define "prometheus-universal-exporter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := include "prometheus-universal-exporter.name" . }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}
{{- define "prometheus-universal-exporter.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end }}
{{- /* A value as text: a number written out in full, anything else as it
       prints. Helm reads the numbers of a values file, and of --set-json, as
       floating point, and a template prints one of a million or more with an
       exponent: 2000000 as 2e+06, which is not what was written, and is no
       whole number to a flag or to a check that looks for digits. toJson
       writes the number out, 2000000, whether it was written 2000000, 2e6 or
       2000000.0, and a fraction as the fraction it is. */ -}}
{{- define "prometheus-universal-exporter.text" -}}
{{- if kindIs "float64" . }}{{ toJson . }}{{ else }}{{ toString . }}{{ end -}}
{{- end }}
{{- /* A whole number of the values, from (list name value least most),
       written out in full: the value is one by what it is, however it was
       written, and anything else — a fraction, a number outside least to
       most, a text that is no number — fails rendering. The digits are
       counted before they are read, since a number of 1e19 or more is
       beyond what a template can hold as a whole number. A value left out
       or null renders nothing, and is Kubernetes' to default. */ -}}
{{- define "prometheus-universal-exporter.wholeNumber" -}}
{{- if not (kindIs "invalid" (index . 1)) -}}
{{- $text := include "prometheus-universal-exporter.text" (index . 1) -}}
{{- $least := index . 2 | int64 -}}
{{- $most := index . 3 | int64 -}}
{{- if or (not (regexMatch "^-?(0|[1-9][0-9]{0,17})$" $text)) (lt (int64 $text) $least) (gt (int64 $text) $most) -}}
{{- fail (printf "%s %s must be a whole number from %d to %d" (index . 0) $text $least $most) -}}
{{- end -}}
{{- int64 $text -}}
{{- end -}}
{{- end }}
{{- /* A disruption budget's count, from (list name value): a string, which
       is how a percentage is written, as it is, and a number as the whole
       number it is. The string is rendered bare, so a string of digits
       becomes the number, and one with a zero first YAML's octal number,
       010 as eight; one past 2147483647, and a percentage past 100%, which
       Kubernetes refuses on a budget, would fail only when applied. So the
       string is held to the values schema's pattern, which spells the range
       out digit by digit. */ -}}
{{- define "prometheus-universal-exporter.budgetCount" -}}
{{- if kindIs "string" (index . 1) -}}
{{- if not (regexMatch "^(0|[1-9][0-9]{0,8}|1[0-9]{9}|20[0-9]{8}|21[0-3][0-9]{7}|214[0-6][0-9]{6}|2147[0-3][0-9]{5}|21474[0-7][0-9]{4}|214748[0-2][0-9]{3}|2147483[0-5][0-9]{2}|21474836[0-3][0-9]|214748364[0-7]|(0|[1-9][0-9]?|100)%)$" (index . 1)) -}}
{{- fail (printf "%s %q must be a whole number from 0 to 2147483647, or a percentage from 0%% to 100%%, written with no zero before another digit" (index . 0) (index . 1)) -}}
{{- end -}}
{{- index . 1 -}}
{{- else -}}
{{- include "prometheus-universal-exporter.wholeNumber" (list (index . 0) (index . 1) 0 2147483647) -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.servicePort" -}}
{{- include "prometheus-universal-exporter.wholeNumber" (list "service.port" .Values.service.port 1 65535) -}}
{{- end }}
{{- /* The exporter's Service as Prometheus reaches it from any namespace. A
       port set to null prints as it did, <nil>; the Service is then
       Kubernetes' to refuse. */ -}}
{{- define "prometheus-universal-exporter.serviceAddress" -}}
{{- printf "%s.%s.svc:%s" (include "prometheus-universal-exporter.fullname" .) (include "prometheus-universal-exporter.namespace" .) (include "prometheus-universal-exporter.servicePort" . | default "<nil>") -}}
{{- end }}
{{- /* Probe monitors send Prometheus to the exporter's Service, whatever
       their type, so they need it. */ -}}
{{- define "prometheus-universal-exporter.requireServiceForProbes" -}}
{{- range .Values.monitors }}
{{- if and .enabled (not $.Values.service.enabled) }}
{{- fail (printf "monitors entry %q probes through the exporter's Service, which service.enabled=false leaves out; enable the Service" (toString (.name | default "unnamed"))) }}
{{- end }}
{{- end }}
{{- end }}
{{- /* Whether the chart renders the self-metrics monitor: with
       selfMetrics.enabled, when the chart renders another monitor, which
       says the Prometheus Operator's resources exist, or the cluster
       serves that monitor's kind. */ -}}
{{- define "prometheus-universal-exporter.selfMonitor" -}}
{{- if .Values.selfMetrics.enabled }}
{{- $kind := ternary "PodMonitor" "ServiceMonitor" (eq (.Values.selfMetrics.type | default "service") "pod") }}
{{- $other := false }}
{{- range .Values.monitors }}{{- if .enabled }}{{- $other = true }}{{- end }}{{- end }}
{{- if and .Values.staticTargets.enabled .Values.staticTargets.monitor.enabled }}{{- $other = true }}{{- end }}
{{- if or $other (.Capabilities.APIVersions.Has (printf "monitoring.coreos.com/v1/%s" $kind)) }}{{ $kind }}{{- end }}
{{- end }}
{{- end }}
{{- define "prometheus-universal-exporter.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "prometheus-universal-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "prometheus-universal-exporter.metadataLabels" -}}
{{- $root := index . 0 -}}
{{- $labels := dict -}}
{{- range $key, $value := $root.Values.defaultLabels }}{{- $_ := set $labels $key $value -}}{{- end -}}
{{- if gt (len .) 1 }}{{- range $key, $value := index . 1 }}{{- $_ := set $labels $key $value -}}{{- end -}}{{- end -}}
{{- range $key, $value := (include "prometheus-universal-exporter.labels" $root | fromYaml) }}{{- $_ := set $labels $key $value -}}{{- end -}}
{{- toYaml $labels -}}
{{- end }}
{{- define "prometheus-universal-exporter.metadataAnnotations" -}}
{{- $root := index . 0 -}}
{{- $annotations := dict -}}
{{- range $key, $value := $root.Values.defaultAnnotations }}{{- $_ := set $annotations $key $value -}}{{- end -}}
{{- if gt (len .) 1 }}{{- range $key, $value := index . 1 }}{{- $_ := set $annotations $key $value -}}{{- end -}}{{- end -}}
{{- toYaml $annotations -}}
{{- end }}
{{- define "prometheus-universal-exporter.listenAddress" -}}
{{- $address := default ":8080" .Values.server.listenAddress -}}
{{- /* Go's net.Listen wants host:port. A bare port such as "9115" renders
       perfectly well here and then makes the container exit at once with
       "listen tcp: address 9115: missing port in address", so it is rejected
       while the chart is still being rendered. The host may be empty, a name or
       IPv4 address, or a bracketed IPv6 address. */ -}}
{{- if not (regexMatch "^([^:]*|\\[[0-9A-Fa-f:.]+\\]):[0-9]+$" $address) -}}
{{- fail (printf "server.listenAddress %q must be host:port with the port after a colon, for example \":8080\", \"0.0.0.0:8080\" or \"[::]:8080\"" $address) -}}
{{- end -}}
{{- /* A loopback host renders and starts, and then nothing outside the pod
       reaches the exporter: the kubelet's probes fail, the pod restarts for
       ever, and the Service has no endpoint to send Prometheus to. */ -}}
{{- $host := $address | splitList ":" | initial | join ":" | lower -}}
{{- if regexMatch "^(127\\.[0-9]+\\.[0-9]+\\.[0-9]+|localhost|\\[::1\\])$" $host -}}
{{- fail (printf "server.listenAddress %q listens on a loopback address, which neither the kubelet's probes nor the Service reach; leave the host empty, as in \":8080\", or use \"0.0.0.0:8080\" or \"[::]:8080\"" $address) -}}
{{- end -}}
{{- $port := $address | splitList ":" | last | int -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "server.listenAddress %q must end in a TCP port between 1 and 65535" $address) -}}
{{- end -}}
{{- $address -}}
{{- end }}
{{- define "prometheus-universal-exporter.containerPort" -}}
{{- /* listenAddress has already validated the shape and the range. */ -}}
{{- include "prometheus-universal-exporter.listenAddress" . | splitList ":" | last | int -}}
{{- end }}
{{- define "prometheus-universal-exporter.watchConfigInterval" -}}
{{- $interval := default "60s" .Values.server.watchConfigInterval -}}
{{- /* A Go duration, compound ones such as 1m30s included, and more than
       zero: the exporter refuses 0s, and the pod would never start. */ -}}
{{- if not (and (regexMatch "^([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+$" $interval) (regexMatch "[1-9]" $interval)) -}}
{{- fail (printf "server.watchConfigInterval %q must be a positive Go duration, for example \"60s\" or \"1m30s\"" $interval) -}}
{{- end -}}
{{- $interval -}}
{{- end }}
{{- define "prometheus-universal-exporter.logLevel" -}}
{{- $level := default "info" .Values.server.logLevel -}}
{{- if not (has $level (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "server.logLevel %q must be debug, info, warn or error" $level) -}}
{{- end -}}
{{- $level -}}
{{- end }}
{{- define "prometheus-universal-exporter.probeTimeoutOffset" -}}
{{- /* Empty leaves the flag out: the exporter's default applies, and an image
       from before the flag existed still starts. */ -}}
{{- $offset := .Values.server.probeTimeoutOffset | default "" | toString -}}
{{- if $offset -}}
{{- if not (regexMatch "^(0|([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+)$" $offset) -}}
{{- fail (printf "server.probeTimeoutOffset %q must be a Go duration of zero or more, for example \"500ms\" or \"1s\"" $offset) -}}
{{- end -}}
{{- $offset -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.probeDefaultTimeout" -}}
{{- /* Empty leaves the flag out, as for probeTimeoutOffset. */ -}}
{{- $timeout := .Values.server.probeDefaultTimeout | default "" | toString -}}
{{- if $timeout -}}
{{- if not (regexMatch "^(0|([0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h))+)$" $timeout) -}}
{{- fail (printf "server.probeDefaultTimeout %q must be a Go duration of zero or more, for example \"30s\" or \"1m\"; 0 leaves a probe without a deadline unbounded" $timeout) -}}
{{- end -}}
{{- $timeout -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.countFlag" -}}
{{- /* A count flag's value, from (list name value): empty or null leaves the
       flag out; otherwise a whole number from 0 to 2147483647, as a number
       or as a string of digits with no zero before another digit: the
       exporter's flag reads 010 as eight and refuses 08, so the chart takes
       neither. The pattern is the values schema's, and spells the range out
       digit by digit, since it has to hold for a string of any length. */ -}}
{{- $name := index . 0 -}}
{{- $value := index . 1 -}}
{{- if not (or (kindIs "invalid" $value) (eq (toString $value) "")) -}}
{{- $text := include "prometheus-universal-exporter.text" $value -}}
{{- if not (regexMatch "^(0|[1-9][0-9]{0,8}|1[0-9]{9}|20[0-9]{8}|21[0-3][0-9]{7}|214[0-6][0-9]{6}|2147[0-3][0-9]{5}|21474[0-7][0-9]{4}|214748[0-2][0-9]{3}|2147483[0-5][0-9]{2}|21474836[0-3][0-9]|214748364[0-7])$" $text) -}}
{{- fail (printf "%s %q must be a whole number from 0 to 2147483647, 0 for no limit, or empty to keep the exporter's default" $name $text) -}}
{{- end -}}
{{- $text -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.shutdownTimeout" -}}
{{- /* Empty leaves the flag out, as for probeTimeoutOffset. Only whole hours,
       minutes and seconds, so the chart can work out the grace period. */ -}}
{{- $timeout := .Values.server.shutdownTimeout | default "" | toString -}}
{{- if $timeout -}}
{{- if not (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $timeout) -}}
{{- fail (printf "server.shutdownTimeout %q must be whole hours, minutes and seconds, for example \"30s\" or \"1m30s\"" $timeout) -}}
{{- end -}}
{{- if eq (include "prometheus-universal-exporter.durationSeconds" $timeout) "0" -}}
{{- fail (printf "server.shutdownTimeout %q must be positive" $timeout) -}}
{{- end -}}
{{- $timeout -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.shutdownDelay" -}}
{{- /* Empty leaves the flag out; 0s is rendered and turns the delay off. Only
       whole hours, minutes and seconds, so the chart can work out the grace
       period. */ -}}
{{- $delay := .Values.server.shutdownDelay | default "" | toString -}}
{{- if $delay -}}
{{- if not (regexMatch "^([0-9]+h)?([0-9]+m)?([0-9]+s)?$" $delay) -}}
{{- fail (printf "server.shutdownDelay %q must be whole hours, minutes and seconds, for example \"5s\" or \"0s\"" $delay) -}}
{{- end -}}
{{- $delay -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.durationMilliseconds" -}}
{{- /* A duration of whole numbers in milliseconds: Go's h, m and s, and the
       ms, d, w and y a Prometheus duration adds, a year being 365 days as
       Prometheus counts it. */ -}}
{{- $seconds := dict "y" 31536000 "w" 604800 "d" 86400 "h" 3600 "m" 60 "s" 1 -}}
{{- $milliseconds := 0 -}}
{{- range $part := regexFindAll "[0-9]+(ms|[ywdhms])" (toString .) -1 -}}
{{- $unit := regexFind "[a-z]+$" $part -}}
{{- $n := trimSuffix $unit $part | atoi -}}
{{- if eq $unit "ms" -}}{{- $milliseconds = add $milliseconds $n -}}
{{- else -}}{{- $milliseconds = add $milliseconds (mul $n (get $seconds $unit) 1000) -}}{{- end -}}
{{- end -}}
{{- $milliseconds -}}
{{- end }}
{{- define "prometheus-universal-exporter.durationSeconds" -}}
{{- div (include "prometheus-universal-exporter.durationMilliseconds" . | int64) 1000 -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateScrapeTiming" -}}
{{- /* Prometheus refuses a scrape whose timeout is longer than its interval,
       so the Prometheus Operator leaves such a monitor out of the
       configuration and its targets are never scraped, with nothing said
       where the values were written. From (list name interval
       scrapeTimeout); one left unset, or 0, takes Prometheus's own default,
       which the chart does not know, and is not compared. */ -}}
{{- $interval := index . 1 | default "" | toString -}}
{{- $timeout := index . 2 | default "" | toString -}}
{{- $every := include "prometheus-universal-exporter.durationMilliseconds" $interval | int64 -}}
{{- $limit := include "prometheus-universal-exporter.durationMilliseconds" $timeout | int64 -}}
{{- if and (gt $every 0) (gt $limit $every) -}}
{{- fail (printf "%s has scrapeTimeout %s, longer than its interval %s; Prometheus refuses a scrape timeout longer than the scrape interval, so lower scrapeTimeout or raise interval" (index . 0) $timeout $interval) -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.terminationGracePeriodSeconds" -}}
{{- /* A stopping pod needs the shutdown delay, the shutdown timeout, then
       time for the last OTLP export and exit. Kubernetes kills it after 30
       seconds unless told otherwise, which would cut them short. */ -}}
{{- $delay := include "prometheus-universal-exporter.shutdownDelay" . | default "0s" -}}
{{- $shutdown := include "prometheus-universal-exporter.shutdownTimeout" . | default "15s" -}}
{{- $needed := add (include "prometheus-universal-exporter.durationSeconds" $delay | atoi) (include "prometheus-universal-exporter.durationSeconds" $shutdown | atoi) 10 -}}
{{- $explicit := .Values.terminationGracePeriodSeconds -}}
{{- if not (kindIs "invalid" $explicit) -}}
{{- $seconds := include "prometheus-universal-exporter.wholeNumber" (list "terminationGracePeriodSeconds" $explicit 0 2147483647) | int64 -}}
{{- if lt $seconds (int64 $needed) -}}
{{- fail (printf "terminationGracePeriodSeconds %d is shorter than the %d seconds a stopping pod needs: server.shutdownDelay (%s), server.shutdownTimeout (%s) and 10 seconds for the last OTLP export and exit; raise it or lower server.shutdownDelay or server.shutdownTimeout" $seconds $needed $delay $shutdown) -}}
{{- end -}}
{{- $seconds -}}
{{- else if gt (int $needed) 30 -}}
{{- $needed -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.pythonPath" -}}
{{- default "/usr/local/bin/python3" .Values.server.pythonPath -}}
{{- end }}
{{- define "prometheus-universal-exporter.staticTargetsFile" -}}
{{- default "static-targets.yaml" .Values.staticTargets.fileName -}}
{{- end }}
{{- define "prometheus-universal-exporter.staticTargetsPath" -}}
{{- $path := default "/static-targets" .Values.staticTargets.path -}}
{{- if eq $path .Values.selfMetrics.path -}}
{{- fail (printf "staticTargets.path %q is also selfMetrics.path; the two endpoints need paths of their own" $path) -}}
{{- end -}}
{{- $path -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateTargets" -}}
{{- if .Values.staticTargets.enabled -}}
{{- /* The file is a key of the configuration ConfigMap, so its name must be
       a ConfigMap key, and not one config.data already uses, which it would
       collide with or silently replace. */ -}}
{{- $fileName := include "prometheus-universal-exporter.staticTargetsFile" . -}}
{{- if or (not (regexMatch "^[-._a-zA-Z0-9]+$" $fileName)) (eq $fileName ".") (eq $fileName "..") -}}
{{- fail (printf "staticTargets.fileName %q must be a ConfigMap key: letters, digits, -, _ and ., with no /" $fileName) -}}
{{- end -}}
{{- if and .Values.config.enabled (hasKey (.Values.config.data | default dict) $fileName) -}}
{{- fail (printf "staticTargets.fileName %q is also a key of config.data; the static target file needs a name of its own in the ConfigMap" $fileName) -}}
{{- end -}}
{{- $data := trim (.Values.staticTargets.data | default "") -}}
{{- if .Values.config.enabled -}}
{{- if not $data -}}
{{- fail "staticTargets.enabled requires staticTargets.data to hold the static target document" -}}
{{- end -}}
{{- else if $data -}}
{{- /* Without config.enabled the chart renders no ConfigMap, so the data
       would be dropped without a word, and the exporter would read whatever
       the ConfigMap supplied instead has under the file name. */ -}}
{{- fail (printf "staticTargets.data is rendered into the chart's ConfigMap, which config.enabled: false leaves out; put the static target file into your ConfigMap %s under the key %s, and leave staticTargets.data empty" (include "prometheus-universal-exporter.fullname" .) $fileName) -}}
{{- end -}}
{{- /* The monitor may read only some targets. A name the document does not
       have would make every scrape of the endpoint fail with 400, so
       rendering fails first. A target without a name is called
       <collector>_<index>, as the exporter calls it. */ -}}
{{- $wanted := .Values.staticTargets.monitor.targets | default list -}}
{{- if and .Values.staticTargets.monitor.enabled $wanted $data -}}
{{- $names := dict -}}
{{- range $index, $target := (get (fromYaml .Values.staticTargets.data) "targets" | default list) -}}
{{- if kindIs "map" $target -}}
{{- $_ := set $names (get $target "name" | default (printf "%v_%d" (get $target "collector") $index) | toString) true -}}
{{- end -}}
{{- end -}}
{{- range $wanted -}}
{{- if not (hasKey $names (toString .)) -}}
{{- fail (printf "staticTargets.monitor.targets names %q, but staticTargets.data has no target of that name; the endpoint would answer every scrape with 400" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.config.enabled -}}
{{- $raw := index .Values.config.data "config.yaml" | default "" -}}
{{- if not (trim $raw) -}}
{{- fail "config.data must contain a config.yaml entry, which is the file the exporter reads. When supplying it with --set-file, quote the whole argument so the escaped dot reaches helm instead of being consumed by the shell." -}}
{{- end -}}
{{- $config := fromYaml $raw -}}
{{- $targets := fromYaml .Values.staticTargets.data -}}
{{- /* A target exported over OTLP needs OTLP export; the exporter refuses to
       start without it, so rendering fails first. */ -}}
{{- if not (dig "otlp" "enabled" false $config) -}}
{{- range (get $targets "targets" | default list) -}}
{{- if and (kindIs "map" .) (get . "export_via_otlp") -}}
{{- fail (printf "static target %v sets export_via_otlp, which needs otlp.enabled: true in config.data.config.yaml; the exporter refuses to start without it" (get . "name" | default "(unnamed)")) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
{{- /* Every probe monitor names a collector, and, when the chart holds the
       configuration, one it defines: a probe of none is answered 400 on
       every scrape. The collectors are those of config.yaml and of the
       collector files among config.data's keys; when config.yaml also lists
       collector files by absolute path, which may lie outside the
       ConfigMap, a name not found here is left to the exporter. */ -}}
{{- define "prometheus-universal-exporter.validateMonitors" -}}
{{- $names := dict -}}
{{- $complete := false -}}
{{- if .Values.config.enabled -}}
{{- $complete = true -}}
{{- range $key, $raw := .Values.config.data -}}
{{- $doc := fromYaml (toString $raw) -}}
{{- if kindIs "map" $doc -}}
{{- range (get $doc "collectors" | default list) -}}
{{- if kindIs "map" . }}{{- $_ := set $names (toString (get . "name")) true }}{{- end -}}
{{- end -}}
{{- if eq $key "config.yaml" -}}
{{- range (get $doc "collector_files" | default list) -}}
{{- if hasPrefix "/" (toString .) }}{{- $complete = false }}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* A monitor is named <fullname>-<name>, so two entries of one name, or
       one named after a monitor the chart renders itself, would render two
       objects of one name, and the second would replace the first. */ -}}
{{- $reserved := dict "self" "the self-metrics monitor" "static-targets" "the static targets monitor" -}}
{{- $seen := dict -}}
{{- range $index, $monitor := .Values.monitors -}}
{{- $name := toString ($monitor.name | default "") -}}
{{- if $name -}}
{{- if hasKey $reserved $name -}}
{{- fail (printf "monitors entry %q has the name of %s, <fullname>-%s, which the chart renders itself; choose another name" $name (get $reserved $name) $name) -}}
{{- end -}}
{{- if hasKey $seen $name -}}
{{- fail (printf "monitors entries #%v and #%d are both named %q; each entry renders <fullname>-<name>, so names must be unique" (get $seen $name) $index $name) -}}
{{- end -}}
{{- $_ := set $seen $name $index -}}
{{- end -}}
{{- end -}}
{{- range $index, $monitor := .Values.monitors -}}
{{- if $monitor.enabled -}}
{{- $label := $monitor.name | default (printf "#%d" $index) -}}
{{- /* The chart renders the collector parameter from .collector and the
       target parameter from each discovered target's address, so a params
       entry of either would render a second key of that name, and skip the
       collector check above. */ -}}
{{- range $key := list "collector" "target" -}}
{{- if hasKey ($monitor.params | default dict) $key -}}
{{- if eq $key "collector" -}}
{{- fail (printf "monitors entry %s sets params.collector; the chart renders the collector parameter itself, so set the entry's .collector instead" $label) -}}
{{- else -}}
{{- fail (printf "monitors entry %s sets params.target; the chart sets the target parameter from each discovered target's address, so select the targets with .targetSelector and name the collector with .collector instead" $label) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not $monitor.collector -}}
{{- fail (printf "monitors entry %s names no collector; every probe it sends would be answered 400" $label) -}}
{{- end -}}
{{- if and $complete (not (hasKey $names $monitor.collector)) (not (contains "${" $monitor.collector)) -}}
{{- fail (printf "monitors entry %s names collector %q, which config.data does not define; every probe it sends would be answered 400" $label $monitor.collector) -}}
{{- end -}}
{{- include "prometheus-universal-exporter.validateScrapeTiming" (list (printf "monitors entry %s" $label) $monitor.interval $monitor.scrapeTimeout) -}}
{{- end -}}
{{- end -}}
{{- /* The chart's own monitors, while their values say to render them. */ -}}
{{- if .Values.selfMetrics.enabled -}}
{{- include "prometheus-universal-exporter.validateScrapeTiming" (list "selfMetrics" .Values.selfMetrics.interval .Values.selfMetrics.scrapeTimeout) -}}
{{- end -}}
{{- if and .Values.staticTargets.enabled .Values.staticTargets.monitor.enabled -}}
{{- include "prometheus-universal-exporter.validateScrapeTiming" (list "staticTargets.monitor" .Values.staticTargets.monitor.interval .Values.staticTargets.monitor.scrapeTimeout) -}}
{{- end -}}
{{- end }}
{{- /* The credential a probe monitor's endpoint presents, from (list root
       monitor): the entry's own auth when it is enabled, else the webAuth
       Secret's while webAuth is enabled, else none. An enabled auth says
       which kind it is and which Secret holds it: without a type no
       credential would be rendered, or the exporter's own instead of the
       entry's, and without a Secret a selector naming none, so Prometheus
       would scrape with the wrong credential or with none and nothing would
       say why. */ -}}
{{- define "prometheus-universal-exporter.monitorAuth" -}}
{{- $root := index . 0 -}}
{{- $monitor := index . 1 -}}
{{- $auth := $monitor.auth | default dict -}}
{{- if $auth.enabled -}}
{{- if not (has $auth.type (list "bearer" "basic")) -}}
{{- fail (printf "monitors entry %q enables auth without a type; set its auth.type to bearer or basic" (toString $monitor.name)) -}}
{{- end -}}
{{- if not $auth.secretName -}}
{{- fail (printf "monitors entry %q enables auth without a Secret; set its auth.secretName to the Secret holding the %s credential" (toString $monitor.name) $auth.type) -}}
{{- end -}}
{{- if eq $auth.type "bearer" -}}
authorization:
  type: Bearer
  credentials:
    name: {{ $auth.secretName | quote }}
    key: {{ $auth.secretKey | default "token" | quote }}
    optional: {{ $auth.optional | default false }}
{{- else -}}
basicAuth:
  username:
    name: {{ $auth.secretName | quote }}
    key: {{ $auth.usernameKey | default "username" | quote }}
    optional: {{ $auth.optional | default false }}
  password:
    name: {{ $auth.secretName | quote }}
    key: {{ $auth.passwordKey | default "password" | quote }}
    optional: {{ $auth.optional | default false }}
{{- end -}}
{{- else if and $root.Values.webAuth $root.Values.webAuth.enabled -}}
{{- /* The exporter's own Basic Auth protects /probe too, so a monitor
       without a credential of its own presents the exporter's, as the
       self-metrics monitor does; without it every scrape is a 401. */ -}}
basicAuth:
  username:
    name: {{ $root.Values.webAuth.secretName | quote }}
    key: {{ $root.Values.webAuth.usernameKey | quote }}
  password:
    name: {{ $root.Values.webAuth.secretName | quote }}
    key: {{ $root.Values.webAuth.passwordKey | quote }}
{{- end -}}
{{- end }}
{{- /* The port of a probe monitor's endpoint, from its monitors entry: http
       unless the entry names another. It is a port's name, which is what the
       Prometheus Operator's port field takes: of a Service port for type
       service, a DNS label of up to 63 characters, and of a container port
       for type pod, up to 15 with no two hyphens in a row. A number, 9115 or
       "9115", would be rendered as a name and looked for among the ports'
       names, so the monitor would find no target and nothing would say why;
       it is refused here, as the values schema refuses it, and so is any
       other text no port is named. A name needs a letter: Kubernetes lets a
       Service port be named in digits alone, which the chart does not take,
       since it cannot tell such a name from a number given by mistake. */ -}}
{{- define "prometheus-universal-exporter.monitorPort" -}}
{{- if or (not (hasKey . "port")) (kindIs "invalid" .port) -}}
http
{{- else -}}
{{- $text := include "prometheus-universal-exporter.text" .port -}}
{{- $name := toString .name -}}
{{- $pod := eq (toString .type) "pod" -}}
{{- if regexMatch "^[0-9]+$" $text -}}
{{- if $pod -}}
{{- fail (printf "monitors entry %q has port %s, a port number; a monitor of type pod takes the name of a container port, so name the port in the selected pods' spec.containers[].ports and set port to that name" $name $text) -}}
{{- end -}}
{{- fail (printf "monitors entry %q has port %s, a port number; a monitor of type service takes the name of a Service port, so name the port in the selected Services' spec.ports and set port to that name" $name $text) -}}
{{- end -}}
{{- $named := and (kindIs "string" .port) (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $text) (regexMatch "[a-z]" $text) -}}
{{- if $pod -}}
{{- if not (and $named (le (len $text) 15) (not (contains "--" $text))) -}}
{{- fail (printf "monitors entry %q has port %q; a monitor of type pod takes the name of a container port: 1 to 15 lower-case letters, digits and hyphens, at least one of them a letter, with no hyphen first or last and no two in a row" $name $text) -}}
{{- end -}}
{{- else if not (and $named (le (len $text) 63)) -}}
{{- fail (printf "monitors entry %q has port %q; a monitor of type service takes the name of a Service port: 1 to 63 lower-case letters, digits and hyphens, with no hyphen first or last, and, by this chart's own rule, at least one letter, since it takes digits alone for a port number" $name $text) -}}
{{- end -}}
{{- $text -}}
{{- end -}}
{{- end }}
{{- /* Whether a file of the configuration ConfigMap can be written as a
       block scalar and read back as the same bytes: "true", or empty. It
       cannot with control characters, carriage returns and the other line
       breaks YAML would rewrite, a byte order mark, bytes that are not
       UTF-8, or anything but one line break or none after its last visible
       character, since what ends a manifest in white space is helm's to
       trim; such a file goes into binaryData instead, which the pod mounts
       as the same file. */ -}}
{{- define "prometheus-universal-exporter.textFile" -}}
{{- if and (regexMatch "[^\\s\\p{Z}]\\n?$" .) (not (regexMatch "[^\\t\\n\\x20-\\x7e\\x{a0}-\\x{2027}\\x{202a}-\\x{d7ff}\\x{e000}-\\x{fefe}\\x{ff00}-\\x{fffc}\\x{10000}-\\x{10ffff}]" .)) -}}
true
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "prometheus-universal-exporter.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ default "default" .Values.serviceAccount.name }}{{ end }}
{{- end }}

{{- define "prometheus-universal-exporter.rollingUpdateCheck" -}}
{{- /* Kubernetes refuses a rolling update whose maxSurge and maxUnavailable
       are both 0, as a number or as 0%, since it could then neither add a
       pod nor take one away. Only what is rendered counts: a count left out
       is Kubernetes' default, 25%, which is not 0. */ -}}
{{- if and (kindIs "map" .) (include "prometheus-universal-exporter.zeroCount" .maxSurge) (include "prometheus-universal-exporter.zeroCount" .maxUnavailable) -}}
{{- fail (printf "strategy.rollingUpdate.maxSurge %v and strategy.rollingUpdate.maxUnavailable %v are both 0, which Kubernetes refuses, since the rolling update could then neither add a pod nor take one away; set one of them above 0, such as maxSurge: 1, or set strategy.type: Recreate" .maxSurge .maxUnavailable) -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.zeroCount" -}}
{{- /* Whether a rolling update's count is 0: a number of 0, or a percentage
       of 0, which Kubernetes reads with any zeros before it. */ -}}
{{- if kindIs "string" . -}}
{{- if regexMatch "^0+%$" . }}true{{ end -}}
{{- else if and (not (kindIs "invalid" .)) (not (kindIs "bool" .)) (regexMatch "^-?0+(\\.0*)?$" (toString .)) -}}
true
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.memoryLimitRatio" -}}
{{- /* --runtime.memory-limit-ratio from goMemLimit: empty, leaving the flag
       out, when it is off. The exporter reads the container's memory limit
       from its cgroup, so no limit is needed here; without one it keeps the
       Go default. A number of a values file is floating point, which a
       template prints with an exponent below 0.0001, and toJson below
       0.000001. So the number is written out with the digits toJson gives
       it, 1.5e-7 as 0.00000015, and the message refusing it below 0.1 says
       the value as it reads; a string is checked as written. */ -}}
{{- if .Values.goMemLimit.enabled -}}
{{- $ratio := .Values.goMemLimit.ratio | toString -}}
{{- if kindIs "float64" .Values.goMemLimit.ratio -}}
{{- $ratio = toJson .Values.goMemLimit.ratio -}}
{{- $exponent := "^([1-9])\\.?([0-9]*)e-0*([1-9][0-9]*)$" -}}
{{- if regexMatch $exponent $ratio -}}
{{- $zeros := regexReplaceAll $exponent $ratio "${3}" | atoi | add -1 | int -}}
{{- $ratio = printf "0.%s%s" (repeat $zeros "0") (regexReplaceAll $exponent $ratio "${1}${2}") -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch "^(0?\\.[0-9]*[1-9][0-9]*|1(\\.0*)?)$" $ratio) -}}
{{- fail (printf "goMemLimit.ratio %s must be more than 0 and at most 1, such as 0.8" $ratio) -}}
{{- end -}}
{{- /* Below 0.1 the Go memory limit leaves the heap almost nothing, and the
       runtime would spend its time collecting garbage; the exporter refuses
       it too. The ratio is written out by now, so a first decimal of 0 is
       exactly a ratio below 0.1. */ -}}
{{- if regexMatch "^0?\\.0" $ratio -}}
{{- fail (printf "goMemLimit.ratio %s must be at least 0.1, since below it the Go memory limit leaves the heap almost nothing and the Go runtime spends its time collecting garbage; use 0.5 to 0.95, or set goMemLimit.enabled to false" $ratio) -}}
{{- end -}}
{{- $ratio -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.goGC" -}}
{{- /* The GOGC environment variable from goGC.percent: empty, leaving the
       variable out and Go's default of 100 in place, when it is empty or
       null. The Go runtime reads the variable itself, so the exporter has no
       flag for it. A whole number from 1 to 10000, or off. */ -}}
{{- $percent := (.Values.goGC | default dict).percent -}}
{{- if not (or (kindIs "invalid" $percent) (eq (toString $percent) "")) -}}
{{- $text := include "prometheus-universal-exporter.text" $percent -}}
{{- if not (regexMatch "^([1-9][0-9]{0,3}|10000|off)$" $text) -}}
{{- fail (printf "goGC.percent %q must be a whole number from 1 to 10000, or \"off\" — in quotes in a values file, since YAML reads a bare off as false — or empty to keep Go's default of 100" $text) -}}
{{- end -}}
{{- /* The variable has one source. Kubernetes accepts a name twice in env
       and keeps the last, so a GOGC entry there would replace this one
       without a word, or be replaced by it. An entry on its own, with no
       percent here, is rendered as it always was. */ -}}
{{- $memoryLimit := "" -}}
{{- range $entry := .Values.env -}}
{{- if eq (toString $entry.name) "GOGC" -}}
{{- fail (printf "goGC.percent is %s and env has a GOGC entry too: the container would get the variable from both, and Kubernetes keeps the last; remove the env entry, or leave goGC.percent empty" $text) -}}
{{- end -}}
{{- if eq (toString $entry.name) "GOMEMLIMIT" -}}
{{- $value := get $entry "value" | default "" | toString -}}
{{- if eq $value "off" -}}
{{- $memoryLimit = "off" -}}
{{- else if or $value (hasKey $entry "valueFrom") -}}
{{- $memoryLimit = "set" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* With off, nothing collects until the Go memory limit is near, so
       without one the heap grows until the node or the container's limit
       kills the pod. The limit is in force as memoryLimitRatio above gives
       it: goMemLimit.enabled renders the ratio, and the exporter then takes
       that share of the container's memory limit, which is there only with
       resources.limits.memory. GOMEMLIMIT in env wins over the ratio in the
       exporter, so it does here: an entry is the operator's own limit,
       unless it says off; an empty one the exporter takes as unset. A
       number needs no limit: the heap then stays within its multiple of the
       live data. */ -}}
{{- if eq $text "off" -}}
{{- $why := "" -}}
{{- $remedy := "" -}}
{{- if eq $memoryLimit "off" -}}
{{- $why = "env sets GOMEMLIMIT to off" -}}
{{- $remedy = "remove that env entry or give it a limit" -}}
{{- else if not $memoryLimit -}}
{{- if not .Values.goMemLimit.enabled -}}
{{- $why = "goMemLimit.enabled is false" -}}
{{- $remedy = "enable goMemLimit, with resources.limits.memory set" -}}
{{- else if not (dig "limits" "memory" "" (.Values.resources | default dict)) -}}
{{- $why = "resources.limits.memory is not set" -}}
{{- $remedy = "set resources.limits.memory" -}}
{{- end -}}
{{- end -}}
{{- if $why -}}
{{- fail (printf "goGC.percent is off, which leaves garbage collection to the Go memory limit alone, and %s, so no Go memory limit is in force and the heap would grow without bound; %s, or set goGC.percent to a number such as 400" $why $remedy) -}}
{{- end -}}
{{- end -}}
{{- $text -}}
{{- end -}}
{{- end }}
{{- define "prometheus-universal-exporter.validateProbe" -}}
{{- /* A probe's settings must not replace the check itself: the chart owns
       the path and port. (list name probe) */ -}}
{{- $name := index . 0 -}}
{{- range $key := list "httpGet" "exec" "tcpSocket" "grpc" -}}
{{- if hasKey (index $ 1 | default dict) $key -}}
{{- fail (printf "%s.%s is set by the chart, which checks /health and /ready on the http port; set only timings such as timeoutSeconds and failureThreshold" $name $key) -}}
{{- end -}}
{{- end -}}
{{- end }}
