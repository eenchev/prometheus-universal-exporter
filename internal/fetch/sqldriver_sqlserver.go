//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// The sqlserver driver is microsoft/go-mssqldb. SQL Server has no read-only
// transaction (the driver refuses one), so a statement runs as it is: the
// account must be one that can only read. When the scrape's deadline comes
// the driver asks the server to stop the statement. encrypt says how much of
// the connection TLS covers: disable none of it, false the login alone (its
// certificate verified only as tls keys say, as SQL Server's own tools do
// not verify it), true and strict all of it (strict with TDS 8.0, TLS before
// anything else); unless it is disable, a server that answers it cannot
// encrypt is refused rather than sent the login in clear; a named instance
// is reached by its port, since finding it would ask the SQL Server Browser
// over UDP, which the exporter's dialer refuses.
func init() {
	registerSQLDriver(&sqlDriver{
		name:      SQLDriverSQLServer,
		port:      func(map[string]string) string { return "1433" },
		options:   map[string][]string{"encrypt": {"disable", "false", "true", "strict"}},
		tlsOption: "encrypt",
		tlsModes:  map[string]sqlTLSMode{"disable": sqlTLSOff, "false": sqlTLSVerifyByKeys, "true": sqlTLSVerify, "strict": sqlTLSVerify},
		connect:   connectSQLServer,
		run:       sqlPlain,
	})
}

// sqlServerEncrypt is the encrypt setting of a connection: the option, or,
// left out, true for one with TLS settings and false otherwise.
func sqlServerEncrypt(spec *sqlConnectSpec) string {
	if value, ok := spec.options["encrypt"]; ok {
		return value
	}
	if spec.tls != nil {
		return "true"
	}
	return "false"
}

