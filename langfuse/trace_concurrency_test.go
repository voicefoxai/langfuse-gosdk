package langfuse

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"
)

// newQueueOnlyClient builds a client that enqueues but never auto-flushes (no
// network), so tests can inspect the batcher queue directly.
func newQueueOnlyClient(t *testing.T) *Client {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.PublicKey = "pk-test"
	cfg.SecretKey = "sk-test"
	cfg.FlushInterval = time.Hour
	cfg.FlushAt = 1 << 30
	cfg.MaxQueueSize = 1 << 20
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		c.batcher.mu.Lock()
		c.batcher.queue = nil
		c.batcher.mu.Unlock()
		close(c.batcher.done)
	})
	return c
}

// TestTraceUpdateDoesNotMutateQueuedEventMetadata reproduces the process-crashing
// bug: Trace.Update merges into the same metadata map that an already-enqueued
// create event still references. The batcher marshals queued events asynchronously,
// so the in-place insert races json.Marshal -> "index out of range" panic in
// encoding/json mapEncoder.
func TestTraceUpdateDoesNotMutateQueuedEventMetadata(t *testing.T) {
	c := newQueueOnlyClient(t)

	tr, err := c.CreateTrace(TraceParams{Metadata: map[string]interface{}{"a": 1}})
	if err != nil {
		t.Fatalf("CreateTrace: %v", err)
	}

	c.batcher.mu.Lock()
	queuedMeta, _ := c.batcher.queue[0].Body["metadata"].(map[string]interface{})
	c.batcher.mu.Unlock()
	if queuedMeta == nil {
		t.Fatal("expected metadata in queued create event")
	}

	if err := tr.Update(TraceParams{Metadata: map[string]interface{}{"b": 2}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, leaked := queuedMeta["b"]; leaked {
		t.Fatal("BUG: Update mutated metadata of an already-enqueued event (shared map) — races the async marshal and panics in production")
	}
}

// TestConcurrentTraceUpdateAndMarshalNoRace marshals a queued create event while
// the trace is being Updated, mirroring the batcher flush goroutine racing a turn
// finalize. Run with -race: without the fix this is a concurrent map read/write.
func TestConcurrentTraceUpdateAndMarshalNoRace(t *testing.T) {
	c := newQueueOnlyClient(t)

	tr, err := c.CreateTrace(TraceParams{Metadata: map[string]interface{}{"k0": 0}})
	if err != nil {
		t.Fatalf("CreateTrace: %v", err)
	}

	c.batcher.mu.Lock()
	createEvent := c.batcher.queue[0]
	c.batcher.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			if _, err := json.Marshal(createEvent.Body); err != nil {
				t.Errorf("marshal: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			_ = tr.Update(TraceParams{Metadata: map[string]interface{}{"k" + strconv.Itoa(i): i}})
		}
	}()
	wg.Wait()
}
