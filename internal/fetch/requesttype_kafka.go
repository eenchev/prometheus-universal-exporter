//go:build request_type_kafka

package fetch

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The kafka request type asks a Kafka cluster, over Kafka's own protocol,
// how it stands: its brokers, its topics and their partitions' leaders,
// replicas and offsets, and its consumer groups, their members and their
// lag. It reads no message and writes nothing, and needs nothing outside
// the standard library: every request it sends is one of the admin APIs
// (kafkaproto.go), and it logs in with SASL (kafkasasl.go). Its target is
// a broker to begin with, host or host:port (port 9092), or tls://host:port
// for a listener that speaks TLS.
//
//	request:
//	  type: kafka
//	  username: monitor
//	  password_file: /etc/exporter/kafka-password
//	  sasl_mechanism: SCRAM-SHA-512
//	  include_groups: [billing, 'orders-.*']
//
// A scrape connects to the target and asks for the cluster's metadata,
// which lists the brokers, the topics and each partition's leader and
// replicas. It then connects to the brokers it needs, every one of them,
// each through the collector's target policy and with the same login, and
// asks:
//
//   - every broker for the consumer groups it coordinates (ListGroups);
//   - the broker that listed each chosen group, its coordinator, for the
//     group's state and members (DescribeGroups) and for its committed
//     offsets (OffsetFetch);
//   - each partition's leader for the partition's newest and oldest
//     offsets (ListOffsets), after the committed ones, so that a lag read
//     at once does not go below zero.
//
// The brokers are asked at once, each on its own connection, and every
// connection is closed when the scrape ends. The answer is one JSON
// document: the brokers, the chosen topics with their partitions, and the
// chosen groups with their lag by topic and by partition. A broker that
// cannot be reached once the metadata is read does not fail the scrape:
// what it would have answered is missing, and errors says why, so a rule
// sees a partial answer as one. The target failing does.

// KafkaConfig is a kafka collector's own request keys.
type KafkaConfig struct {
	// SASLMechanism is the login's mechanism: PLAIN, SCRAM-SHA-256,
	// SCRAM-SHA-512 or OAUTHBEARER. Left out, it is PLAIN with a username
	// and OAUTHBEARER with a bearer_token_file.
	SASLMechanism string `yaml:"sasl_mechanism"`
	// IncludeTopics and IncludeGroups are regular expressions, one of
	// which a topic's or a group's whole name matches to be reported, every
	// one when left out; ExcludeTopics and ExcludeGroups leave out those one
	// of theirs matches.
	IncludeTopics []string `yaml:"include_topics"`
	ExcludeTopics []string `yaml:"exclude_topics"`
	IncludeGroups []string `yaml:"include_groups"`
	ExcludeGroups []string `yaml:"exclude_groups"`
	// InternalTopics reports Kafka's own topics too, such as
	// __consumer_offsets.
	InternalTopics bool `yaml:"internal_topics"`
}

// kafkaRequest is a kafka collector's own keys.
func kafkaRequest(c *model.Collector) KafkaConfig {
	own, _ := c.Request.Own.(KafkaConfig)
	return own
}

const (
	kafkaMethod  = "METADATA"
	kafkaTimeout = 30 * time.Second
	// kafkaMaxGroups and kafkaMaxPartitions bound the groups and the
	// partitions a scrape reports, which are each a series or more, and
	// kafkaMaxPatterns and kafkaMaxPattern the patterns of a key and the
	// length of each.
	kafkaMaxGroups     = 1000
	kafkaMaxPartitions = 10000
	kafkaMaxPatterns   = 100
	kafkaMaxPattern    = 1000
)

// kafkaPorts gives each scheme of a target its port.
var kafkaPorts = map[string]int{"": 9092, "kafka": 9092, "tls": 9092}

func init() {
	registerRequestType(&RequestType{
		Name:         RequestTypeKafka,
		Fields:       []string{"username", "password_file", "bearer_token_file", "tls", "max_response_bytes", "allowed_targets", "denied_targets"},
		Config:       KafkaConfig{},
		KeyRules:     kafkaKeyRules,
		Overrides:    []string{"timeout", PathParamPrefix},
		TargetFields: []string{"timeout"},
		Requester:    kafkaRequester{},
		JSONAnswer:   true,
		TargetHint: func(*model.Collector) (string, string) {
			return "Kafka broker", "host, host:9092 or tls://host:9093"
		},
		AnswerHint: "Paste the answer as the JSON document the exporter makes of it, as the Response of a run shows it",
		UnusedParams: func(c *model.Collector, _ RequestOverrides, unused string) error {
			return unusedParamsIn(c, unused, "its label values")
		},
		Stage: "kafka",
		CheckFiles: func(c *model.Collector, t *model.StaticTarget) []string {
			if t != nil {
				return nil
			}
			warnings := brokerFileWarnings(c, c.Request.TLS != (model.TLSConfig{}))
			if _, err := kafkaToken(c); err != nil {
				warnings = append(warnings, fmt.Sprintf("collector %q: %v; a scrape would fail on it now (the file is read at each scrape, so one put in place later is used then)", c.Name, err))
			}
			return warnings
		},
	})
}

