//go:build request_type_kafka

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/kafkatest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// kafkaCluster3 is a cluster of three brokers: a topic, orders, whose
// partition 1 is under-replicated and not led by its preferred replica and
// whose partition 2 is offline; a topic, payments; Kafka's own
// __consumer_offsets; and three groups, billing and audit on brokers 2 and
// 3, and locked, on broker 1, which the account may not describe.
func kafkaCluster3(t *testing.T, secure bool) *kafkatest.Cluster {
	t.Helper()
	k := kafkatest.New(t, 3, secure)
	rack := "eu-1a"
	k.Edit(func(c *kafkatest.Cluster) {
		c.Brokers[0].Rack = &rack
		c.Topics = []kafkatest.Topic{
			{Name: "orders", Partitions: []kafkatest.Partition{
				{Leader: 1, Replicas: []int32{1, 2, 3}, ISR: []int32{1, 2, 3}, Newest: 100, Oldest: 10},
				{Leader: 3, Replicas: []int32{2, 3, 1}, ISR: []int32{3, 1}, Newest: 50},
				{Leader: -1, Replicas: []int32{3, 1, 2}, Offline: []int32{3, 1, 2}},
			}},
			{Name: "payments", Partitions: []kafkatest.Partition{{Leader: 2, Replicas: []int32{2, 3, 1}, ISR: []int32{2, 3, 1}, Newest: 7, Oldest: 7}}},
			{Name: "__consumer_offsets", Internal: true, Partitions: []kafkatest.Partition{{Leader: 1, Replicas: []int32{1}, ISR: []int32{1}, Newest: 9}}},
		}
		c.Groups = []kafkatest.Group{
			{Name: "billing", Protocol: "consumer", State: "Stable", Members: 2, Coordinator: 2,
				Offsets: map[string]map[int32]int64{"orders": {0: 90, 1: 55, 2: -1}, "__consumer_offsets": {0: 1}}},
			{Name: "audit", Protocol: "consumer", State: "Empty", Coordinator: 3, Offsets: map[string]map[int32]int64{"payments": {0: 3}}},
			{Name: "locked", Protocol: "consumer", Coordinator: 1, DescribeError: kafkatest.GroupAuthorization, Offsets: map[string]map[int32]int64{"orders": {0: 100}}},
		}
	})
	return k
}

