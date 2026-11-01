package manager

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A panic inside processFunc used to unwind the worker goroutine itself —
// with a fixed-size pool, enough panicking jobs silently drained it to zero,
// leaving every subsequently queued job stuck forever with no error and a
// passing healthcheck. runJob's recover must keep the worker alive so later
// jobs still get processed.
func TestWorkerSurvivesPanicInProcessFunc(t *testing.T) {
	var (
		mu        sync.Mutex
		processed []string
	)

	processFunc := func(_ context.Context, job *Job) {
		if job.ID == "panics" {
			panic("boom")
		}
		mu.Lock()
		processed = append(processed, job.ID)
		mu.Unlock()
	}

	q := NewJobQueue(context.Background(), 1, processFunc)
	defer q.Close()

	if err := q.Submit(&Job{ID: "panics"}); err != nil {
		t.Fatalf("Submit(panics) error = %v", err)
	}
	if err := q.Submit(&Job{ID: "after"}); err != nil {
		t.Fatalf("Submit(after) error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := len(processed)
		mu.Unlock()
		if got >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker never processed the job submitted after the panic")
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 1 || processed[0] != "after" {
		t.Fatalf("processed = %v, want [after]", processed)
	}
}

// runJob itself must not propagate the panic to its caller.
func TestRunJobRecoversPanic(t *testing.T) {
	var ran atomic.Bool
	q := &JobQueue{
		logger: zerolog.Nop(),
		processFunc: func(_ context.Context, _ *Job) {
			ran.Store(true)
			panic("boom")
		},
	}

	q.runJob(&Job{ID: "x"})

	if !ran.Load() {
		t.Fatal("processFunc was never invoked")
	}
}

// AddNewNZB only drops a job's parsed NZB when the queue already has a full
// worker pool's worth of jobs waiting; below that the job starts soon and
// re-parsing it in the worker would be wasted NNTP work.
func TestJobQueueBacklogged(t *testing.T) {
	block := make(chan struct{})
	q := NewJobQueue(context.Background(), 2, func(_ context.Context, _ *Job) { <-block })
	defer q.Close()
	defer close(block)

	// Two jobs occupy both workers; wait until neither is still pending.
	_ = q.Submit(&Job{ID: "a"})
	_ = q.Submit(&Job{ID: "b"})
	deadline := time.After(2 * time.Second)
	for q.ActiveCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("workers never picked up the first two jobs")
		case <-time.After(5 * time.Millisecond):
		}
	}

	_ = q.Submit(&Job{ID: "c"})
	if q.Backlogged() {
		t.Fatal("1 pending job with 2 workers should not count as backlogged")
	}
	_ = q.Submit(&Job{ID: "d"})
	if !q.Backlogged() {
		t.Fatal("2 pending jobs with 2 workers should count as backlogged")
	}
}

// Popped and deleted jobs must not stay reachable through the queue's
// backing array: workers attach each job's parsed NZB to it, so a retained
// slot kept every processed NZB in memory for as long as the array lived.
func TestJobQueueReleasesPoppedAndDeletedJobs(t *testing.T) {
	q := &JobQueue{jobs: []*Job{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}}
	q.cond = sync.NewCond(&q.mu)
	backing := q.jobs

	if job := q.pop(); job == nil || job.ID != "a" {
		t.Fatalf("pop() = %#v, want a", job)
	}
	if backing[0] != nil {
		t.Fatal("popped job is still referenced by the backing array")
	}

	if !q.DeleteJob("c") {
		t.Fatal("DeleteJob(c) = false")
	}
	for i, job := range backing {
		if job != nil && job.ID == "c" {
			t.Fatalf("deleted job still referenced at backing[%d]", i)
		}
	}
	if len(q.jobs) != 2 || q.jobs[0].ID != "b" || q.jobs[1].ID != "d" {
		t.Fatalf("remaining jobs = %v, want [b d]", q.jobs)
	}
	if backing[3] != nil {
		t.Fatal("DeleteJob left a duplicate pointer in the vacated tail slot")
	}
}