// kafkaRequester is the kafka request type's Requester.
type kafkaRequester struct{ parsedTarget[kafkaTarget] }

// kafkaTarget reads a kafka target: label returns the url label of a
// request of it.
type kafkaTarget struct{}

func (kafkaTarget) label(target string) (string, error) {
	address, err := parseNetTarget("kafka", target, kafkaPorts, false)
	if err != nil {
		return "", err
	}
	return kafkaLabel(address), nil
}

// kafkaLabel is the url label of a request to address: kafka:// and the
// broker, tls:// for a target written so.
func kafkaLabel(address netAddress) string {
	if address.scheme == "tls" {
		return "tls://" + address.hostPort
	}
	return "kafka://" + address.hostPort
}

// Method is the http_method label of a kafka request: what a scrape asks
// first.
func (kafkaRequester) Method(*model.Collector, RequestOverrides) string { return kafkaMethod }

// kafkaMechanism is the mechanism a collector logs in with, "" for none.
func kafkaMechanism(c *model.Collector) string {
	switch m := kafkaRequest(c).SASLMechanism; {
	case m != "":
		return m
	case c.Request.BearerTokenFile != "":
		return kafkaOAuthBearer
	case c.Request.Username != "":
		return kafkaPlain
	}
	return ""
}

// Validate checks a kafka collector's request: a mechanism that has the
// credential it sends, and patterns that compile.
func (kafkaRequester) Validate(c *model.Collector) error {
	r, req := kafkaRequest(c), &c.Request
	switch mechanism := kafkaMechanism(c); {
	case r.SASLMechanism != "" && !slices.Contains(kafkaMechanisms, r.SASLMechanism):
		return fmt.Errorf("collector %q request.sasl_mechanism %q is none of %s", c.Name, r.SASLMechanism, strings.Join(kafkaMechanisms, ", "))
	case req.BearerTokenFile != "" && (req.Username != "" || req.PasswordFile != ""):
		return fmt.Errorf("collector %q sets request.bearer_token_file beside request.username or password_file; a kafka login sends a token or a user and a password", c.Name)
	case HasPathParams(req.BearerTokenFile):
		return fmt.Errorf("collector %q request.bearer_token_file has a placeholder; the token is the configuration's", c.Name)
	case mechanism == kafkaOAuthBearer && req.BearerTokenFile == "":
		return fmt.Errorf("collector %q request.sasl_mechanism is OAUTHBEARER, which sends a token: set request.bearer_token_file", c.Name)
	case mechanism != "" && mechanism != kafkaOAuthBearer && req.Username == "":
		return fmt.Errorf("collector %q request.sasl_mechanism is %s, which sends a user and a password: set request.username and password_file", c.Name, mechanism)
	}
	for _, p := range []struct {
		key      string
		patterns []string
	}{{"include_topics", r.IncludeTopics}, {"exclude_topics", r.ExcludeTopics}, {"include_groups", r.IncludeGroups}, {"exclude_groups", r.ExcludeGroups}} {
		if len(p.patterns) > kafkaMaxPatterns {
			return fmt.Errorf("collector %q request.%s has more than %d patterns", c.Name, p.key, kafkaMaxPatterns)
		}
		for i, pattern := range p.patterns {
			if pattern == "" {
				return fmt.Errorf("collector %q request.%s[%d] is empty; a pattern that matches every name is .*", c.Name, p.key, i)
			}
			if len(pattern) > kafkaMaxPattern {
				return fmt.Errorf("collector %q request.%s[%d] is longer than %d characters", c.Name, p.key, i, kafkaMaxPattern)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("collector %q request.%s[%d] %q is not a regular expression: %w", c.Name, p.key, i, pattern, err)
			}
		}
	}
	return checkBrokerAccount(c)
}

// kafkaPattern compiles patterns of names, one of which a name matches
// whole; none is nil, which kafkaChosen reads as its default.
func kafkaPattern(patterns []string) *regexp.Regexp {
	if len(patterns) == 0 {
		return nil
	}
	whole := make([]string, len(patterns))
	for i, pattern := range patterns {
		whole[i] = "(?:" + pattern + ")"
	}
	// Each compiles, as Validate checked.
	re, _ := regexp.Compile("^(?:" + strings.Join(whole, "|") + ")$")
	return re
}

// kafkaKeyRules is what the configuration schema says of a kafka
// collector's own keys.
func kafkaKeyRules() map[string]map[string]any {
	pattern := func(what string) map[string]any {
		return map[string]any{"maxItems": kafkaMaxPatterns, "description": "kafka: " + what}
	}
	item := map[string]any{"minLength": 1, "maxLength": kafkaMaxPattern}
	return map[string]map[string]any{
		"sasl_mechanism":   {"enum": append(slices.Clone(kafkaMechanisms), ""), "description": "kafka: the SASL mechanism of the login. PLAIN, SCRAM-SHA-256 and SCRAM-SHA-512 send username and password_file, OAUTHBEARER sends bearer_token_file. Left out, PLAIN with a username, OAUTHBEARER with a bearer_token_file, and no login without either."},
		"include_topics":   pattern("regular expressions, one of which the whole name of a topic matches to be reported, at most 100; every topic when left out."),
		"include_topics[]": item,
		"exclude_topics":   pattern("regular expressions whose topics are left out, whole names matched, at most 100."),
		"exclude_topics[]": item,
		"include_groups":   pattern("regular expressions, one of which the whole name of a consumer group matches to be reported, at most 100; every group when left out."),
		"include_groups[]": item,
		"exclude_groups":   pattern("regular expressions whose consumer groups are left out, whole names matched, at most 100."),
		"exclude_groups[]": item,
		"internal_topics":  {"description": "kafka: report Kafka's own topics too, such as __consumer_offsets; false when left out."},
	}
}