// kafkaCollector is a validated kafka collector, changed by edit first.
func kafkaCollector(t *testing.T, edit func(*model.Collector, *KafkaConfig)) *model.Collector {
	t.Helper()
	c, own := &model.Collector{Name: "kafka", Request: model.RequestConfig{Type: RequestTypeKafka}}, KafkaConfig{}
	if edit != nil {
		edit(c, &own)
	}
	c.Request.Own = own
	if err := ValidateRequest(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func kafkaFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func kafkaDoc(t *testing.T, c *model.Collector, target string) (kafkaDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := FetchCollector(ctx, target, c, RequestOverrides{}, nil)
	if err != nil {
		return kafkaDocument{}, err
	}
	var doc kafkaDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("%v in %s", err, resp.Body)
	}
	return doc, nil
}

// kafkaJSON is v as JSON, for a comparison.
func kafkaJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A scrape reads the cluster: its brokers, with the partitions each leads
// and holds a replica of; its topics but Kafka's own, each with its
// partitions' leaders, replicas and offsets, a broker's replicas counted
// over every topic, counted as offline,
// under-replicated and not led by their preferred replica, and the messages
// they hold where every partition's offsets are known; and its groups, with
// their state, members and lag by topic and partition, a committed offset
// past the newest read as no lag, a partition without one as no lag, and a
// topic not chosen left out. Each group is asked of the broker that lists
// it, its coordinator, and none of FindCoordinator; one it refuses to
// describe says so, in its entry and among the errors, and still has its
// lag. The request is traced and labelled by the broker, and the
// committed offsets are asked before the newest, so that a lag is not
// negative.
func TestAKafkaScrapeReadsTheCluster(t *testing.T) {
	k := kafkaCluster3(t, false)
	ctx, trace := WithRequestTrace(context.Background())
	c := kafkaCollector(t, nil)
	resp, err := FetchCollector(ctx, k.Brokers[0].Addr, c, RequestOverrides{Timeout: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc kafkaDocument
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if *doc.ClusterID != "stand-in-cluster" || doc.Controller != 1 || doc.TLS || doc.Address != k.Brokers[0].Addr || len(doc.Errors) != 1 || doc.Errors[0] != `describing group "locked": GROUP_AUTHORIZATION_FAILED` {
		t.Errorf("document %s", resp.Body)
	}
	brokers := []string{}
	for _, b := range doc.Brokers {
		brokers = append(brokers, fmt.Sprintf("%d %s:%d controller=%v leads=%d replicas=%d rack=%v", b.ID, b.Host, b.Port, b.Controller, b.LeaderOf, b.Replicas, b.Rack != nil))
	}
	_, port1, _ := net.SplitHostPort(k.Brokers[0].Addr)
	if !strings.HasPrefix(brokers[0], "1 127.0.0.1:"+port1+" controller=true leads=2 replicas=5 rack=true") || !strings.Contains(brokers[1], "controller=false leads=1 replicas=4 rack=false") || !strings.Contains(brokers[2], "leads=1 replicas=4") {
		t.Errorf("brokers %q", brokers)
	}
	if len(doc.Topics) != 2 || doc.Topics[0].Name != "orders" || doc.Topics[1].Name != "payments" {
		t.Fatalf("topics %s", kafkaJSON(t, doc.Topics))
	}
	orders := doc.Topics[0]
	want := `{"name":"orders","internal":false,"error":"","partition_count":3,"replication_factor":3,"offline":1,"under_replicated":2,"not_preferred_leader":1,"messages":null,"disk_bytes":null,"configs":null,"partitions":[` +
		`{"partition":0,"leader":1,"replicas":[1,2,3],"isr":[1,2,3],"offline_replicas":[],"preferred_leader":true,"under_replicated":false,"offline":false,"newest":100,"oldest":10,"size_bytes":null,"error":""},` +
		`{"partition":1,"leader":3,"replicas":[2,3,1],"isr":[3,1],"offline_replicas":[],"preferred_leader":false,"under_replicated":true,"offline":false,"newest":50,"oldest":0,"size_bytes":null,"error":""},` +
		`{"partition":2,"leader":-1,"replicas":[3,1,2],"isr":[],"offline_replicas":[3,1,2],"preferred_leader":false,"under_replicated":true,"offline":true,"newest":null,"oldest":null,"size_bytes":null,"error":""}]}`
	if got := kafkaJSON(t, orders); got != want {
		t.Errorf("orders\n%s\nwant\n%s", got, want)
	}
	if p := doc.Topics[1]; p.Messages == nil || *p.Messages != 0 || p.Offline != 0 || p.UnderReplicated != 0 {
		t.Errorf("payments %s", kafkaJSON(t, p))
	}
	want = `[{"group":"audit","protocol_type":"consumer","state":"Empty","members":0,"coordinator":3,"error":"","topics":[{"group":"audit","topic":"payments","committed_sum":3,"lag":4,"lag_seconds":null,"partitions":[{"partition":0,"committed":3,"lag":4,"lag_seconds":null,"error":""}]}]},` +
		`{"group":"billing","protocol_type":"consumer","state":"Stable","members":2,"coordinator":2,"error":"","topics":[{"group":"billing","topic":"orders","committed_sum":145,"lag":10,"lag_seconds":null,"partitions":[{"partition":0,"committed":90,"lag":10,"lag_seconds":null,"error":""},{"partition":1,"committed":55,"lag":0,"lag_seconds":0,"error":""},{"partition":2,"committed":-1,"lag":null,"lag_seconds":null,"error":""}]}]},` +
		`{"group":"locked","protocol_type":"consumer","state":null,"members":null,"coordinator":1,"error":"describing group \"locked\": GROUP_AUTHORIZATION_FAILED","topics":[{"group":"locked","topic":"orders","committed_sum":100,"lag":0,"lag_seconds":0,"partitions":[{"partition":0,"committed":100,"lag":0,"lag_seconds":0,"error":""}]}]}]`
	if got := kafkaJSON(t, doc.Groups); got != want {
		t.Errorf("groups\n%s\nwant\n%s", got, want)
	}
	requests := trace.Requests()
	if len(requests) != 1 || requests[0].Method != "METADATA" || requests[0].URL != "kafka://"+k.Brokers[0].Addr || requests[0].Outcome != "3 brokers, 2 topics, 3 groups" {
		t.Errorf("traced %+v", requests)
	}
	if method, stage := RequestMethodFor(c, RequestOverrides{}), FetchStage(c); method != "METADATA" || stage != "kafka" {
		t.Errorf("method %q, stage %q", method, stage)
	}
	asked := k.Asked()
	for _, a := range asked {
		if a.API == 10 {
			t.Errorf("FindCoordinator was sent to broker %d", a.Broker)
		}
	}
	lastFetch, firstList := -1, len(asked)
	listedBy := map[int32]bool{}
	for i, a := range asked {
		switch a.API {
		case kafkatest.OffsetFetch:
			lastFetch = i
		case kafkatest.ListOffsets:
			firstList = min(firstList, i)
			if a.Broker != 1 && a.Broker != 2 && a.Broker != 3 {
				t.Errorf("ListOffsets to broker %d", a.Broker)
			}
		case kafkatest.ListGroups:
			listedBy[a.Broker] = true
		}
	}
	if lastFetch < 0 || lastFetch > firstList || len(listedBy) != 3 {
		t.Errorf("asked %+v", asked)
	}
}

// The topics and groups reported are those request.include_topics and
// request.include_groups match whole and request.exclude_topics and
// request.exclude_groups do not; Kafka's own topics come with
// internal_topics, and a group's lag is of the chosen topics alone.
func TestAKafkaScrapeReportsTheChosenTopicsAndGroups(t *testing.T) {
	k := kafkaCluster3(t, false)
	for _, check := range []struct {
		edit           func(*KafkaConfig)
		topics, groups string
	}{
		{func(r *KafkaConfig) { r.IncludeTopics = []string{"ord"} }, "", "audit billing locked"},
		{func(r *KafkaConfig) { r.IncludeTopics = []string{"pay", "ord.*"} }, "orders", "audit billing locked"},
		{func(r *KafkaConfig) { r.ExcludeTopics = []string{"pay.*"} }, "orders", "audit billing locked"},
		{func(r *KafkaConfig) { r.InternalTopics = true }, "__consumer_offsets orders payments", "audit billing locked"},
		{func(r *KafkaConfig) { r.IncludeGroups, r.ExcludeGroups = []string{"a.*", "b.*"}, []string{"audit"} }, "orders payments", "billing"},
	} {
		doc, err := kafkaDoc(t, kafkaCollector(t, func(_ *model.Collector, r *KafkaConfig) { check.edit(r) }), k.Brokers[1].Addr)
		if err != nil {
			t.Fatal(err)
		}
		var topics, groups []string
		for _, topic := range doc.Topics {
			topics = append(topics, topic.Name)
		}
		for _, g := range doc.Groups {
			groups = append(groups, g.Group)
			for _, gt := range g.Topics {
				if !slices.Contains(topics, gt.Topic) {
					t.Errorf("group %s has lag of %s, which is not chosen", g.Group, gt.Topic)
				}
			}
		}
		if strings.Join(topics, " ") != check.topics || strings.Join(groups, " ") != check.groups {
			t.Errorf("topics %q, groups %q; want %q, %q", topics, groups, check.topics, check.groups)
		}
	}
}

// Every connection logs in with the collector's account, by PLAIN, SCRAM
// with SHA-256 or SHA-512, or OAUTHBEARER with a token. A login the broker
// refuses fails the scrape with what it said and without the password; a
// mechanism it does not take names those it does; a SCRAM broker that
// cannot prove it knows the password fails it, and one that asks for too
// many iterations is not obeyed. A token it does not take is refused, though
// Kafka answers it with an error challenge rather than an error code. A
// broker that takes a login refuses every request before one, and a
// listener without SASL says it takes no login.
func TestAKafkaScrapeLogsIn(t *testing.T) {
	k := kafkaCluster3(t, false)
	k.Edit(func(c *kafkatest.Cluster) {
		c.Mechanisms = []string{"PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512", "OAUTHBEARER"}
		c.Users, c.Token = map[string]string{"monitor": "s3cret-pw", "a,b=c": "other-pw"}, "s3cret-token"
	})
	account := func(mechanism, user, password string) *model.Collector {
		return kafkaCollector(t, func(c *model.Collector, r *KafkaConfig) {
			r.SASLMechanism, c.Request.Username, c.Request.PasswordFile = mechanism, user, kafkaFile(t, password)
		})
	}
	for _, c := range []*model.Collector{
		account("", "monitor", "s3cret-pw"),
		account("SCRAM-SHA-256", "monitor", "s3cret-pw"),
		account("SCRAM-SHA-512", "a,b=c", "other-pw"),
		kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.BearerTokenFile = kafkaFile(t, "s3cret-token") }),
	} {
		doc, err := kafkaDoc(t, c, k.Brokers[0].Addr)
		if err != nil || len(doc.Groups) != 3 || len(doc.Errors) != 1 {
			t.Errorf("%s: %v, %+v", kafkaMechanism(c), err, doc.Errors)
		}
	}
	for mechanism, want := range map[string]string{
		"PLAIN":         `refused the PLAIN login of "monitor": Authentication failed: Invalid username or password`,
		"SCRAM-SHA-512": `refused the SCRAM-SHA-512 login of "monitor": Authentication failed: Invalid username or password`,
	} {
		if _, err := kafkaDoc(t, account(mechanism, "monitor", "wr0ng-pw"), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "wr0ng-pw") {
			t.Errorf("%s with a wrong password: %v", mechanism, err)
		}
	}
	// A token the broker does not take is answered with an error challenge
	// and no error code, as Kafka does, which is a refusal all the same.
	wrongToken := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.BearerTokenFile = kafkaFile(t, "wr0ng-token") })
	if _, err := kafkaDoc(t, wrongToken, k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), `refused the OAUTHBEARER login of the token: {"status":"invalid_token"}`) || strings.Contains(err.Error(), "wr0ng-token") {
		t.Errorf("a token not taken: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) { c.Mechanisms = []string{"SCRAM-SHA-512"} })
	if _, err := kafkaDoc(t, account("PLAIN", "monitor", "s3cret-pw"), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "does not take the SASL mechanism PLAIN (UNSUPPORTED_SASL_MECHANISM); it takes SCRAM-SHA-512") {
		t.Errorf("a mechanism the broker does not take: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) { c.BadSignature = true })
	if _, err := kafkaDoc(t, account("SCRAM-SHA-512", "monitor", "s3cret-pw"), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "a signature that does not match the password") {
		t.Errorf("a broker that does not know the password: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) { c.BadSignature, c.Iterations = false, 1_000_000 })
	if _, err := kafkaDoc(t, account("SCRAM-SHA-512", "monitor", "s3cret-pw"), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "to hash the password 1000000 times, more than the 100000") {
		t.Errorf("too many iterations: %v", err)
	}
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "closed the connection while reading the answer to Metadata") {
		t.Errorf("no login where one is needed: %v", err)
	}
	// A login sent to a listener without SASL says so.
	plain := kafkaCluster3(t, false)
	if _, err := kafkaDoc(t, account("PLAIN", "monitor", "s3cret-pw"), plain.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "takes no SASL login on this listener (ILLEGAL_SASL_STATE)") {
		t.Errorf("a login where none is taken: %v", err)
	}
}

// A SCRAM-SHA-256 login sends what RFC 7677's example sends, given its
// nonce, and checks the server's signature in the example's last message.
func TestAKafkaScramLoginIsRFC7677s(t *testing.T) {
	nonce := kafkaScramNonce
	kafkaScramNonce = func() string { return "rOprNGfwEbeRWgbNEkqO" }
	t.Cleanup(func() { kafkaScramNonce = nonce })
	const serverFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	var sent []string
	conn := &kafkaConn{broker: "b1:9092"}
	err := conn.scram(kafkaLogin{mechanism: kafkaScram256, username: "user", password: "pencil"}, func(message []byte) ([]byte, error) {
		sent = append(sent, string(message))
		if len(sent) == 1 {
			return []byte(serverFirst), nil
		}
		return []byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="), nil
	})
	want := []string{"n,,n=user,r=rOprNGfwEbeRWgbNEkqO", "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="}
	if err != nil || !slices.Equal(sent, want) {
		t.Errorf("sent %q, %v; want %q", sent, err, want)
	}
}

// A broker that cannot be reached once the metadata is read, or that the
// collector's lists refuse by its name, does not fail the scrape: what it
// would have said is missing, the groups it alone lists among it, and
// errors says why. So does one whose address no connection can use. The target refused, or
// unreachable, fails it.
func TestAKafkaScrapeGoesOnWithoutABroker(t *testing.T) {
	k := kafkaCluster3(t, false)
	k.Stop(3)
	doc, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr)
	if err != nil {
		t.Fatal(err)
	}
	errs := strings.Join(doc.Errors, "\n")
	if len(doc.Errors) != 3 || !strings.Contains(errs, "listing the groups of broker 3: connecting to "+k.Brokers[2].Addr) ||
		!strings.Contains(errs, "asking broker 3 for its partitions' offsets: connecting to "+k.Brokers[2].Addr) || !strings.Contains(errs, `group "locked"`) {
		t.Errorf("errors %q", doc.Errors)
	}
	if p := doc.Topics[0].Partitions[1]; p.Newest != nil || !strings.Contains(p.Error, "broker 3") {
		t.Errorf("a partition of broker 3: %s", kafkaJSON(t, p))
	}
	if p := doc.Topics[0].Partitions[0]; p.Newest == nil || *p.Newest != 100 {
		t.Errorf("a partition of broker 1: %s", kafkaJSON(t, p))
	}
	// Broker 3 alone lists audit, the group it coordinates.
	if len(doc.Groups) != 2 || doc.Groups[0].Group != "billing" || doc.Groups[0].Topics[0].Lag == nil {
		t.Errorf("groups %s", kafkaJSON(t, doc.Groups))
	}

	// A partition whose metadata has an error keeps it, though its offsets
	// are missing for another reason too.
	early := kafkaCluster3(t, false)
	early.Edit(func(c *kafkatest.Cluster) { c.Topics[0].Partitions[1].Error = 5 })
	early.Stop(3)
	doc, err = kafkaDoc(t, kafkaCollector(t, nil), early.Brokers[0].Addr)
	if err != nil || doc.Topics[0].Partitions[1].Error != "LEADER_NOT_AVAILABLE" {
		t.Errorf("a partition the metadata says has no leader available: %v, %s", err, kafkaJSON(t, doc.Topics[0].Partitions[1]))
	}

	// A broker whose address no connection can use is one of the errors
	// too.
	odd := kafkaCluster3(t, false)
	odd.Edit(func(c *kafkatest.Cluster) { c.Brokers[2].Host = "kafka 3" })
	doc, err = kafkaDoc(t, kafkaCollector(t, nil), odd.Brokers[0].Addr)
	if err != nil || !strings.Contains(strings.Join(doc.Errors, "\n"), `listing the groups of broker 3: broker 3's address kafka 3:`) {
		t.Errorf("a broker of an address no connection can use: %v, %q", err, doc.Errors)
	}

	named := kafkaCluster3(t, false)
	named.Edit(func(c *kafkatest.Cluster) { c.Brokers[1].Host = "localhost" })
	refusing := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.DeniedTargets = []string{"localhost"} })
	doc, err = kafkaDoc(t, refusing, named.Brokers[0].Addr)
	if err != nil || !strings.Contains(strings.Join(doc.Errors, "\n"), "listing the groups of broker 2: ") {
		t.Errorf("a broker the lists refuse: %v, %q", err, doc.Errors)
	}
	for _, a := range named.Asked() {
		if a.Broker == 2 {
			t.Fatalf("broker 2, refused by name, was asked %d", a.API)
		}
	}
	denied := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.DeniedTargets = []string{"127.0.0.0/8"} })
	if _, err := kafkaDoc(t, denied, k.Brokers[0].Addr); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("a refused target: %v", err)
	}
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[2].Addr); err == nil || !strings.Contains(err.Error(), "connecting to "+k.Brokers[2].Addr) {
		t.Errorf("an unreachable target: %v", err)
	}
}

