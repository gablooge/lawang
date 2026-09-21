package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/ingress"
)

// TestTheServerTimeoutsAreSetAndOrdered. The webhook edge mounts on this server, so the numbers
// here are the ones that decide how long a stranger can hold a connection open. A zero is no
// timeout at all, which is the defect this guards against.
func TestTheServerTimeoutsAreSetAndOrdered(t *testing.T) {
	t.Parallel()

	srv := newServer(testHandler(t))
	switch {
	case srv.ReadHeaderTimeout <= 0:
		t.Fatal("ReadHeaderTimeout is not set: a client could dribble headers forever")
	case srv.ReadTimeout <= 0:
		t.Fatal("ReadTimeout is not set: a client could dribble a body forever")
	case srv.WriteTimeout <= 0:
		t.Fatal("WriteTimeout is not set")
	case srv.IdleTimeout <= 0:
		t.Fatal("IdleTimeout is not set")
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Fatalf("ReadTimeout (%v) covers the headers too, so it cannot be under ReadHeaderTimeout (%v)",
			srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	// A request that is cut off while the accept path is still deciding would be answered by
	// nothing at all, and the provider would see a reset instead of a status.
	if srv.WriteTimeout <= ingress.DefaultAcceptTimeout {
		t.Fatalf("WriteTimeout (%v) must be above the accept timeout (%v)",
			srv.WriteTimeout, ingress.DefaultAcceptTimeout)
	}
}

// TestASlowClientIsCutOff proves the mechanism rather than the production durations, which would
// make the test wait five seconds. The server is the one newServer builds, with the two read
// timeouts shortened; a client that stalls in the headers and a client that stalls in the body
// must both be dropped, on a path shaped like the webhook edge.
func TestASlowClientIsCutOff(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// Headers that never end.
		"slow headers": "POST /ingress/fake HTTP/1.1\r\nHost: x\r\nX-A: 1\r\n",
		// Headers complete, then a body that stops short of its Content-Length.
		"slow body": "POST /ingress/fake HTTP/1.1\r\nHost: x\r\nContent-Length: 4096\r\n\r\nab",
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			srv := newServer(testHandler(t))
			srv.ReadHeaderTimeout = 100 * time.Millisecond
			srv.ReadTimeout = 200 * time.Millisecond
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			// The deadline is the test's own failure mode: without it a server that never cuts
			// the client off would hang the run instead of failing it.
			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatalf("SetDeadline: %v", err)
			}
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatalf("write: %v", err)
			}

			// The server either closes the connection or answers 408. Either ends the stall.
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode/100 == 2 {
					t.Fatalf("a stalled client got %d", resp.StatusCode)
				}
				return
			}
			// Any other error is the server having closed the connection, which is the point.
			// The one failure is this test's own deadline running out: that would mean the
			// stall worked.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("the connection was still open after the read timeout: %v", err)
			}
		})
	}
}

// TestHealthzStillAnswers keeps the wiring honest: /healthz is an ingress.Route now, behind the
// same path guard as the webhook endpoint, and the handler ingress.New returns is the one served.
func TestHealthzStillAnswers(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}
