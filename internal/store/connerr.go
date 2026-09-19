package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// connErrPrefix names the variable the operator has to look at, never its value.
const connErrPrefix = "SLUICEWAY_DATABASE_URL: cannot use the database"

// connError turns a failure to reach or to talk to the database into an error that is safe to
// log.
//
// pgx quotes the connection target in what it returns ("failed to connect to `user=app
// database=prod`: ... lookup db.internal: no such host", "10.0.0.5:5432 (db.internal): failed SASL
// auth"), and the server does the same in its own messages (`password authentication failed for
// user "app"`, `database "prod" does not exist`, `no pg_hba.conf entry for host ...`). Every one
// of those is a part of SLUICEWAY_DATABASE_URL. So the original error is never returned, wrapped
// or formatted: only a classification that cannot carry a value is kept, which is still enough to
// tell "does not resolve" from "refused" from "wrong password" from "no such database".
//
// The order matters. The server's verdict comes first, because with sslmode=prefer pgx joins the
// error of the TLS attempt to the error of the plain one, and the SQLSTATE is the one that says
// what is wrong.
func connError(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return errors.New(connErrPrefix + ": " + describeSQLState(pgErr.Code))
	}
	// Cancelled or out of time, for example a signal during a slow connect. Wrapped, so that
	// errors.Is(err, context.Canceled) keeps working; the text of a context error is constant.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", connErrPrefix, ctxErr)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// DNSError.Error() quotes the name, so only its classification is used.
		switch {
		case dnsErr.IsNotFound:
			return errors.New(connErrPrefix + ": the host name does not resolve")
		case dnsErr.IsTimeout:
			return errors.New(connErrPrefix + ": the host name lookup timed out")
		default:
			return errors.New(connErrPrefix + ": the host name lookup failed")
		}
	}
	var netErr net.Error
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return errors.New(connErrPrefix + ": the connection attempt timed out (the host drops packets, or connect_timeout is too short)")
	}
	// An errno's text comes from the operating system's table ("connection refused", "network is
	// unreachable"), never from the input. Wrapping it alone keeps errors.Is(err,
	// syscall.ECONNREFUSED) working.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("%s: %w", connErrPrefix, errno)
	}
	if isTLSFailure(err) {
		return errors.New(connErrPrefix + ": TLS negotiation failed (check sslmode and the certificates)")
	}
	return errors.New(connErrPrefix + ": the connection failed")
}

// scrub is connError for the paths where most errors are not about the connection at all: a
// transaction body, a migration. An error is replaced only when it carries the connection target
// (pgx's connect error, or a net error with its addresses); anything else, a constraint violation
// for example, reaches the caller untouched, SQLSTATE and all.
//
// It judges the error and not its origin, so a net error that a transaction function produced by
// itself is replaced as well, wrapping and sentinels included. That is deliberate: see begin.
func scrub(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var (
		connectErr *pgconn.ConnectError
		opErr      *net.OpError
		dnsErr     *net.DNSError
	)
	if errors.As(err, &connectErr) || errors.As(err, &opErr) || errors.As(err, &dnsErr) {
		return connError(ctx, err)
	}
	return err
}

// describeSQLState words a server refusal from its SQLSTATE alone. The message, detail and hint of
// the server are never used: at connection time they quote the role, the database and the client
// address.
func describeSQLState(code string) string {
	// The code is five characters from the server. It is checked anyway, so that nothing but a
	// code can ever be printed here, whatever is on the other end of the connection.
	if !validSQLState(code) {
		return "the server refused, with no usable SQLSTATE"
	}
	switch {
	case strings.HasPrefix(code, "28"):
		return "authentication failed, check the user, the password and pg_hba.conf (SQLSTATE " + code + ")"
	case code == "3D000":
		return "the database does not exist (SQLSTATE 3D000)"
	case code == "42501":
		return "the role may not connect to the database (SQLSTATE 42501)"
	case code == "53300":
		return "the server has no connection slot left (SQLSTATE 53300)"
	case code == "57P03":
		return "the server is not accepting connections yet (SQLSTATE 57P03)"
	default:
		return "the server refused (SQLSTATE " + code + ")"
	}
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := range len(code) {
		c := code[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// isTLSFailure recognizes a failed TLS setup. The typed errors of crypto/tls and crypto/x509 come
// first. pgx reports a server that declines TLS, and a handshake it could not finish, only as
// text, so that text is read as well: read, to classify, and never repeated.
func isTLSFailure(err error) bool {
	var (
		certErr      *tls.CertificateVerificationError
		recordErr    tls.RecordHeaderError
		alertErr     tls.AlertError
		authorityErr x509.UnknownAuthorityError
		hostErr      x509.HostnameError
		invalidErr   x509.CertificateInvalidError
	)
	if errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) ||
		errors.As(err, &authorityErr) || errors.As(err, &hostErr) || errors.As(err, &invalidErr) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "tls error") || strings.Contains(text, "server refused TLS connection")
}