// kafkaToken reads request.bearer_token_file, "" when the collector names
// none; an empty file is a failure.
func kafkaToken(c *model.Collector) (string, error) {
	if c.Request.BearerTokenFile == "" {
		return "", nil
	}
	token, err := ReadCredentialFile(c.Request.BearerTokenFile)
	if err != nil {
		return "", fmt.Errorf("reading request.bearer_token_file: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("request.bearer_token_file %s is empty", c.Request.BearerTokenFile)
	}
	return token, nil
}

// kafkaTooLarge is the failure of a scrape whose answers pass limit.
func kafkaTooLarge(limit int64) error {
	return model.MarkError(model.Errorf("the brokers' answers come to more than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes, or narrow request.include_topics and request.include_groups", model.Size(limit)), model.ErrLimitExceeded)
}

// The document a kafka scrape hands the transforms.
type (
	kafkaDocument struct {
		// Address is the address the first connection was made to, and TLS
		// whether the connections speak TLS.
		Address    string           `json:"address"`
		TLS        bool             `json:"tls"`
		ClusterID  *string          `json:"cluster_id"`
		Controller int32            `json:"controller"`
		Brokers    []kafkaBrokerDoc `json:"brokers"`
		Topics     []kafkaTopicDoc  `json:"topics"`
		Groups     []kafkaGroupDoc  `json:"groups"`
		// Errors are what went wrong after the metadata was read, each a
		// sentence naming the broker, the topic or the group.
		Errors          []string `json:"errors"`
		DurationSeconds float64  `json:"duration_seconds"`
	}
	kafkaBrokerDoc struct {
		ID         int32   `json:"id"`
		Host       string  `json:"host"`
		Port       int32   `json:"port"`
		Rack       *string `json:"rack"`
		Controller bool    `json:"controller"`
		// LeaderOf and Replicas count the partitions of every topic,
		// chosen or not, that it leads and that it holds a replica of.
		LeaderOf int `json:"leader_of"`
		Replicas int `json:"replicas"`
	}
	kafkaTopicDoc struct {
		Name              string `json:"name"`
		Internal          bool   `json:"internal"`
		Error             string `json:"error"`
		PartitionCount    int    `json:"partition_count"`
		ReplicationFactor int    `json:"replication_factor"`
		Offline           int    `json:"offline"`
		UnderReplicated   int    `json:"under_replicated"`
		// NotPreferredLeader counts the partitions led by another broker
		// than the first of their replicas.
		NotPreferredLeader int `json:"not_preferred_leader"`
		// Messages is newest minus oldest over the partitions, null when an
		// offset of one is missing.
		Messages   *int64              `json:"messages"`
		Partitions []kafkaPartitionDoc `json:"partitions"`
	}
	kafkaPartitionDoc struct {
		Partition       int32   `json:"partition"`
		Leader          int32   `json:"leader"`
		Replicas        []int32 `json:"replicas"`
		ISR             []int32 `json:"isr"`
		OfflineReplicas []int32 `json:"offline_replicas"`
		PreferredLeader bool    `json:"preferred_leader"`
		UnderReplicated bool    `json:"under_replicated"`
		Offline         bool    `json:"offline"`
		Newest          *int64  `json:"newest"`
		Oldest          *int64  `json:"oldest"`
		Error           string  `json:"error"`
	}
	kafkaGroupDoc struct {
		Group        string  `json:"group"`
		ProtocolType string  `json:"protocol_type"`
		State        *string `json:"state"`
		Members      *int    `json:"members"`
		Coordinator  int32   `json:"coordinator"`
		Error        string  `json:"error"`
		// Topics are the chosen topics the group has committed offsets of.
		Topics []kafkaGroupTopicDoc `json:"topics"`
	}
	kafkaGroupTopicDoc struct {
		Group string `json:"group"`
		Topic string `json:"topic"`
		// CommittedSum adds the committed offsets up, and Lag the lag of
		// the partitions that have one, null when none has.
		CommittedSum int64                    `json:"committed_sum"`
		Lag          *int64                   `json:"lag"`
		Partitions   []kafkaGroupPartitionDoc `json:"partitions"`
	}
	kafkaGroupPartitionDoc struct {
		Partition int32 `json:"partition"`
		// Committed is -1, and Lag null, for a partition the group has
		// committed no offset of.
		Committed int64  `json:"committed"`
		Lag       *int64 `json:"lag"`
		Error     string `json:"error"`
	}
)

// Fetch performs one request of a kafka collector.
func (kafkaRequester) Fetch(ctx context.Context, target string, c *model.Collector, overrides RequestOverrides, _ http.Header) (*HTTPResponse, error) {
	start := time.Now()
	address, err := parseNetTarget("kafka", target, kafkaPorts, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := netBudget(ctx, overrides, kafkaTimeout)
	defer cancel()
	if ctx, err = netGuard(ctx, c, "kafka", address.hostPort); err != nil {
		return nil, err
	}
	login := kafkaLogin{mechanism: kafkaMechanism(c), username: c.Request.Username}
	if login.password, err = netPassword(c); err != nil {
		return nil, err
	}
	if login.token, err = kafkaToken(c); err != nil {
		return nil, err
	}
	k := &kafkaCluster{ctx: ctx, c: c, login: login, limit: responseLimit(c), nodes: map[int32]*kafkaNode{}}
	k.budget.left.Store(k.limit)
	if address.scheme == "tls" {
		if k.tls, err = brokerTLSConfig(c); err != nil {
			return nil, err
		}
	}
	defer k.close()
	at := traceRequestAt(ctx, kafkaMethod, kafkaLabel(address), nil, "", false)
	doc, err := k.scrape(address)
	if err != nil {
		at.outcome("error: "+err.Error(), true)
		return nil, err
	}
	at.outcome(fmt.Sprintf("%d brokers, %d topics, %d groups", len(doc.Brokers), len(doc.Topics), len(doc.Groups)), false)
	doc.DurationSeconds = time.Since(start).Seconds()
	return jsonAnswer(doc, target, c, start)
}

// kafkaCluster is a scrape's view of a cluster: its login, its byte
// budget and a connection to each broker it has asked.
type kafkaCluster struct {
	ctx    context.Context
	c      *model.Collector
	login  kafkaLogin
	tls    *tls.Config
	limit  int64
	budget kafkaBudget

	mu    sync.Mutex
	nodes map[int32]*kafkaNode
	boot  *kafkaConn
}

// kafkaNode is a broker of the cluster and its connection, made the first
// time it is asked for. bad says why its address cannot be connected to.
type kafkaNode struct {
	address netAddress
	bad     error
	once    sync.Once
	conn    *kafkaConn
	err     error
}

// kafkaConn is a connection to a broker.
type kafkaConn struct {
	*netConn
	ctx         context.Context
	broker      string
	budget      *kafkaBudget
	limit       int64
	correlation int32
	// greeted says the broker has answered ApiVersions, before which an
	// answer larger than kafkaMaxGreeting is no Kafka broker's.
	greeted bool
}

// kafkaMaxGreeting bounds the answer to ApiVersions, a few hundred bytes
// from a broker: what is no Kafka broker, such as a web server, sends a
// first answer whose first bytes read as a size far larger.
const kafkaMaxGreeting = 64 << 10

// fail is the failure of a read or a write of api's request.
func (c *kafkaConn) fail(api kafkaAPI, doing string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return fmt.Errorf("%s closed the connection while %s %s", c.broker, doing, api.name)
	}
	return netFailure(c.ctx, fmt.Errorf("%s %s to %s: %w", doing, api.name, c.broker, err))
}

// close closes every connection of the scrape.
func (k *kafkaCluster) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.boot != nil {
		k.boot.close()
	}
	for _, n := range k.nodes {
		if n.conn != nil {
			n.conn.close()
		}
	}
}