// sqlServerConfig is the configuration of a connection made with spec.
func sqlServerConfig(spec *sqlConnectSpec) (msdsn.Config, error) {
	encrypt := sqlServerEncrypt(spec)
	query := url.Values{"encrypt": {encrypt}, "app name": {spec.appName}}
	if encrypt == "false" && spec.tls == nil {
		// The login alone is encrypted, and with no tls keys its
		// certificate, which SQL Server makes itself unless given one, is
		// not verified, as SQL Server's own tools do not verify it.
		query.Set("TrustServerCertificate", "true")
	}
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

func connectSQLServer(ctx context.Context, spec *sqlConnectSpec) (driver.Conn, error) {
	config, err := sqlServerConfig(spec)
	if err != nil {
		return nil, err
	}
	connector := mssql.NewConnectorConfig(config)
	// The prelogin answer is read in clear unless the connection is TLS
	// from its first byte (strict), so the guard reads it for every other
	// setting; it refuses a server that cannot encrypt for encrypt false
	// alone.
	connector.Dialer = sqlServerDialer{
		host: spec.host, dial: spec.dial,
		guard:            config.Encryption != msdsn.EncryptionStrict,
		allowUnencrypted: config.Encryption != msdsn.EncryptionOff,
	}
	return connector.Connect(ctx)
}

// sqlServerDialer hands the driver's connections to the exporter's dialer,
// with the host's name rather than an address the driver looked up
// (mssql.HostDialer), so the dialer resolves it and holds it to the policy.
// guard says the server's prelogin answer is read in clear, and is checked
// before the driver reads it (sqlServerPreloginGuard); allowUnencrypted
// that an answer saying the server cannot encrypt is the driver's to take
// (encrypt disable) or refuse (true), not the guard's.
type sqlServerDialer struct {
	host             string
	dial             func(ctx context.Context, network, address string) (net.Conn, error)
	guard            bool
	allowUnencrypted bool
}

func (d sqlServerDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.dial(ctx, network, address)
	if err != nil || !d.guard {
		return conn, err
	}
	return &sqlServerPreloginGuard{Conn: conn, allowUnencrypted: d.allowUnencrypted}, nil
}

func (d sqlServerDialer) HostName() string { return d.host }

// errSQLServerNoEncryption is the error of a connection to a server that
// answers it cannot encrypt, which would be sent the login in clear.
var errSQLServerNoEncryption = errors.New("the server answered that it does not support encryption, so the login would be sent unencrypted; " +
	"enable encryption on the server, or set request.options.encrypt to disable to send the login unencrypted")

// errSQLServerEmptyEncryption is the error of a connection to a server
// whose prelogin answer has an ENCRYPTION option with no byte, which the
// driver reads without checking and panics on.
var errSQLServerEmptyEncryption = errors.New("the server's answer to the prelogin has an empty ENCRYPTION option, which no SQL Server sends; " +
	"check that the target is a SQL Server and that nothing between it and the exporter changes what it sends")

// sqlServerPreloginGuard is a connection that reads the server's answer to
// the driver's prelogin, its first message, before the driver does, and
// fails the read when that answer says the server does not support
// encryption, unless allowUnencrypted, or when its ENCRYPTION option is
// empty. With encrypt false the driver sends the login unencrypted to
// such a server, and offers no hook to refuse it (go-mssqldb tds.go,
// interpretPreloginResponse refuses that answer only for encrypt true);
// and it reads the option's first byte without checking there is one
// (interpretPreloginResponse, encryptBytes[0]), whatever encrypt is. The
// answer is read as the driver reads it (readPrelogin): packets up to the
// first with a status, then the option table; an answer the driver could not
// read either is handed to it unchanged, for it to refuse.
type sqlServerPreloginGuard struct {
	net.Conn
	allowUnencrypted bool
	checked          bool
	// pending is what was read of the answer and not yet handed on.
	pending []byte
	err     error
}

// sqlServerPreloginLimit bounds the answer read: a prelogin answer is a few
// dozen bytes.
const sqlServerPreloginLimit = 1 << 16

func (c *sqlServerPreloginGuard) Read(b []byte) (int, error) {
	if !c.checked {
		c.checked = true
		c.pending, c.err = c.readAnswer()
	}
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(b)
}

// readAnswer reads the server's first message and gives what it read, with
// errSQLServerNoEncryption, and nothing to hand on, when the message says
// the server does not support encryption.
func (c *sqlServerPreloginGuard) readAnswer() ([]byte, error) {
	var read, payload []byte
	for {
		header := make([]byte, 8)
		n, err := io.ReadFull(c.Conn, header)
		read = append(read, header[:n]...)
		if err != nil {
			return read, err
		}
		size := int(binary.BigEndian.Uint16(header[2:4]))
		if size < len(header) || len(read)+size > sqlServerPreloginLimit {
			return read, nil
		}
		body := make([]byte, size-len(header))
		n, err = io.ReadFull(c.Conn, body)
		read = append(read, body[:n]...)
		if err != nil {
			return read, err
		}
		payload = append(payload, body...)
		if header[1] != 0 {
			break
		}
	}
	switch sqlServerPreloginEncryption(payload) {
	case sqlServerEncryptEmpty:
		return nil, errSQLServerEmptyEncryption
	case sqlServerEncryptNotSupported:
		if !c.allowUnencrypted {
			return nil, errSQLServerNoEncryption
		}
	}
	return read, nil
}

// sqlServerEncryptNotSupported is the prelogin ENCRYPTION value of a server
// that does not support encryption (ENCRYPT_NOT_SUP).
const sqlServerEncryptNotSupported = 2

// sqlServerEncryptEmpty is what sqlServerPreloginEncryption gives for an
// ENCRYPTION option with no byte.
const sqlServerEncryptEmpty = -2

// sqlServerPreloginEncryption is the first byte of the ENCRYPTION option
// (token 1) of a prelogin answer, the last one when it is given twice, as
// the driver keeps it, sqlServerEncryptEmpty when that option has no byte,
// or -1 when the driver could not read the option table or it has no such
// option.
func sqlServerPreloginEncryption(payload []byte) int {
	const terminator, traceID, encryption, optionSize = 0xff, 0x05, 0x01, 5
	value := -1
	for offset := 0; ; offset += optionSize {
		if offset >= len(payload) {
			return -1
		}
		token := payload[offset]
		if token == terminator {
			return value
		}
		if offset+optionSize >= len(payload) {
			return -1
		}
		start := int(binary.BigEndian.Uint16(payload[offset+1:]))
		length := int(binary.BigEndian.Uint16(payload[offset+3:]))
		if token == traceID {
			continue
		}
		if start+length > len(payload) || start >= len(payload) {
			return -1
		}
		if token == encryption {
			value = sqlServerEncryptEmpty
			if length > 0 {
				value = int(payload[start])
			}
		}
	}
}
