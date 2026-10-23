package store

import (
	"path/filepath"
	"testing"
)

// TestPruneAuditBatchedKeepsNewestRows verifies trimming keeps the newest rows
// and does not leave the table oversized.
func TestPruneAuditBatchedKeepsNewestRows(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 5000; i++ {
		if err := st.AddAudit(AuditRow{TS: int64(i), Module: "m", Target: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.AuditCount(); n != 5000 {
		t.Fatalf("seed count = %d", n)
	}
	removed, err := st.PruneAuditBatched(1000)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4000 {
		t.Fatalf("removed = %d, want 4000", removed)
	}
	if n := st.AuditCount(); n != 1000 {
		t.Fatalf("after prune count = %d, want 1000", n)
	}
	rows, err := st.Audit(AuditQuery{Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("query failed: %v", err)
	}
	// newest row must survive
	if rows[0].TS != 4999 {
		t.Fatalf("newest row lost, got TS=%d", rows[0].TS)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// TestAuditWriteThrottle ensures the collector never lets a burst write
// everything to disk.
func TestPruneBelowLimitIsNoop(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 10; i++ {
		_ = st.AddAudit(AuditRow{TS: int64(i)})
	}
	removed, err := st.PruneAuditBatched(100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if n := st.AuditCount(); n != 10 {
		t.Fatalf("count = %d", n)
	}
}
