package runtime

import (
	"crypto/tls"
	"path/filepath"
	"testing"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
	"github.com/LECYWZA/mytun/internal/store"
)

// TestSessionRemovedAfterClientDisconnects guards a regression: authenticated
// sessions used to stay in the inbound list forever after the peer went away,
// which made dead clients look "online" and stalled forwards aimed at them.
func TestSessionRemovedAfterClientDisconnects(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertServer(store.ServerRow{
		ID: "s1", Alias: "test", BindHost: "127.0.0.1", Port: 33981,
		Password: "pw-test", CommonName: "mail.qq.com", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	rt := New(Options{Store: st, CertDir: dir, DataDir: dir, AutoStart: true})
	defer rt.Stop()
	if err := rt.Start(); err != nil {
		t.Fatal(err)
	}

	inbound := func() []string {
		var ids []string
		if list, ok := rt.Status()["inbound"].([]map[string]any); ok {
			for _, e := range list {
				if id, ok := e["clientId"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}
	if got := inbound(); len(got) != 0 {
		t.Fatalf("expected no sessions yet, got %v", got)
	}

	// Connect a client using the same TLS + mux stack the real client uses.
	conn, err := tls.Dial("tcp", "127.0.0.1:33981", &tls.Config{
		InsecureSkipVerify: true, // self-signed by design
		ServerName:         "mail.qq.com",
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sealer, err := mux.NewSealer(mux.DeriveKey("pw-test"))
	if err != nil {
		t.Fatal(err)
	}
	sess := mux.NewSession(conn, sealer, false)
	done := make(chan struct{})
	go func() { _ = sess.Run(); close(done) }()
	if err := sess.SendAuth("pw-test", "tester", ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(inbound()) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := inbound(); len(got) != 1 || got[0] != "tester" {
		t.Fatalf("expected the client to be registered, got %v", got)
	}

	// Drop the connection; the server must forget the session shortly after.
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client session did not stop")
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(inbound()) == 0 {
			return // cleaned up
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("disconnected client still listed as inbound: %v", inbound())
}