// A broker older than Kafka 2.1, which does not speak the versions the
// type sends, fails the scrape saying so; so does what is not Kafka, a
// TLS listener reached in plaintext with a hint, an answer that does not
// decode, and a broker that says nothing, at the deadline.
func TestAKafkaScrapeRefusesWhatIsNotKafka21(t *testing.T) {
	k := kafkaCluster3(t, false)
	k.Edit(func(c *kafkatest.Cluster) { c.Versions = map[int16][2]int16{kafkatest.Metadata: {0, 5}} })
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "speaks Metadata v0 to v5, not v7, which the kafka type sends; it needs Kafka 2.1 or later") {
		t.Errorf("an old broker: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) {
		c.Versions = nil
		c.Answer = func(_ int32, api int16) []byte {
			if api == kafkatest.Metadata {
				return []byte{0, 0, 0, 0, 0, 0, 0, 99}
			}
			return nil
		}
	})
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "answered Metadata with what does not decode") || !errors.Is(err, errKafkaProtocol) {
		t.Errorf("an answer that does not decode: %v", err)
	}
	web := testutil.NewTCPStub(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
	})
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), web.Addr); err == nil || !strings.Contains(err.Error(), "answered ApiVersions with a frame of 1213486160 bytes") || !errors.Is(err, errKafkaProtocol) {
		t.Errorf("what is not Kafka: %v", err)
	}
	closing := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = conn.Read(make([]byte, 16)) })
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), closing.Addr); err == nil || !strings.Contains(err.Error(), "a target of one is written tls://host:port") {
		t.Errorf("a listener that closes the connection: %v", err)
	}
	mute := testutil.NewTCPStub(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := FetchCollector(ctx, mute.Addr, kafkaCollector(t, nil), RequestOverrides{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a broker that says nothing: %v", err)
	}
}

// A target written tls:// is spoken to over TLS, and so is every broker,
// each certificate verified by the tls block; a target not verified fails
// the scrape.
func TestAKafkaScrapeSpeaksTLS(t *testing.T) {
	k := kafkaCluster3(t, true)
	c := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.TLS.CAFile = k.Brokers[0].CAFile })
	doc, err := kafkaDoc(t, c, "tls://"+k.Brokers[0].Addr)
	if err != nil || !doc.TLS || len(doc.Brokers) != 3 || len(doc.Errors) != 1 || len(doc.Groups) != 3 {
		t.Fatalf("over TLS: %+v, %v", doc, err)
	}
	asked := map[int32]bool{}
	for _, a := range k.Asked() {
		asked[a.Broker] = true
	}
	if len(asked) != 3 {
		t.Errorf("brokers asked over TLS: %v", asked)
	}
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), "tls://"+k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "TLS handshake with "+k.Brokers[0].Addr) {
		t.Errorf("an unverified broker: %v", err)
	}
}

