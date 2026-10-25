//go:build (!select_request_types || request_type_sql) && sql_driver_vertica

package fetch

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// verticaWire is a Vertica server as far as the vertica driver's
// connection and statements reach one: it accepts any login, answers a
// simple query, a prepared statement's parse, describe, bind and execute,
// and a close, with one varchar column, one, of "1" for a SELECT, and
// records the text of every statement it is sent, prepared or not, and the
// values bound to them.
type verticaWire struct {
	mu sync.Mutex
	// texts are the statements parsed (prepared) and queried (sent as
	// text), each after "parse: " or "query: ".
	texts []string
	// bound are the values bound to prepared statements.
	bound []string
}

func newVerticaWire(t *testing.T) (w *verticaWire, port string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	w = &verticaWire{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go w.serve(conn)
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return w, port
}

func (w *verticaWire) record(texts []string, bound []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.texts = append(w.texts, texts...)
	w.bound = append(w.bound, bound...)
}

func (w *verticaWire) recorded() (texts, bound []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.texts...), append([]string(nil), w.bound...)
}

// verticaWireMsg is a backend message under construction.
type verticaWireMsg struct{ bytes.Buffer }

func (m *verticaWireMsg) u16(v uint16) *verticaWireMsg {
	_ = binary.Write(m, binary.BigEndian, v)
	return m
}

func (m *verticaWireMsg) u32(v uint32) *verticaWireMsg {
	_ = binary.Write(m, binary.BigEndian, v)
	return m
}

func (m *verticaWireMsg) str(s string) *verticaWireMsg {
	m.WriteString(s)
	m.WriteByte(0)
	return m
}

func (w *verticaWire) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	in := bufio.NewReader(conn)
	out := bufio.NewWriter(conn)
	send := func(tag byte, m *verticaWireMsg) {
		if m == nil {
			m = &verticaWireMsg{}
		}
		_ = out.WriteByte(tag)
		_ = binary.Write(out, binary.BigEndian, uint32(m.Len()+4))
		_, _ = out.Write(m.Bytes())
	}
	read := func() ([]byte, bool) {
		var size uint32
		if binary.Read(in, binary.BigEndian, &size) != nil || size < 4 {
			return nil, false
		}
		body := make([]byte, size-4)
		_, err := io.ReadFull(in, body)
		return body, err == nil
	}
	// A column of type oid named name, of no table.
	rowDesc := func(name string, oid uint32) *verticaWireMsg {
		m := (&verticaWireMsg{}).u16(1).u32(0).str(name)
		m.Write(make([]byte, 8)) // table oid
		m.u16(0)                 // attribute number
		m.WriteByte(0)           // not a user type
		m.u32(oid).u16(0xffff).u16(1).u16(0).u32(0xffffffff).u16(0)
		return m
	}
	dataRow := func(value string) *verticaWireMsg {
		m := (&verticaWireMsg{}).u16(1).u32(uint32(len(value)))
		m.WriteString(value)
		return m
	}
	ready := func() *verticaWireMsg {
		m := &verticaWireMsg{}
		m.WriteByte('I')
		return m
	}
	if _, ok := read(); !ok { // the startup message
		return
	}
	send('R', (&verticaWireMsg{}).u32(0))
	send('K', (&verticaWireMsg{}).u32(1).u32(2))
	send('Z', ready())
	_ = out.Flush()
	prepared := map[string]string{}
	var bound string
	for {
		tag, err := in.ReadByte()
		if err != nil {
			return
		}
		body, ok := read()
		if !ok {
			return
		}
		fields := bytes.Split(body, []byte{0})
		switch tag {
		case 'Q':
			text := string(fields[0])
			w.record([]string{"query: " + text}, nil)
			if strings.Contains(text, "now()") {
				send('T', rowDesc("now", 13))
				send('D', dataRow("2026-10-25 10:00:00+00"))
			} else {
				send('T', rowDesc("one", 9))
				send('D', dataRow("1"))
			}
			send('C', (&verticaWireMsg{}).str("SELECT"))
			send('Z', ready())
		case 'P': // name, command, parameter types
			name, text := string(fields[0]), string(fields[1])
			prepared[name] = text
			w.record([]string{"parse: " + text}, nil)
			send('1', nil)
		case 'D': // 'S' and the statement's name
			text := prepared[string(body[1:len(body)-1])]
			params := strings.Count(text, "?")
			m := (&verticaWireMsg{}).u16(uint16(params)).u32(0)
			for range params {
				m.WriteByte(0)
				m.u32(9).u32(0xffffffff).u16(1)
			}
			send('t', m)
			bound = text
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(text)), "SELECT") {
				send('T', rowDesc("one", 9))
			} else {
				send('n', nil)
			}
		case 'B': // portal, statement, formats, values (after the types)
			portalEnd := bytes.IndexByte(body, 0)
			statementEnd := portalEnd + 1 + bytes.IndexByte(body[portalEnd+1:], 0)
			bound = prepared[string(body[portalEnd+1:statementEnd])]
			rest := body[statementEnd+1+2:]
			n := int(binary.BigEndian.Uint16(rest))
			rest = rest[2+4*strings.Count(bound, "?"):]
			var values []string
			for range n {
				size := binary.BigEndian.Uint32(rest)
				rest = rest[4:]
				if size == 0xffffffff {
					continue
				}
				values = append(values, string(rest[:size]))
				rest = rest[size:]
			}
			w.record(nil, values)
			send('2', nil)
		case 'E':
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(bound)), "SELECT") {
				send('D', dataRow("1"))
			}
			send('C', (&verticaWireMsg{}).str("OK"))
		case 'C':
			send('3', nil)
		case 'S':
			send('Z', ready())
		case 'X':
			_ = out.Flush()
			return
		}
		if tag == 'H' || tag == 'Q' || tag == 'S' {
			_ = out.Flush()
		}
	}
}

