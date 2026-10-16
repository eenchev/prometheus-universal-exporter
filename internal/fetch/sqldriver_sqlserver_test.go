//go:build !select_request_types || request_type_sql

package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// The sqlserver login-only encryption, encrypt false or left out, does not
// verify the server's certificate when no tls key is set, as SQL Server's
// own tools do not: its default certificate is one it made itself. With a
// tls key, or with encrypt true, the certificate is verified as the keys
// say.
func TestSQLServerLoginOnlyEncryptionVerifiesTheCertificateOnlyAsTLSKeysSay(t *testing.T) {
	d := sqlDriverTable[SQLDriverSQLServer]
	for _, tc := range []struct {
		options  map[string]string
		tls      model.TLSConfig
		insecure bool
	}{
		{nil, model.TLSConfig{}, true},
		{map[string]string{"encrypt": "false"}, model.TLSConfig{}, true},
		{map[string]string{"encrypt": "false"}, model.TLSConfig{ServerName: "db.internal"}, false},
		{map[string]string{"encrypt": "false"}, model.TLSConfig{InsecureSkipVerify: true}, true},
		{map[string]string{"encrypt": "true"}, model.TLSConfig{}, false},
		{map[string]string{"encrypt": "strict"}, model.TLSConfig{}, false},
		{nil, model.TLSConfig{ServerName: "db.internal"}, false},
	} {
		config, _, err := sqlTLSConfig(d, &sqlBase{tls: tc.tls}, tc.options, "db.example")
		if err != nil {
			t.Fatal(err)
		}
		spec := sqlSpec(t, "1433", config, tc.options)
		msconfig, err := sqlServerConfig(spec)
		if err != nil {
			t.Fatal(err)
		}
		if msconfig.TLSConfig == nil || msconfig.TLSConfig.InsecureSkipVerify != tc.insecure {
			t.Errorf("%v %+v: %+v, want the certificate verified %t", tc.options, tc.tls, msconfig.TLSConfig, !tc.insecure)
		}
	}
}

// A sqlserver connection whose login is to be encrypted is refused, with a
// sentence saying what to change, when the server answers that it does not
// support encryption: the server never receives the login. With encrypt
// disable the login is sent unencrypted, as asked; and a server that
// answers it encrypts gets the TLS handshake, the answer reaching the driver
// unchanged.
func TestSQLServerNeverSendsTheLoginUnencryptedUnlessToldTo(t *testing.T) {
	for _, tc := range []struct {
		options map[string]string
		answer  byte
		refused bool
		next    byte
	}{
		{nil, sqlServerEncryptNotSupported, true, 0},
		{map[string]string{"encrypt": "false"}, sqlServerEncryptNotSupported, true, 0},
		{map[string]string{"encrypt": "disable"}, sqlServerEncryptNotSupported, false, 0x10},
		{nil, 0, false, 0x12},
	} {
		port, next := tdsPreloginResponder(t, tc.answer)
		config, _, err := sqlTLSConfig(sqlDriverTable[SQLDriverSQLServer], &sqlBase{}, tc.options, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		spec := sqlSpec(t, port, config, tc.options)
		spec.dial = sqlDial(nil, "127.0.0.1", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = connectSQLServer(ctx, spec)
		cancel()
		if errors.Is(err, errSQLServerNoEncryption) != tc.refused {
			t.Errorf("%v: %v, want refused %t", tc.options, err, tc.refused)
		}
		if got := <-next; got != tc.next {
			t.Errorf("%v: the server next received a packet of type %#x, want %#x (none is 0)", tc.options, got, tc.next)
		}
	}
}

// tdsPreloginResponder is a stand-in for SQL Server that reads a client's
// prelogin, answers it with the encryption value answer, and sends the type
// of the next packet the client writes, 0 when it writes none, before it
// closes the connection.
func tdsPreloginResponder(t *testing.T, answer byte) (port string, next chan byte) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	next = make(chan byte, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if _, err := tdsReadPacket(conn); err == nil {
				_, _ = conn.Write(tdsPreloginAnswer(answer))
			}
			header, _ := tdsReadPacket(conn)
			next <- header
			_ = conn.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return port, next
}

// tdsReadPacket reads one TDS packet from r and gives its type.
func tdsReadPacket(r io.Reader) (byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, err
	}
	_, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint16(header[2:4]))-8)
	return header[0], err
}

