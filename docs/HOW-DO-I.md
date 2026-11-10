# How do I…

Find your task, follow the link. Each link goes to the part of the reference
that does it, usually with an example to copy. New here? Start with
[Getting started](GETTING-STARTED.md).

## Read data

| I want to… | Read |
| --- | --- |
| Turn a JSON API into metrics | [Getting started](GETTING-STARTED.md), then [Metrics per item](CONFIGURATION.md#metrics-per-item) |
| Know which request type fits my source | [Which request type?](CONFIGURATION.md#which-request-type) |
| Read YAML, XML, CSV, HTML, logfmt, TOML, INI or plain text | [Collectors](CONFIGURATION.md#collectors), and for CSV [Reading CSV](CONFIGURATION.md#reading-csv-what-to-expect) |
| Pass through, filter or rename metrics a service already exposes in Prometheus format | a `prometheus` transform with no `metrics` rules, and its `include`, `exclude` and `rename`, in [Collector-wide labels](CONFIGURATION.md#collector-wide-labels) |
| Make one series per item of a list | [Metrics per item](CONFIGURATION.md#metrics-per-item) |
| Turn a status text such as `"healthy"` into a number | [Turning a status into metrics](CONFIGURATION.md#turning-a-status-into-metrics) and [Mapping text to values](CONFIGURATION.md#mapping-text-to-values-and-scaling-them) |
| Use a date or a time as the value | [Reading a date or a time as the value](CONFIGURATION.md#reading-a-date-or-a-time-as-the-value) |
| Do something no expression can | [Python](PYTHON.md) |
| Read a paged API | [Pagination](REQUESTS.md#pagination) |
| Read a file on this host | [Local files](LOCALFILE.md) |
| Read a file on another host | [Files over SSH](SSHFILE.md) |
| Run a command on another host and read its output | [Commands over SSH](SSHEXEC.md) |
| Read objects in S3, MinIO or another store with its API | [Objects in S3](S3FILE.md) |
| Run a SQL query | [Databases over SQL](SQL.md) |
| Read Redis or Valkey | [Redis and Valkey](REDIS.md) |
| Ask a service that speaks a line protocol over TCP or UDP | [Text protocols over TCP](TCP.md) |
| Search an LDAP directory | [LDAP directories](LDAP.md) |
| Call a gRPC method | [gRPC](GRPC.md) |
| Read series from Graphite | [Graphite](GRAPHITE.md) |
| Read Kubernetes objects | [Kubernetes objects](KUBERNETES.md) |
| Read an SNMP agent | [SNMP agents](SNMP.md) |
| Read registers of a PLC, an inverter or a meter | [Modbus](MODBUS.md), in a build with the opt-in type |
| Read what devices publish to an MQTT broker | [MQTT](MQTT.md), in a build with the opt-in type |
| Read a message from a WebSocket | [WebSocket](WEBSOCKET.md), in a build with the opt-in type |
| Read a file from an FTP server | [Files over FTP](FTP.md), in a build with the opt-in type |

## Check that something works

| I want to… | Read |
| --- | --- |
| Check that a website is up, as blackbox_exporter does | [Uptime probes](REQUESTS.md#uptime-probes) |
| Check a TLS certificate's expiry | [TLS certificates](TLSCERT.md) |
| Check that a port is open | [Checking that a port is open](TCP.md#checking-that-a-port-is-open) |
| Ping a host | [Ping over ICMP](ICMP.md) |
| Check a DNS name | [DNS](DNS.md) |
| Check a clock against an NTP server | [NTP](NTP.md) |
| Check that a mail server takes a login, or a recipient, without sending | [SMTP](SMTP.md), in a build with the opt-in type |

## Ask the right way

| I want to… | Read |
| --- | --- |
| Use one collector for many tenants, places or IDs | [Path parameters](REQUESTS.md#path-parameters) |
| Put today's or yesterday's date in a request | [Dates and times](REQUESTS.md#dates-and-times) |
| Ask only for what happened since the last scrape | [Since the last success](REQUESTS.md#since-the-last-success) |
| Send a username and password, or a token from a file | [Reaching the target](AUTHENTICATION.md#reaching-the-target) |
| Log in with a JSON request for a token | [A JSON login](AUTHENTICATION.md#a-json-login) |
| Log in with OAuth 2.0 | [OAuth 2.0 client credentials](AUTHENTICATION.md#oauth-20-client-credentials) and [A user's refresh token](AUTHENTICATION.md#a-users-refresh-token) |
| Log in to a site that needs several requests | [A login of several steps](AUTHENTICATION.md#a-login-of-several-steps) |
| Use a client certificate or my own CA | [TLS](REQUESTS.md#tls) |
| Go through a proxy | [Proxies](REQUESTS.md#proxies) |
| Retry, or take a status other than 2xx | [Retries](REQUESTS.md#retries) and [Accepting other statuses](REQUESTS.md#accepting-other-statuses) |
| Stay under an API's rate limit | [Rate limiting](REQUESTS.md#rate-limiting) and [Response caching](CONFIGURATION.md#response-caching) |
| Keep serving the last good result while a source is down | [Serving the last good result](CONFIGURATION.md#serving-the-last-good-result-when-the-target-fails) |
| Stop the exporter reaching hosts it should not | [Restricting targets](REQUESTS.md#restricting-targets) |

## Run it

| I want to… | Read |
| --- | --- |
| Have Prometheus scrape through the exporter | [Getting started](GETTING-STARTED.md#4-point-prometheus-at-it) |
| Have the exporter scrape a fixed list on its own | [Static targets](STATIC-TARGETS.md) |
| Deploy on Kubernetes | [Install with Helm](../README.md#install-with-helm), the [chart README](../charts/prometheus-universal-exporter/README.md) and [Prometheus Operator](PROMETHEUS-OPERATOR.md) |
| Spread targets over several replicas | [Target allocator](ALLOCATOR.md) |
| Change the configuration without a restart | [Watching the configuration](CONFIGURATION.md#watching-the-configuration) and [Reloading on demand](CONFIGURATION.md#reloading-on-demand) |
| Check a configuration in CI before deploying it | [Dry run](CONFIGURATION.md#dry-run) |
| Get editor completion and checks for the configuration | [Editor support](CONFIGURATION.md#editor-support) |
| Split the configuration over several files | [Collector files](CONFIGURATION.md#collector-files) |
| Send metrics, logs and traces over OTLP | [OTLP](OTLP.md) |

## Find out what is wrong

| I want to… | Read |
| --- | --- |
| Try a collector on a live target from the browser | [Debugging collectors](CONFIGURATION.md#debugging-collectors) |
| See why a probe failed | [When a stage of the probe fails](CONFIGURATION.md#when-a-stage-of-the-probe-fails) and [Logging](LOGGING.md) |
| See how each static target is doing | [Status page](STATIC-TARGETS.md#status-page) |
| Watch the exporter itself | [Self-metrics](SELF-METRICS.md) |
| Check my metric names follow Prometheus conventions | [Metric lint](CONFIGURATION.md#metric-lint) |