// verticaWireConnector makes connections through connectVertica to the
// verticaWire at port, guarded as a pool's connector guards them.
type verticaWireConnector struct {
	t    *testing.T
	port string
}

func (c verticaWireConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := connectVertica(ctx, sqlSpec(c.t, c.port, nil, nil))
	if err != nil {
		return nil, err
	}
	return guardSQLConn(SQLDriverVertica, conn), nil
}

func (verticaWireConnector) Driver() driver.Driver { return sqlDriverStub{} }

// A vertica statement the exporter loads is prepared on the server, and a
// probe's value is bound to it, never written into its text, so that a
// value ending in a \, which in an E'...' or with
// standard_conforming_strings off escapes the quote the driver would close
// it with, cannot end a string early. The driver writes the args into the
// text of a statement it does not prepare (stmt.go interpolate), which is
// one it takes for a COPY ... FROM LOCAL by its own reading, however the
// server reads it: such a statement is refused when the configuration is
// loaded, in any case and with comments anywhere, and so is every other
// COPY, as is a function's DDL, which the driver sends unprepared too. The
// statements refused here were let through, and the driver sent them to
// the server with the value inside them.
func TestAVerticaProbeValueIsBoundNeverWrittenIntoTheStatement(t *testing.T) {
	file := testutil.WriteIn(t, t.TempDir(), "rows.csv", "1\n")
	const value = `x\`
	for _, tc := range []struct {
		statement string
		refused   string
	}{
		{"SELECT ? AS one", ""},
		{"SELECT 'copy' AS one, ? AS two", ""},
		{"SELECT 1 AS copy, ? AS two", ""},
		{"/* COPY t FROM LOCAL 'f' */ SELECT ? AS one", ""},
		{"COPY t FROM LOCAL '" + file + "' DELIMITER ?", "COPY ... FROM LOCAL"},
		{"copy /* c */ t -- d\n from LOCAL '" + file + "' delimiter ?", "COPY ... FROM LOCAL"},
		{"  -- c\n Copy t FROM local '" + file + "', '" + file + "' DELIMITER ?;", "COPY ... FROM LOCAL"},
		{"/* /* */ COPY t FROM LOCAL '" + file + "' */ SELECT ? AS one", "COPY ... FROM LOCAL"},
		{`"x" COPY t FROM LOCAL '` + file + `' DELIMITER ?`, "COPY ... FROM LOCAL"},
		{"COPY t FROM STDIN DELIMITER ?", "is a COPY, which loads rows"},
		{"/* c */ copy t FROM '/data/rows.csv' DELIMITER ?", "is a COPY, which loads rows"},
		{"CREATE OR REPLACE FUNCTION f(x int) RETURN int AS BEGIN RETURN x + ? END", "function's DDL"},
		{"GRANT EXECUTE ON FUNCTION f(int) TO metrics", "function's DDL"},
	} {
		c := &model.Collector{Name: "db", Request: model.RequestConfig{
			Type: RequestTypeSQL, Driver: SQLDriverVertica, Statement: tc.statement, Username: "metrics", Args: []string{"{{param_x}}"},
		}}
		err := ValidateRequest(c)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%q: loading gives %v, want %q", tc.statement, err, tc.refused)
			} else {
				continue
			}
		} else if err != nil {
			t.Errorf("%q: %v", tc.statement, err)
			continue
		}
		wire, port := newVerticaWire(t)
		db := sql.OpenDB(verticaWireConnector{t, port})
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rows, done, err := sqlReadOnlyTx(nil)(context.Background(), conn, tc.statement, []any{value}, 0)
		if err == nil {
			for rows.Next() {
			}
			done()
		}
		_ = conn.Close()
		_ = db.Close()
		texts, bound := wire.recorded()
		for _, text := range texts {
			if strings.Contains(text, value) {
				t.Errorf("%q: the server was sent the value inside the statement: %s", tc.statement, text)
			}
		}
		if tc.refused == "" && (err != nil || !strings.Contains(strings.Join(texts, "\n"), "parse: "+tc.statement) || len(bound) != 1 || bound[0] != value) {
			t.Errorf("%q: %v; the server was sent %q, bound %q, want it prepared as written and %q bound", tc.statement, err, texts, bound, value)
		}
	}
}