// dial connects to a broker through the target policy, which judges its
// name as it judges the target's, asks which versions it speaks, and logs
// in.
func (k *kafkaCluster) dial(address netAddress) (*kafkaConn, error) {
	if guard, ok := k.ctx.Value(policyGuardKey{}).(*policyGuard); ok {
		if err := guard.check(k.ctx, &url.URL{Scheme: "kafka", Host: address.hostPort}); err != nil {
			return nil, err
		}
	}
	plain, err := netDial(k.ctx, address, k.tls)
	if err != nil {
		return nil, err
	}
	conn := &kafkaConn{netConn: plain, ctx: k.ctx, broker: address.hostPort, budget: &k.budget, limit: k.limit}
	if err := k.greet(conn); err != nil {
		conn.close()
		return nil, err
	}
	return conn, nil
}

// greet asks conn's broker which versions of each API it speaks, refuses
// one that does not speak those a scrape sends, and logs in.
func (k *kafkaCluster) greet(conn *kafkaConn) error {
	r, err := conn.roundTrip(kafkaAPIVersions, kafkaAPIVersions.version, nil)
	if err != nil {
		if strings.Contains(err.Error(), "closed the connection") {
			return fmt.Errorf("%w; a listener that speaks TLS closes a plaintext connection so, and a target of one is written tls://host:port", err)
		}
		return err
	}
	conn.greeted = true
	// An error answers in version 0's layout, which begins as version 2's.
	code := r.int16()
	spoken := map[int16][2]int16{}
	for n := r.array(6); n > 0; n-- {
		key, low, high := r.int16(), r.int16(), r.int16()
		spoken[key] = [2]int16{low, high}
	}
	if r.err != nil {
		if code == 0 {
			return fmt.Errorf("%s answered ApiVersions with what does not decode, and may not be a Kafka broker: %w", conn.broker, r.err)
		}
	}
	if code != 0 {
		return fmt.Errorf("%s refused ApiVersions v%d (%s): it is older than Kafka 2.1, which the kafka type needs", conn.broker, kafkaAPIVersions.version, kafkaError(code))
	}
	needed := slices.Clone(kafkaAPIs)
	if k.login.mechanism != "" {
		needed = append(needed, kafkaSaslHandshake, kafkaSaslAuthenticate)
	}
	for _, api := range needed {
		versions, ok := spoken[api.key]
		switch {
		case !ok:
			return fmt.Errorf("%s does not speak %s, which the kafka type needs; it needs Kafka 2.1 or later", conn.broker, api.name)
		case api.version < versions[0] || api.version > versions[1]:
			return fmt.Errorf("%s speaks %s v%d to v%d, not v%d, which the kafka type sends; it needs Kafka 2.1 or later", conn.broker, api.name, versions[0], versions[1], api.version)
		}
	}
	if k.login.mechanism == "" {
		return nil
	}
	authenticate := int16(0)
	if spoken[kafkaSaslAuthenticate.key][1] >= 1 {
		authenticate = 1
	}
	return conn.login(k.login, authenticate)
}

