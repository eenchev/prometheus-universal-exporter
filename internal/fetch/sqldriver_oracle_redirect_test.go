//go:build (!select_request_types || request_type_sql) && sql_driver_oracle

package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// oracleListenerCertificate is a self-signed certificate for 127.0.0.1, a
// stand-in listener's.
func oracleListenerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tnsRedirect is a TNS REDIRECT packet sending the client to address.
func tnsRedirect(address string) []byte {
	packet := make([]byte, 10, 10+len(address))
	binary.BigEndian.PutUint16(packet, uint16(10+len(address)))
	packet[4] = 5
	binary.BigEndian.PutUint16(packet[8:], uint16(len(address)))
	return append(packet, address...)
}

// An oracle connection with ssl true never goes unencrypted: a listener
// that answers the CONNECT, over TLS, with a REDIRECT to the target's own
// host naming PROTOCOL=TCP, which go-ora would follow without TLS, has the
// connection to that address closed before anything is written on it, and
// the connection fails saying why. The address redirected to is a stand-in
// that records the first bytes it receives.
func TestAnOracleConnectionWithSSLNeverGoesUnencrypted(t *testing.T) {
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	received := make(chan []byte, 1)
	go func() {
		conn, err := plain.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		got := make([]byte, 512)
		n, _ := conn.Read(got)
		received <- got[:n]
	}()
	_, plainPort, _ := net.SplitHostPort(plain.Addr().String())
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{oracleListenerCertificate(t)}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(header))-8); err != nil {
			return
		}
		_, _ = conn.Write(tnsRedirect(fmt.Sprintf("(ADDRESS=(PROTOCOL=TCP)(HOST=127.0.0.1)(PORT=%s))", plainPort)))
		_, _ = io.Copy(io.Discard, conn)
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	spec := sqlSpec(t, port, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, map[string]string{"ssl": "true"}) //nolint:gosec // G402: the stand-in's certificate is not what the test is about.
	spec.serviceName = "ORCL"
	spec.dial = sqlDial(nil, "127.0.0.1", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := connectOracle(ctx, spec); !errors.Is(err, errOracleNoTLS) {
		t.Errorf("%v, want the unencrypted connection refused", err)
	}
	select {
	case got := <-received:
		if len(got) != 0 {
			t.Errorf("the address redirected to received %d bytes in clear: %q", len(got), got)
		}
	case <-ctx.Done():
		t.Error("the driver did not follow the redirect")
	}
}

// The check of an oracle connection with ssl true lets a TLS handshake
// through and refuses anything else as its first write, writing nothing of
// it; what follows the first write is not looked at.
func TestAnOracleConnectionWithSSLBeginsWithATLSHandshake(t *testing.T) {
	for _, tc := range []struct {
		first   []byte
		refused bool
	}{
		{[]byte{22, 3, 1, 0, 5}, false},
		{[]byte{0, 58, 0, 0, 1, 0, 0, 0}, true},
		{[]byte{22, 0, 0, 0, 1}, true},
		{[]byte{22}, true},
	} {
		client, server := net.Pipe()
		got := make(chan []byte, 1)
		go func() {
			b, _ := io.ReadAll(server)
			got <- b
		}()
		conn := &oracleTLSConn{Conn: client}
		_, err := conn.Write(tc.first)
		if errors.Is(err, errOracleNoTLS) != tc.refused {
			t.Errorf("%v: %v", tc.first, err)
		}
		if !tc.refused {
			if _, err := conn.Write([]byte{0, 58, 0, 0, 1}); err != nil {
				t.Errorf("%v, then a write: %v", tc.first, err)
			}
		}
		_ = conn.Close()
		if b := <-got; tc.refused && len(b) != 0 || !tc.refused && len(b) != len(tc.first)+5 {
			t.Errorf("%v: the server received %v", tc.first, b)
		}
	}
}
