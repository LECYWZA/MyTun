package tunnel

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// dialTestClient connects and authenticates a client session against srv.
func dialTestClient(t *testing.T, port int, password, clientID string) *mux.Session {
	t.Helper()
	conn, err := tls.Dial("tcp", "127.0.0.1:"+itoa(port), &tls.Config{
		InsecureSkipVerify: true, // self-signed by design
		ServerName:         "mail.qq.com",
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sealer, err := mux.NewSealer(mux.DeriveKey(password))
	if err != nil {
		t.Fatal(err)
	}
	sess := mux.NewSession(conn, sealer, false)
	go func() { _ = sess.Run() }()
	if err := sess.SendAuth(password, clientID, ""); err != nil {
		t.Fatal(err)
	}
	return sess
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// statsWithin reads Stats with a hard timeout so a deadlock fails the test
// instead of hanging until the go test timeout.
func statsWithin(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	done := make(chan map[string]any, 1)
	go func() { done <- srv.Stats() }()
	select {
	case st := <-done:
		return st
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Stats() blocked — deadlock on the server mutex")
		return nil
	}
}

// stopGuarded stops the server without letting a deadlock hang the test again.
func stopGuarded(srv *Server) {
	done := make(chan struct{})
	go func() { _ = srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func waitSessions(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := statsWithin(t, srv)["clients"].(int); n == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d sessions, got %v", want, statsWithin(t, srv)["clients"])
}

// TestServerSurvivesSameClientReconnect guards a deadlock: replacing a session
// under the same client id used to call Close() while holding the server mutex,
// and Close fires OnClose which takes that same (non-reentrant) mutex.
func TestServerSurvivesSameClientReconnect(t *testing.T) {
	srv, err := NewServer(ServerConfig{BindHost: "127.0.0.1", Port: 33991, Password: "pw", CommonName: "mail.qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopGuarded(srv)

	first := dialTestClient(t, 33991, "pw", "dup")
	defer first.Close()
	waitSessions(t, srv, 1)

	second := dialTestClient(t, 33991, "pw", "dup")
	defer second.Close()
	// Give the server a moment to process the replacement; without this the
	// assertion below can run before the handler reaches the swap.
	time.Sleep(400 * time.Millisecond)

	// The server must still answer: a deadlock here blocks every reader.
	done := make(chan map[string]any, 1)
	go func() { done <- srv.Stats() }()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Stats() blocked after a same-id reconnect (deadlock)")
	}
}

// TestServerStopWithLiveSession guards the same reentrancy through Stop().
func TestServerStopWithLiveSession(t *testing.T) {
	srv, err := NewServer(ServerConfig{BindHost: "127.0.0.1", Port: 33992, Password: "pw", CommonName: "mail.qq.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	sess := dialTestClient(t, 33992, "pw", "live")
	defer sess.Close()
	waitSessions(t, srv, 1)

	done := make(chan struct{})
	go func() { _ = srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() blocked with a live session (deadlock)")
	}
}
