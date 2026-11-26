//go:build request_type_kafka

package fetch

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// The SASL logins of a kafka scrape. After ApiVersions, a connection asks
// for its mechanism with SaslHandshake, then sends each of the
// mechanism's messages in a SaslAuthenticate and reads the broker's in its
// answer:
//
//   - PLAIN sends the user and the password in one message (RFC 4616);
//   - SCRAM-SHA-256 and SCRAM-SHA-512 prove the password without sending
//     it, and check that the broker knows it too (RFC 5802, RFC 7677);
//   - OAUTHBEARER sends a token (RFC 7628), read from bearer_token_file.

// The mechanisms, as request.sasl_mechanism and Kafka write them.
const (
	kafkaPlain       = "PLAIN"
	kafkaScram256    = "SCRAM-SHA-256"
	kafkaScram512    = "SCRAM-SHA-512"
	kafkaOAuthBearer = "OAUTHBEARER"
)

// kafkaMechanisms are the mechanisms a collector may name.
var kafkaMechanisms = []string{kafkaPlain, kafkaScram256, kafkaScram512, kafkaOAuthBearer}

// kafkaScramMaxIterations bounds the iterations a broker may ask a SCRAM
// login to hash the password with: Kafka refuses more than 16,384, and a
// broker that asked for millions would hold the scrape for seconds.
const kafkaScramMaxIterations = 100_000

// kafkaLogin is what a connection logs in with: a mechanism and its
// credential, or no mechanism for a cluster without a login.
type kafkaLogin struct {
	mechanism string
	username  string
	password  string
	token     string
}

// login runs the SASL exchange of l on c.
func (c *kafkaConn) login(l kafkaLogin, authenticateVersion int16) error {
	w := kafkaWriter{}
	w.string(l.mechanism)
	r, err := c.roundTrip(kafkaSaslHandshake, kafkaSaslHandshake.version, w.b)
	if err != nil {
		return err
	}
	code := r.int16()
	n := r.array(2)
	enabled := make([]string, 0, n)
	for range n {
		enabled = append(enabled, r.string())
	}
	if err := c.decoded(kafkaSaslHandshake, r); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s does not take the SASL mechanism %s (%s); it takes %s: set request.sasl_mechanism to one of them", c.broker, l.mechanism, kafkaError(code), brokerSaid(strings.Join(enabled, ", ")))
	}
	authenticate := func(message []byte) ([]byte, error) {
		w := kafkaWriter{}
		w.bytes(message)
		r, err := c.roundTrip(kafkaSaslAuthenticate, authenticateVersion, w.b)
		if err != nil {
			return nil, err
		}
		code := r.int16()
		said, _ := r.nullableString()
		answer := r.bytes()
		if authenticateVersion >= 1 {
			r.int64() // session_lifetime_ms
		}
		if err := c.decoded(kafkaSaslAuthenticate, r); err != nil {
			return nil, err
		}
		if code != 0 {
			if said == "" {
				said = kafkaError(code)
			}
			return nil, fmt.Errorf("%s refused the %s login of %s: %s", c.broker, l.mechanism, kafkaLoginName(l), brokerSaid(said))
		}
		return answer, nil
	}
	switch l.mechanism {
	case kafkaPlain:
		_, err := authenticate([]byte("\x00" + l.username + "\x00" + l.password))
		return err
	case kafkaOAuthBearer:
		_, err := authenticate([]byte("n,,\x01auth=Bearer " + l.token + "\x01\x01"))
		return err
	}
	return c.scram(l, authenticate)
}

// kafkaLoginName names the account of a login in a refusal: its user, or
// the token's file.
func kafkaLoginName(l kafkaLogin) string {
	if l.mechanism == kafkaOAuthBearer {
		return "the token"
	}
	return strconv.Quote(l.username)
}

