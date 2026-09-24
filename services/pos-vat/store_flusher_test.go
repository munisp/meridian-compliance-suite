package main

import (
	"testing"
	"time"
)

// TestGroupCommitLingerFlusher verifies the time-based half of the
// group-commit policy: a single receipt (far below the 64-receipt
// high-water mark) must be fsynced by the background flusher within the
// linger bound, without any subsequent write traffic.
func TestGroupCommitLingerFlusher(t *testing.T) {
	st := newStoreWithLinger(t.TempDir(), 5*time.Millisecond)
	t.Cleanup(func() { st.Close() })

	if err := st.PutReceipt(&Receipt{ID: "linger-1", TenantID: "t1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// One write is well under syncHighWater, so no inline flush happened.
	if got := st.syncs(); got != 0 {
		t.Fatalf("premature sync: got %d, want 0", got)
	}
	// Poll (no fixed sleep): the flusher must fire well within 250ms even
	// though the test linger is only 5ms.
	deadline := time.Now().Add(250 * time.Millisecond)
	for st.syncs() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("background flusher did not fsync pending receipt within 250ms")
		}
		time.Sleep(time.Millisecond)
	}
	if st.pending() != 0 {
		t.Fatalf("pendingSync = %d after flush, want 0", st.pending())
	}
}

// TestCloseFlushesPending verifies Close stops the flusher and fsyncs any
// still-pending writes (no acknowledged receipt left un-fsynced).
func TestCloseFlushesPending(t *testing.T) {
	st := newStoreWithLinger(t.TempDir(), time.Hour) // flusher effectively disabled
	if err := st.PutReceipt(&Receipt{ID: "close-1", TenantID: "t1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := st.syncs(); got != 1 {
		t.Fatalf("syncCount = %d after Close, want 1 (final flush)", got)
	}
	// Idempotent second Close must not panic or corrupt state.
	if err := st.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func (st *Store) syncs() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.syncCount
}

func (st *Store) pending() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pendingSync
}