// tdsPreloginAnswer is a prelogin answer packet with a VERSION and the
// ENCRYPTION option encrypt.
func tdsPreloginAnswer(encrypt byte) []byte {
	payload := []byte{0x00, 0, 11, 0, 6, 0x01, 0, 17, 0, 1, 0xff, 16, 0, 0x07, 0xd0, 0, 0, encrypt}
	return tdsPacket(4, 1, payload)
}

// tdsPacket is a TDS packet of type with status and payload.
func tdsPacket(typ, status byte, payload []byte) []byte {
	packet := []byte{typ, status, 0, 0, 0, 0, 1, 0}
	binary.BigEndian.PutUint16(packet[2:4], uint16(8+len(payload)))
	return append(packet, payload...)
}

// The prelogin guard hands the driver every answer it does not refuse
// byte for byte, whether in one packet or several, malformed or not, and
// reads the ENCRYPTION option as the driver does: the last one given,
// TRACEID's data left unchecked.
func TestTheSQLServerPreloginGuardHandsOnWhatItDoesNotRefuse(t *testing.T) {
	notSup := []byte{0x01, 0, 6, 0, 1, 0xff, sqlServerEncryptNotSupported}
	for _, tc := range []struct {
		name    string
		answer  []byte
		refused bool
	}{
		{"not supported", tdsPreloginAnswer(sqlServerEncryptNotSupported), true},
		{"off", tdsPreloginAnswer(0), false},
		{"required", tdsPreloginAnswer(3), false},
		{"not supported in two packets", append(tdsPacket(4, 0, notSup[:3]), tdsPacket(4, 1, notSup[3:])...), true},
		{"twice, the last on", tdsPacket(4, 1, []byte{0x01, 0, 11, 0, 1, 0x01, 0, 12, 0, 1, 0xff, 2, 0}), false},
		{"twice, the last not supported", tdsPacket(4, 1, []byte{0x01, 0, 11, 0, 1, 0x01, 0, 12, 0, 1, 0xff, 0, 2}), true},
		{"after a trace id", tdsPacket(4, 1, []byte{0x05, 0xff, 0xff, 0, 9, 0x01, 0, 11, 0, 1, 0xff, 2}), true},
		{"out of bounds", tdsPacket(4, 1, []byte{0x01, 0, 40, 0, 1, 0xff, 2}), false},
		{"no terminator", tdsPacket(4, 1, []byte{0x01, 0, 5, 0, 1, 2}), false},
		{"empty", tdsPacket(4, 1, nil), false},
		{"short header", []byte{4, 1, 0, 4, 0, 0, 1, 0, 9, 9}, false},
		{"cut short", tdsPreloginAnswer(sqlServerEncryptNotSupported)[:12], false},
	} {
		client, server := net.Pipe()
		go func() {
			_, _ = server.Write(append(tc.answer, "after"...))
			_ = server.Close()
		}()
		got, err := io.ReadAll(&sqlServerPreloginGuard{Conn: client})
		_ = client.Close()
		if errors.Is(err, errSQLServerNoEncryption) != tc.refused || tc.refused && len(got) != 0 ||
			!tc.refused && !bytes.Equal(got, append(tc.answer, "after"...)) {
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
}

// The TLS of every driver's connection is what it was before the login-only
// encryption of sqlserver stopped verifying a certificate no tls key says
// how to verify, for every option and tls setting but that one: the
// function as it was, with sqlserver's encrypt false asking for verified
// TLS, is the oracle.
func TestOnlySQLServersLoginOnlyEncryptionChangedItsTLS(t *testing.T) {
	before := func(d *sqlDriver, base *sqlBase, options map[string]string, host string) (*tls.Config, string, error) {
		mode := sqlTLSModeOf(d, options, base.tls != model.TLSConfig{})
		if mode == sqlTLSVerifyByKeys {
			mode = sqlTLSVerify
		}
		if mode == sqlTLSOff {
			return nil, "", nil
		}
		cfg, err := tlsConfig(base.tls)
		if err != nil {
			return nil, "", err
		}
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		if mode == sqlTLSInsecure {
			cfg.InsecureSkipVerify = true
		}
		sum := sha256.New()
		fmt.Fprintf(sum, "%s\x00%t\x00", cfg.ServerName, cfg.InsecureSkipVerify)
		sum.Write([]byte{0, 0, 0})
		return cfg, hex.EncodeToString(sum.Sum(nil))[:32], nil
	}
	settings := []model.TLSConfig{{}, {ServerName: "db.internal"}, {InsecureSkipVerify: true}, {ServerName: "db.internal", InsecureSkipVerify: true}}
	compared := 0
	for name, d := range sqlDriverTable {
		values := []string{""}
		for value := range d.tlsModes {
			values = append(values, value)
		}
		for _, value := range values {
			var options map[string]string
			if value != "" {
				options = map[string]string{d.tlsOption: value}
			}
			for _, setting := range settings {
				base := &sqlBase{tls: setting}
				got, gotDigest, gotErr := sqlTLSConfig(d, base, options, "db.example")
				want, wantDigest, wantErr := before(d, base, options, "db.example")
				if gotErr != nil || wantErr != nil {
					t.Fatalf("%s %v %+v: %v %v", name, options, setting, gotErr, wantErr)
				}
				changed := name == SQLDriverSQLServer && value == "false" && setting == model.TLSConfig{}
				same := (got == nil) == (want == nil) && (got == nil || got.InsecureSkipVerify == want.InsecureSkipVerify && got.ServerName == want.ServerName && gotDigest == wantDigest)
				if same == changed {
					t.Errorf("%s %v %+v: %+v %s, before %+v %s", name, options, setting, got, gotDigest, want, wantDigest)
				}
				compared++
			}
		}
	}
	if compared < 3*len(settings) {
		t.Errorf("compared %d settings", compared)
	}
}

// The sqlserver configuration is what it was before, but for a connection
// that encrypts the login alone with no tls key, which now trusts the
// server's certificate: the function as it was is the oracle.
func TestOnlyTheSQLServerConfigurationOfLoginOnlyEncryptionChanged(t *testing.T) {
	before := func(spec *sqlConnectSpec) (msdsn.Config, error) {
		query := url.Values{"encrypt": {sqlServerEncrypt(spec)}, "app name": {spec.appName}}
		if spec.database != "" {
			query.Set("database", spec.database)
		}
		dsn := url.URL{Scheme: "sqlserver", User: url.UserPassword(spec.username, spec.password), Host: spec.hostPort, RawQuery: query.Encode()}
		config, err := msdsn.Parse(dsn.String())
		if err != nil {
			return config, err
		}
		if spec.tls != nil && config.Encryption != msdsn.EncryptionDisabled {
			config.TLSConfig = spec.tls
		}
		return config, nil
	}
	verified := &tls.Config{ServerName: "db.example", MinVersion: tls.VersionTLS12}
	unverified := &tls.Config{ServerName: "db.example", InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // G402: a configuration as insecure_skip_verify makes it.
	for _, config := range []*tls.Config{nil, verified, unverified} {
		for _, options := range []map[string]string{nil, {"encrypt": "disable"}, {"encrypt": "false"}, {"encrypt": "true"}, {"encrypt": "strict"}} {
			for _, database := range []string{"", "app"} {
				spec := sqlSpec(t, "1433", config, options)
				spec.database = database
				got, gotErr := sqlServerConfig(spec)
				want, wantErr := before(spec)
				if gotErr != nil || wantErr != nil {
					t.Fatalf("%v %v: %v %v", config != nil, options, gotErr, wantErr)
				}
				if sqlServerEncrypt(spec) == "false" && config == nil {
					if !got.TrustServerCertificate || got.TLSConfig == nil || !got.TLSConfig.InsecureSkipVerify {
						t.Errorf("%v: %+v, want the certificate trusted", options, got)
					}
					got.TrustServerCertificate, got.TLSConfig.InsecureSkipVerify = want.TrustServerCertificate, want.TLSConfig.InsecureSkipVerify
					delete(got.Parameters, msdsn.TrustServerCertificate)
				}
				// A connection's trace identifier is drawn at random.
				got.ActivityID, want.ActivityID = nil, nil
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%v %v %q: %+v, before %+v", config != nil, options, database, got, want)
				}
			}
		}
	}
}