// scram runs a SCRAM exchange: the client's first message with a nonce,
// the broker's with its nonce, the salt and the iterations, the client's
// proof, and the broker's signature, which shows that it knows the
// password and is checked.
func (c *kafkaConn) scram(l kafkaLogin, authenticate func([]byte) ([]byte, error)) error {
	newHash, size := sha256.New, sha256.Size
	if l.mechanism == kafkaScram512 {
		newHash, size = sha512.New, sha512.Size
	}
	clientNonce := kafkaScramNonce()
	firstBare := "n=" + kafkaScramName(l.username) + ",r=" + clientNonce
	serverFirst, err := authenticate([]byte("n,," + firstBare))
	if err != nil {
		return err
	}
	fields := kafkaScramFields(string(serverFirst))
	serverNonce, salt64, rounds := fields["r"], fields["s"], fields["i"]
	salt, saltErr := base64.StdEncoding.DecodeString(salt64)
	iterations, roundsErr := strconv.Atoi(rounds)
	switch {
	case fields["e"] != "":
		return fmt.Errorf("%s refused the %s login of %q: %s", c.broker, l.mechanism, l.username, brokerSaid(fields["e"]))
	case !strings.HasPrefix(serverNonce, clientNonce) || len(serverNonce) == len(clientNonce):
		return fmt.Errorf("%s answered the %s login with a nonce that does not extend the client's, which SCRAM requires", c.broker, l.mechanism)
	case saltErr != nil || len(salt) == 0 || roundsErr != nil || iterations < 1:
		return fmt.Errorf("%s answered the %s login without a salt and a count of iterations that read, which is %w", c.broker, l.mechanism, errKafkaProtocol)
	case iterations > kafkaScramMaxIterations:
		return fmt.Errorf("%s asked the %s login to hash the password %d times, more than the %d a scrape does", c.broker, l.mechanism, iterations, kafkaScramMaxIterations)
	}
	salted, err := pbkdf2.Key(newHash, l.password, salt, iterations, size)
	if err != nil {
		return fmt.Errorf("the %s login: %w", l.mechanism, err)
	}
	mac := func(key []byte, text string) []byte {
		h := hmac.New(newHash, key)
		h.Write([]byte(text))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := newHash()
	stored.Write(clientKey)
	withoutProof := "c=biws,r=" + serverNonce
	authMessage := firstBare + "," + string(serverFirst) + "," + withoutProof
	proof := mac(stored.Sum(nil), authMessage)
	for i := range proof {
		proof[i] ^= clientKey[i]
	}
	serverFinal, err := authenticate([]byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)))
	if err != nil {
		return err
	}
	final := kafkaScramFields(string(serverFinal))
	if final["e"] != "" {
		return fmt.Errorf("%s refused the %s login of %q: %s", c.broker, l.mechanism, l.username, brokerSaid(final["e"]))
	}
	signature, err := base64.StdEncoding.DecodeString(final["v"])
	if err != nil || subtle.ConstantTimeCompare(signature, mac(mac(salted, "Server Key"), authMessage)) != 1 {
		return fmt.Errorf("%s answered the %s login with a signature that does not match the password: it does not know the account's password, and may not be the broker it says", c.broker, l.mechanism)
	}
	return nil
}

// kafkaScramNonce is a SCRAM login's nonce: 24 random bytes in base64. A
// test sets its own, to check a login against RFC 7677's example.
var kafkaScramNonce = func() string {
	nonce := make([]byte, 24)
	_, _ = rand.Read(nonce)
	return base64.RawStdEncoding.EncodeToString(nonce)
}

// kafkaScramName is a user's name as SCRAM writes it: = and , escaped.
func kafkaScramName(name string) string {
	return strings.NewReplacer("=", "=3D", ",", "=2C").Replace(name)
}

// kafkaScramFields reads a SCRAM message's attributes, k=value separated
// by commas.
func kafkaScramFields(message string) map[string]string {
	fields := map[string]string{}
	for part := range strings.SplitSeq(message, ",") {
		if key, value, found := strings.Cut(part, "="); found && len(key) == 1 {
			if _, seen := fields[key]; !seen {
				fields[key] = value
			}
		}
	}
	return fields
}