// Answers past the response limit fail the scrape as the limit, and so do
// more partitions or groups than a scrape reports.
func TestAKafkaScrapeIsBounded(t *testing.T) {
	k := kafkaCluster3(t, false)
	small := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) { c.Request.MaxResponseBytes = 64 })
	if _, err := kafkaDoc(t, small, k.Brokers[0].Addr); !errors.Is(err, model.ErrLimitExceeded) {
		t.Errorf("answers past the limit: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) {
		c.Topics = append(c.Topics, kafkatest.Topic{Name: "huge", Partitions: make([]kafkatest.Partition, kafkaMaxPartitions)})
	})
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "the chosen topics have 10004 partitions, more than the 10000") {
		t.Errorf("too many partitions: %v", err)
	}
	k.Edit(func(c *kafkatest.Cluster) {
		c.Topics = c.Topics[:2]
		for i := range kafkaMaxGroups {
			c.Groups = append(c.Groups, kafkatest.Group{Name: fmt.Sprintf("g%04d", i), Coordinator: 1})
		}
	})
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), k.Brokers[0].Addr); err == nil || !strings.Contains(err.Error(), "1003 consumer groups are chosen, more than the 1000") {
		t.Errorf("too many groups: %v", err)
	}
}

// A target is a broker, with kafka:// or tls:// or neither, the cloud
// metadata service refused, and the keys are checked when the
// configuration loads.
func TestAKafkaTargetAndKeysAreChecked(t *testing.T) {
	c := kafkaCollector(t, nil)
	for _, target := range []string{"", "http://kafka.example.com", "kafka.example.com/path", "user@kafka.example.com", "kafka.example.com:0"} {
		if err := CheckTarget(c, target, false); err == nil {
			t.Errorf("target %q was taken", target)
		}
	}
	for target, label := range map[string]string{"Kafka.example.com": "kafka://kafka.example.com:9092", "kafka://10.0.0.1:9094": "kafka://10.0.0.1:9094", "tls://b1.example.com:9093": "tls://b1.example.com:9093"} {
		if got, err := RequestLabelFor(target, c, RequestOverrides{}); err != nil || got != label {
			t.Errorf("target %q: label %q, %v; want %q", target, got, err, label)
		}
	}
	for want, edit := range map[string]func(*model.Collector, *KafkaConfig){
		`request.sasl_mechanism "GSSAPI" is none of PLAIN`: func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism = "GSSAPI" },
		"sets request.bearer_token_file beside request.user": func(c *model.Collector, _ *KafkaConfig) {
			c.Request.BearerTokenFile, c.Request.Username, c.Request.PasswordFile = "/t", "u", "/p"
		},
		"is OAUTHBEARER, which sends a token":                                func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism = kafkaOAuthBearer },
		"is SCRAM-SHA-256, which sends a user and a password":                func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism = kafkaScram256 },
		`request.include_topics[1] "(" is not a regular expression`:          func(_ *model.Collector, r *KafkaConfig) { r.IncludeTopics = []string{"a", "("} },
		"request.exclude_groups[0] is longer than 1000":                      func(_ *model.Collector, r *KafkaConfig) { r.ExcludeGroups = []string{strings.Repeat("a", 1001)} },
		"request.exclude_topics[0] is empty":                                 func(_ *model.Collector, r *KafkaConfig) { r.ExcludeTopics = []string{""} },
		"request.include_groups has more than 100 patterns":                  func(_ *model.Collector, r *KafkaConfig) { r.IncludeGroups = slices.Repeat([]string{"a"}, 101) },
		"sets only one of request.username and password_file":                func(c *model.Collector, _ *KafkaConfig) { c.Request.Username = "monitor" },
		"request.bearer_token_file has a placeholder":                        func(c *model.Collector, _ *KafkaConfig) { c.Request.BearerTokenFile = "/{{param_x}}" },
		"sets request.aws_region without request.sasl_mechanism AWS_MSK_IAM": func(_ *model.Collector, r *KafkaConfig) { r.AWSRegion = "eu-west-1" },
		`request.aws_region is "EU West"`:                                    func(_ *model.Collector, r *KafkaConfig) { r.SASLMechanism, r.AWSRegion = kafkaAWSMSKIAM, "EU West" },
		`request.topic_configs[1] is "Retention MS"`: func(_ *model.Collector, r *KafkaConfig) {
			r.TopicConfigs = []string{"retention.ms", "Retention MS"}
		},
		`request.topic_configs names "retention.ms" twice`: func(_ *model.Collector, r *KafkaConfig) {
			r.TopicConfigs = []string{"retention.ms", "retention.ms"}
		},
		"request.topic_configs names more than 100 settings": func(_ *model.Collector, r *KafkaConfig) {
			for i := range 101 {
				r.TopicConfigs = append(r.TopicConfigs, fmt.Sprint("s", i))
			}
		},
	} {
		own := KafkaConfig{}
		c := &model.Collector{Name: "kafka", Request: model.RequestConfig{Type: RequestTypeKafka}}
		edit(c, &own)
		c.Request.Own = own
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want %q", err, want)
		}
	}
	// The cloud metadata service is refused with neither list set.
	if _, err := kafkaDoc(t, kafkaCollector(t, nil), "169.254.169.254:9092"); !errors.Is(err, ErrTargetRefused) {
		t.Errorf("the metadata service: %v", err)
	}
	missing := kafkaCollector(t, func(c *model.Collector, _ *KafkaConfig) {
		c.Request.BearerTokenFile = filepath.Join(t.TempDir(), "none")
	})
	if warnings := FileWarnings(missing, nil); len(warnings) != 1 || !strings.Contains(warnings[0], "request.bearer_token_file") {
		t.Errorf("warnings %v", warnings)
	}
}

// The keys the opt-in kafka type takes that no type of a default build does
// are listed (optInRequestKeys, optInTargetKeys).
func TestTheKafkaTypesOptInKeysAreListed(t *testing.T) {
	checkOptInRequestKeys(t, RequestTypeKafka)
}