// node is the connection to broker id, made now if it was not.
func (k *kafkaCluster) node(id int32) (*kafkaConn, error) {
	k.mu.Lock()
	n := k.nodes[id]
	k.mu.Unlock()
	switch {
	case n == nil:
		return nil, fmt.Errorf("broker %d is not among the brokers the metadata lists", id)
	case n.bad != nil:
		return nil, n.bad
	}
	n.once.Do(func() {
		conn, err := k.dial(n.address)
		k.mu.Lock()
		n.conn, n.err = conn, err
		k.mu.Unlock()
	})
	return n.conn, n.err
}

// learn records where broker id is. An address that cannot be connected
// to is the broker's failure, met when it is asked for (node), so that one
// broker's does not fail the scrape.
func (k *kafkaCluster) learn(id int32, host string, port int32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	written := net.JoinHostPort(host, strconv.Itoa(int(port)))
	canonical, err := canonicalHost(host)
	switch {
	case err != nil:
		k.nodes[id] = &kafkaNode{bad: fmt.Errorf("broker %d's address %s: %w", id, brokerSaid(written), err)}
	case host == "" || port < 1 || port > 65535:
		k.nodes[id] = &kafkaNode{bad: fmt.Errorf("broker %d has the address %s, which no connection can use", id, brokerSaid(written))}
	default:
		k.nodes[id] = &kafkaNode{address: netAddress{host: canonical, hostPort: net.JoinHostPort(canonical, strconv.Itoa(int(port)))}}
	}
}

