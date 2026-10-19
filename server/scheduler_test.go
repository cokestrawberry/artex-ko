package server

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// schedulerFixture returns a Scheduler on the test database with no Server: any
// attempted trigger run dereferences the nil server and fails the test at once.
func schedulerFixture(t *testing.T) *Scheduler {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { m.Close() })
	return &Scheduler{pg: m.pg}
}

// preserveSchedState restores the given scheduler_state keys after the test.
// Seeded values are never empty, so "" means the key was absent.
func preserveSchedState(t *testing.T, pg *db.DB, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, err := pg.GetSchedState(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if value == "" {
				pg.Exec(`DELETE FROM scheduler_state WHERE key=$1`, key)
			} else {
				pg.SetSchedState(key, value)
			}
		})
	}
}

func createSchedulerTask(t *testing.T, pg *db.DB, description string) int64 {
	t.Helper()
	task, err := pg.CreateTaskWithOptions(description, "scheduler timeout", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.DeleteTask(task.ID) })
	return task.ID
}

func setTaskStatus(t *testing.T, pg *db.DB, id int64, status string) {
	t.Helper()
	if err := pg.SetStatus(id, status); err != nil {
		t.Fatal(err)
	}
}

// firedTimeoutCount counts the recorded timeout occurrences of taskID.
func firedTimeoutCount(sc *Scheduler, taskID int64) int {
	n := 0
	for key := range readStateSet[timeoutKey](sc, schedKeyFiredTimeouts) {
		if key.TaskID == taskID {
			n++
		}
	}
	return n
}

var timeoutWatchers = []*db.AgentTrigger{{OnTaskTimeout: true, AgentKey: "timeout-watcher"}}

func TestSchedulerTaskTimeoutsIgnoreCreationOrder(t *testing.T) {
	sc := schedulerFixture(t)
	preserveSchedState(t, sc.pg, schedKeyFiredTimeouts)
	older := createSchedulerTask(t, sc.pg, "older")
	newer := createSchedulerTask(t, sc.pg, "newer")

	setTaskStatus(t, sc.pg, newer, "timeout")
	sc.fireTaskTimeouts(nil)
	if got := firedTimeoutCount(sc, newer); got != 1 {
		t.Fatalf("newer task: %d timeouts recorded, want 1", got)
	}
	if got := firedTimeoutCount(sc, older); got != 0 {
		t.Fatalf("older task recorded before timing out: %d", got)
	}

	// An id watermark would now sit at newer and skip the older task.
	setTaskStatus(t, sc.pg, older, "timeout")
	sc.fireTaskTimeouts(nil)
	if got := firedTimeoutCount(sc, older); got != 1 {
		t.Fatalf("older task timing out later: %d timeouts recorded, want 1", got)
	}
	if got := firedTimeoutCount(sc, newer); got != 1 {
		t.Fatalf("newer task: %d timeouts recorded, want 1", got)
	}
}

func TestSchedulerTaskTimeoutRefiresAfterRerun(t *testing.T) {
	sc := schedulerFixture(t)
	preserveSchedState(t, sc.pg, schedKeyFiredTimeouts)
	id := createSchedulerTask(t, sc.pg, "rerun")

	setTaskStatus(t, sc.pg, id, "timeout")
	sc.fireTaskTimeouts(nil)
	// Every current timeout is recorded, so an active trigger must not fire.
	sc.fireTaskTimeouts(timeoutWatchers)
	if got := firedTimeoutCount(sc, id); got != 1 {
		t.Fatalf("first timeout: %d recorded, want 1", got)
	}

	// Re-running clears completed_at; timing out again stamps a new one.
	setTaskStatus(t, sc.pg, id, "running")
	setTaskStatus(t, sc.pg, id, "timeout")
	sc.fireTaskTimeouts(nil)
	if got := firedTimeoutCount(sc, id); got != 2 {
		t.Fatalf("timeout after re-run: %d recorded, want 2", got)
	}
}

func TestSchedulerInitRetriesAfterFailedRead(t *testing.T) {
	sc := schedulerFixture(t)
	keys := []string{schedKeyLastFinding, schedKeyFiredGoals, schedKeyFiredTimeouts, schedKeyLastToolCall, schedKeyLastTaskCreate}
	preserveSchedState(t, sc.pg, keys...)
	for _, key := range keys {
		if _, err := sc.pg.Exec(`DELETE FROM scheduler_state WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	closed, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()

	if (&Scheduler{pg: closed}).init() {
		t.Fatal("init succeeded on a closed database")
	}
	for _, key := range keys {
		if v := sc.mustState(key); v != "" {
			t.Fatalf("failed init persisted %s=%q", key, v)
		}
	}
	if !sc.init() {
		t.Fatal("init failed on a reachable database")
	}
	for _, key := range keys {
		if sc.mustState(key) == "" {
			t.Fatalf("init left %s unseeded", key)
		}
	}
}
