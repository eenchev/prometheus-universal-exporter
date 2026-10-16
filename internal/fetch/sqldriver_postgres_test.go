//go:build !select_request_types || request_type_sql

package fetch

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A postgres statement whose context ends makes the server get a cancel
// request for it, through the exporter's dialer, for a target named by a
// host the policy holds: pgx sends it to the address its connection
// reached, and the dialer reaches that address by dialing the target's host
// again. A cancel request whose connection would reach another address is
// not sent.
func TestAPostgresStatementWhoseContextEndsIsCancelledAtTheServer(t *testing.T) {
	port, statements, cancels := postgresStandIn(t)
	policy, err := compileTargetPolicy([]string{"127.0.0.0/8", "::1/128"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := sqlSpec(t, port, nil, nil)
	spec.host, spec.hostPort, spec.dial = "localhost", net.JoinHostPort("localhost", port), sqlDial(policy, "localhost", 0)
	conn, err := connectPostgres(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-statements
		cancel()
	}()
	if _, err := conn.(driver.QueryerContext).QueryContext(ctx, "SELECT pg_sleep(60)", nil); err == nil {
		t.Error("the statement ran though its context ended")
	}
	select {
	case pid := <-cancels:
		if pid != postgresStandInPID {
			t.Errorf("the cancel request names the process %d, want %d", pid, postgresStandInPID)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no cancel request reached the server")
	}
	if _, err := postgresDial(spec)(context.Background(), "tcp", net.JoinHostPort("127.0.0.2", port)); err == nil || !strings.Contains(err.Error(), "not the statement's server") {
		t.Errorf("%v, want the cancel request to another address refused", err)
	}
}

// postgresStandInPID is the process the stand-in for postgres says each
// connection is served by.
const postgresStandInPID = 4242

// postgresStandIn is a stand-in for postgres that takes every login, sends
// on statements when a statement comes, which it never answers, and sends
// on cancels the process each cancel request names.
func postgresStandIn(t *testing.T) (port string, statements chan struct{}, cancels chan uint32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	statements, cancels = make(chan struct{}, 16), make(chan uint32, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				startup, err := backend.ReceiveStartupMessage()
				if err != nil {
					return
				}
				if request, ok := startup.(*pgproto3.CancelRequest); ok {
					cancels <- request.ProcessID
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				backend.Send(&pgproto3.BackendKeyData{ProcessID: postgresStandInPID, SecretKey: []byte{1, 2, 3, 4}})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if backend.Flush() != nil {
					return
				}
				for {
					msg, err := backend.Receive()
					if err != nil {
						return
					}
					switch msg.(type) {
					case *pgproto3.Parse, *pgproto3.Query:
						statements <- struct{}{}
					}
				}
			}()
		}
	}()
	_, port, _ = net.SplitHostPort(listener.Addr().String())
	return port, statements, cancels
}

// postgres ends a statement at the statement_timeout the exporter sets to
// the scrape's time left, and its clock may do so a moment before the
// scrape's context ends: the failure still says the deadline came first,
// and is a deadline. A statement a cancel request ended, though it has the
// same SQLSTATE, is not, nor is another driver's failure.
func TestAPostgresStatementTimeoutIsTheScrapesDeadline(t *testing.T) {
	postgres := &sqlPool{driver: sqlDriverTable[SQLDriverPostgres]}
	timeout := &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"}
	err := postgres.sqlDeadline(context.Background(), "running the statement", timeout)
	if !strings.Contains(err.Error(), "running the statement: the scrape's deadline came first") || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, new(*pgconn.PgError)) {
		t.Errorf("a statement timeout: %v", err)
	}
	cancelled := &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to user request"}
	if err := postgres.sqlDeadline(context.Background(), "running the statement", cancelled); strings.Contains(err.Error(), "deadline") || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a cancel request: %v", err)
	}
	mysql := &sqlPool{driver: sqlDriverTable[SQLDriverMySQL]}
	if err := mysql.sqlDeadline(context.Background(), "running the statement", timeout); strings.Contains(err.Error(), "deadline") {
		t.Errorf("another driver: %v", err)
	}
}