// each runs work on the connection of each broker of ids at once, and
// returns the failures by broker. A broker it cannot connect to fails too.
func (k *kafkaCluster) each(ids []int32, work func(id int32, conn *kafkaConn) error) map[int32]error {
	var (
		wg     sync.WaitGroup
		panics goroutinePanics
		mu     sync.Mutex
		failed = map[int32]error{}
	)
	for _, id := range ids {
		wg.Go(func() {
			defer panics.keep()
			conn, err := k.node(id)
			if err == nil {
				err = work(id, conn)
			}
			if err != nil {
				mu.Lock()
				failed[id] = err
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	panics.raise()
	return failed
}

// kafkaChosen says whether name is reported: the include pattern matches
// it, or there is none, and the exclude pattern does not.
func kafkaChosen(name string, include, exclude *regexp.Regexp) bool {
	return (include == nil || include.MatchString(name)) && (exclude == nil || !exclude.MatchString(name))
}

// kafkaPartitionKey names a partition.
type kafkaPartitionKey struct {
	topic     string
	partition int32
}

// scrape asks the cluster what a scrape reports.
func (k *kafkaCluster) scrape(address netAddress) (*kafkaDocument, error) {
	boot, err := k.dial(address)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.boot = boot
	k.mu.Unlock()
	doc := &kafkaDocument{Address: boot.RemoteAddr().String(), TLS: k.tls != nil, Brokers: []kafkaBrokerDoc{}, Topics: []kafkaTopicDoc{}, Groups: []kafkaGroupDoc{}, Errors: []string{}}
	if err := k.metadata(boot, doc); err != nil {
		return nil, err
	}
	failed := func(err error) bool {
		if k.ctx.Err() != nil || errors.Is(err, model.ErrLimitExceeded) {
			return true
		}
		doc.Errors = append(doc.Errors, err.Error())
		return false
	}
	if err := k.groups(doc, failed); err != nil {
		return nil, err
	}
	if err := k.offsets(doc, failed); err != nil {
		return nil, err
	}
	if err := k.ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(doc.Errors)
	return doc, nil
}

// metadata reads the cluster's brokers and topics into doc, the chosen
// topics and their partitions.
func (k *kafkaCluster) metadata(boot *kafkaConn, doc *kafkaDocument) error {
	own := kafkaRequest(k.c)
	include, exclude := kafkaPattern(own.IncludeTopics), kafkaPattern(own.ExcludeTopics)
	w := kafkaWriter{}
	w.array(-1) // every topic
	w.bool(false)
	r, err := boot.roundTrip(kafkaMetadata, kafkaMetadata.version, w.b)
	if err != nil {
		return err
	}
	r.int32() // throttle_time_ms
	byID := map[int32]int{}
	for n := r.array(12); n > 0; n-- {
		b := kafkaBrokerDoc{ID: r.int32(), Host: r.string(), Port: r.int32()}
		if rack, ok := r.nullableString(); ok {
			b.Rack = &rack
		}
		byID[b.ID] = len(doc.Brokers)
		doc.Brokers = append(doc.Brokers, b)
	}
	if id, ok := r.nullableString(); ok {
		doc.ClusterID = &id
	}
	doc.Controller = r.int32()
	partitions := 0
	for n := r.array(9); n > 0; n-- {
		t := kafkaTopicDoc{Partitions: []kafkaPartitionDoc{}}
		code := r.int16()
		t.Name, t.Internal = r.string(), r.bool()
		if code != 0 {
			t.Error = kafkaError(code)
		}
		for m := r.array(26); m > 0; m-- {
			p := kafkaPartitionDoc{}
			code := r.int16()
			p.Partition, p.Leader = r.int32(), r.int32()
			r.int32() // leader_epoch
			p.Replicas, p.ISR, p.OfflineReplicas = r.int32s(), r.int32s(), r.int32s()
			if code != 0 && code != 9 { // REPLICA_NOT_AVAILABLE still describes the partition
				p.Error = kafkaError(code)
			}
			p.Offline = p.Leader < 0
			p.PreferredLeader = len(p.Replicas) > 0 && p.Replicas[0] == p.Leader
			p.UnderReplicated = len(p.ISR) < len(p.Replicas)
			if i, ok := byID[p.Leader]; ok {
				doc.Brokers[i].LeaderOf++
			}
			for _, replica := range p.Replicas {
				if i, ok := byID[replica]; ok {
					doc.Brokers[i].Replicas++
				}
			}
			t.Partitions = append(t.Partitions, p)
		}
		if (t.Internal && !own.InternalTopics) || !kafkaChosen(t.Name, include, exclude) {
			continue
		}
		slices.SortFunc(t.Partitions, func(a, b kafkaPartitionDoc) int { return int(a.Partition - b.Partition) })
		t.PartitionCount = len(t.Partitions)
		for _, p := range t.Partitions {
			t.ReplicationFactor = max(t.ReplicationFactor, len(p.Replicas))
			if p.Offline {
				t.Offline++
			}
			if p.UnderReplicated {
				t.UnderReplicated++
			}
			if !p.PreferredLeader && !p.Offline {
				t.NotPreferredLeader++
			}
		}
		partitions += t.PartitionCount
		doc.Topics = append(doc.Topics, t)
	}
	if err := boot.decoded(kafkaMetadata, r); err != nil {
		return err
	}
	if partitions > kafkaMaxPartitions {
		return fmt.Errorf("the chosen topics have %d partitions, more than the %d a scrape reports; narrow request.include_topics or set request.exclude_topics", partitions, kafkaMaxPartitions)
	}
	for i := range doc.Brokers {
		b := &doc.Brokers[i]
		b.Controller = b.ID == doc.Controller
		k.learn(b.ID, b.Host, b.Port)
	}
	slices.SortFunc(doc.Brokers, func(a, b kafkaBrokerDoc) int { return int(a.ID - b.ID) })
	slices.SortFunc(doc.Topics, func(a, b kafkaTopicDoc) int { return strings.Compare(a.Name, b.Name) })
	return nil
}

// groups lists the consumer groups on every broker, finds each chosen
// one's coordinator, and asks it the group's state, members and committed
// offsets, into doc.
func (k *kafkaCluster) groups(doc *kafkaDocument, failed func(error) bool) error {
	own := kafkaRequest(k.c)
	include, exclude := kafkaPattern(own.IncludeGroups), kafkaPattern(own.ExcludeGroups)
	ids := make([]int32, 0, len(doc.Brokers))
	for _, b := range doc.Brokers {
		ids = append(ids, b.ID)
	}
	// A broker lists the groups it coordinates, so the broker that lists
	// a group is the one to ask about it.
	type listing struct {
		protocol    string
		coordinator int32
	}
	var mu sync.Mutex
	listed := map[string]listing{}
	for id, err := range k.each(ids, func(id int32, conn *kafkaConn) error {
		r, err := conn.roundTrip(kafkaListGroups, kafkaListGroups.version, nil)
		if err != nil {
			return err
		}
		r.int32() // throttle_time_ms
		code := r.int16()
		found := map[string]string{}
		for n := r.array(4); n > 0; n-- {
			group := r.string()
			found[group] = r.string()
		}
		if err := conn.decoded(kafkaListGroups, r); err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("%s refused ListGroups: %s", conn.broker, kafkaError(code))
		}
		mu.Lock()
		for group, protocol := range found {
			listed[group] = listing{protocol, id}
		}
		mu.Unlock()
		return nil
	}) {
		if failed(fmt.Errorf("listing the groups of broker %d: %w", id, err)) {
			return err
		}
	}
	names := []string{}
	for group := range listed {
		if kafkaChosen(group, include, exclude) {
			names = append(names, group)
		}
	}
	slices.Sort(names)
	if len(names) > kafkaMaxGroups {
		return fmt.Errorf("%d consumer groups are chosen, more than the %d a scrape reports; narrow request.include_groups or set request.exclude_groups", len(names), kafkaMaxGroups)
	}
	doc.Groups = make([]kafkaGroupDoc, len(names))
	byCoordinator := map[int32][]int{}
	for i, name := range names {
		g := &doc.Groups[i]
		l := listed[name]
		g.Group, g.ProtocolType, g.Coordinator, g.Topics = name, l.protocol, l.coordinator, []kafkaGroupTopicDoc{}
		byCoordinator[l.coordinator] = append(byCoordinator[l.coordinator], i)
	}
	chosen := map[string]bool{}
	for _, t := range doc.Topics {
		chosen[t.Name] = true
	}
	for id, err := range k.each(slices.Sorted(maps.Keys(byCoordinator)), func(id int32, conn *kafkaConn) error {
		return k.describe(conn, doc.Groups, byCoordinator[id], chosen)
	}) {
		err = fmt.Errorf("asking broker %d about its groups: %w", id, err)
		if failed(err) {
			return err
		}
		for _, i := range byCoordinator[id] {
			if doc.Groups[i].Error == "" {
				doc.Groups[i].Error = err.Error()
			}
		}
	}
	// A group its coordinator refused to describe, or to give the offsets
	// of, is among the errors too.
	for _, g := range doc.Groups {
		if g.Error != "" && !slices.Contains(doc.Errors, g.Error) {
			doc.Errors = append(doc.Errors, g.Error)
		}
	}
	return nil
}

// describe asks a coordinator the state and the members of the groups of
// doc at indexes, then each one's committed offsets of the chosen topics.
func (k *kafkaCluster) describe(conn *kafkaConn, groups []kafkaGroupDoc, indexes []int, chosen map[string]bool) error {
	w := kafkaWriter{}
	w.array(len(indexes))
	for _, i := range indexes {
		w.string(groups[i].Group)
	}
	r, err := conn.roundTrip(kafkaDescribeGroups, kafkaDescribeGroups.version, w.b)
	if err != nil {
		return err
	}
	r.int32() // throttle_time_ms
	at := map[string]int{}
	for _, i := range indexes {
		at[groups[i].Group] = i
	}
	for n := r.array(12); n > 0; n-- {
		code := r.int16()
		name, state := r.string(), r.string()
		r.string() // protocol_type, which ListGroups gave
		r.string() // protocol_data
		members := r.array(14)
		for range members {
			r.string() // member_id
			r.string() // client_id
			r.string() // client_host
			r.bytes()  // member_metadata
			r.bytes()  // member_assignment
		}
		i, ok := at[name]
		if !ok || r.err != nil {
			continue
		}
		if code != 0 {
			groups[i].Error = fmt.Sprintf("describing group %q: %s", name, kafkaError(code))
			continue
		}
		groups[i].State, groups[i].Members = &state, &members
	}
	if err := conn.decoded(kafkaDescribeGroups, r); err != nil {
		return err
	}
	for _, i := range indexes {
		if err := k.committed(conn, &groups[i], chosen); err != nil {
			return err
		}
	}
	return nil
}

// committed asks a coordinator the offsets g committed, of every topic,
// and keeps those of the chosen topics.
func (k *kafkaCluster) committed(conn *kafkaConn, g *kafkaGroupDoc, chosen map[string]bool) error {
	w := kafkaWriter{}
	w.string(g.Group)
	w.array(-1) // every topic
	r, err := conn.roundTrip(kafkaOffsetFetch, kafkaOffsetFetch.version, w.b)
	if err != nil {
		return err
	}
	r.int32() // throttle_time_ms
	for n := r.array(6); n > 0; n-- {
		t := kafkaGroupTopicDoc{Group: g.Group, Topic: r.string(), Partitions: []kafkaGroupPartitionDoc{}}
		for m := r.array(18); m > 0; m-- {
			p := kafkaGroupPartitionDoc{Partition: r.int32(), Committed: r.int64()}
			r.int32()          // committed_leader_epoch
			r.nullableString() // metadata
			if code := r.int16(); code != 0 {
				p.Error = kafkaError(code)
			}
			t.Partitions = append(t.Partitions, p)
		}
		if chosen[t.Topic] {
			slices.SortFunc(t.Partitions, func(a, b kafkaGroupPartitionDoc) int { return int(a.Partition - b.Partition) })
			g.Topics = append(g.Topics, t)
		}
	}
	code := r.int16()
	if err := conn.decoded(kafkaOffsetFetch, r); err != nil {
		return err
	}
	if code != 0 && g.Error == "" {
		g.Error = fmt.Sprintf("fetching the offsets of group %q: %s", g.Group, kafkaError(code))
	}
	slices.SortFunc(g.Topics, func(a, b kafkaGroupTopicDoc) int { return strings.Compare(a.Topic, b.Topic) })
	return nil
}

// offsets asks each leader the newest and the oldest offsets of the chosen
// topics' partitions it leads, and works out the messages of each topic
// and the lag of each group.
func (k *kafkaCluster) offsets(doc *kafkaDocument, failed func(error) bool) error {
	byLeader := map[int32][]kafkaPartitionKey{}
	for _, t := range doc.Topics {
		for _, p := range t.Partitions {
			if !p.Offline {
				byLeader[p.Leader] = append(byLeader[p.Leader], kafkaPartitionKey{t.Name, p.Partition})
			}
		}
	}
	var mu sync.Mutex
	newest, oldest := map[kafkaPartitionKey]kafkaOffset{}, map[kafkaPartitionKey]kafkaOffset{}
	for id, err := range k.each(slices.Sorted(maps.Keys(byLeader)), func(id int32, conn *kafkaConn) error {
		for _, at := range []struct {
			timestamp int64
			into      map[kafkaPartitionKey]kafkaOffset
		}{{-1, newest}, {-2, oldest}} {
			got, err := listOffsets(conn, byLeader[id], at.timestamp)
			if err != nil {
				return err
			}
			mu.Lock()
			maps.Copy(at.into, got)
			mu.Unlock()
		}
		return nil
	}) {
		err = fmt.Errorf("asking broker %d for its partitions' offsets: %w", id, err)
		if failed(err) {
			return err
		}
		for _, key := range byLeader[id] {
			newest[key] = kafkaOffset{err: err.Error()}
		}
	}
	latest := map[kafkaPartitionKey]int64{}
	for i := range doc.Topics {
		t := &doc.Topics[i]
		var messages int64
		whole := true
		for j := range t.Partitions {
			p := &t.Partitions[j]
			key := kafkaPartitionKey{t.Name, p.Partition}
			n, o := newest[key], oldest[key]
			switch {
			case p.Offline:
			case n.err != "" || o.err != "":
				// What the metadata said of the partition comes first.
				if p.Error == "" {
					p.Error = cmp.Or(n.err, o.err)
				}
			default:
				p.Newest, p.Oldest = &n.offset, &o.offset
				latest[key] = n.offset
			}
			if p.Newest == nil || p.Oldest == nil {
				whole = false
				continue
			}
			messages += *p.Newest - *p.Oldest
		}
		if whole && len(t.Partitions) > 0 {
			t.Messages = &messages
		}
	}
	for i := range doc.Groups {
		for j := range doc.Groups[i].Topics {
			t := &doc.Groups[i].Topics[j]
			for m := range t.Partitions {
				p := &t.Partitions[m]
				end, known := latest[kafkaPartitionKey{t.Topic, p.Partition}]
				if p.Committed < 0 {
					continue
				}
				t.CommittedSum += p.Committed
				if !known {
					continue
				}
				lag := max(end-p.Committed, 0)
				p.Lag = &lag
				if t.Lag == nil {
					t.Lag = new(int64)
				}
				*t.Lag += lag
			}
		}
	}
	return nil
}

// kafkaOffset is a partition's offset as ListOffsets answered it, or its
// error.
type kafkaOffset struct {
	offset int64
	err    string
}

// listOffsets asks a leader the offsets of partitions at timestamp: -1 for
// the newest, -2 for the oldest.
func listOffsets(conn *kafkaConn, partitions []kafkaPartitionKey, timestamp int64) (map[kafkaPartitionKey]kafkaOffset, error) {
	byTopic := map[string][]int32{}
	for _, p := range partitions {
		byTopic[p.topic] = append(byTopic[p.topic], p.partition)
	}
	topics := slices.Sorted(maps.Keys(byTopic))
	w := kafkaWriter{}
	w.int32(-1) // replica_id: a client
	w.int8(0)   // isolation_level: read uncommitted, the high watermark
	w.array(len(topics))
	for _, topic := range topics {
		w.string(topic)
		w.array(len(byTopic[topic]))
		for _, p := range byTopic[topic] {
			w.int32(p)
			w.int32(-1) // current_leader_epoch: not checked
			w.int64(timestamp)
		}
	}
	r, err := conn.roundTrip(kafkaListOffsets, kafkaListOffsets.version, w.b)
	if err != nil {
		return nil, err
	}
	r.int32() // throttle_time_ms
	out := map[kafkaPartitionKey]kafkaOffset{}
	for n := r.array(6); n > 0; n-- {
		topic := r.string()
		for m := r.array(26); m > 0; m-- {
			key := kafkaPartitionKey{topic, r.int32()}
			code := r.int16()
			r.int64() // timestamp
			offset := r.int64()
			r.int32() // leader_epoch
			if code != 0 {
				out[key] = kafkaOffset{err: kafkaError(code)}
			} else {
				out[key] = kafkaOffset{offset: offset}
			}
		}
	}
	if err := conn.decoded(kafkaListOffsets, r); err != nil {
		return nil, err
	}
	for _, p := range partitions {
		if _, ok := out[p]; !ok {
			out[p] = kafkaOffset{err: "not in the leader's answer"}
		}
	}
	return out, nil
}
