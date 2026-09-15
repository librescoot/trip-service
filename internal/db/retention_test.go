package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestParseExpungePolicy(t *testing.T) {
	valid := map[string]string{
		"never": "", "age:365d": "365d", "age:1h30m": "1h30m0s", "count:0": "0", "size:42": "42",
	}
	for input, operand := range valid {
		policy, err := ParseExpungePolicy(input)
		if err != nil {
			t.Errorf("ParseExpungePolicy(%q): %v", input, err)
			continue
		}
		if policy.Operand() != operand {
			t.Errorf("ParseExpungePolicy(%q).Operand() = %q, want %q", input, policy.Operand(), operand)
		}
	}
	for _, input := range []string{"", "never ", "age:0", "age:-1h", "age:0d", "age:01d", "count:01", "count:-1", "count:9223372036854775808", "size:1.0", "other:1"} {
		if _, err := ParseExpungePolicy(input); err == nil {
			t.Errorf("ParseExpungePolicy(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseExpungePolicyTransportBoundaryCorpus(t *testing.T) {
	valid := []string{"never", "age:1ns", "age:1us", "age:1.5ms", "age:1h30m", "age:1d", "age:106751d", "count:0", "count:9223372036854775807", "size:0", "size:9223372036854775807"}
	for _, value := range valid {
		if _, err := ParseExpungePolicy(value); err != nil {
			t.Errorf("valid policy %q: %v", value, err)
		}
	}
	invalid := []string{"age:0", "age:0ns", "age:0.5ns", "age:1µs", "age:1μs", "age:106752d", "age:2562047h47m16.854775808s", "count:01", "count:+1", "count:9223372036854775808", "size:-1", "size:9223372036854775808", " age:1s", "age:1s ", "age:1 s", "count:1\t", "size:\n1"}
	for _, value := range invalid {
		if _, err := ParseExpungePolicy(value); err == nil {
			t.Errorf("invalid policy %q unexpectedly parsed", value)
		}
	}
}

func TestParseExpungePolicyDurationParityEdges(t *testing.T) {
	for _, value := range []string{"age:0.5s", "age:1.0s"} {
		if _, err := ParseExpungePolicy(value); err != nil {
			t.Errorf("valid canonical duration %q: %v", value, err)
		}
	}
	for _, value := range []string{"age:.5s", "age:.5h", "age:01h", "age:00.5s", "age:.s", "age:1.s", "age:.5ns"} {
		if _, err := ParseExpungePolicy(value); err == nil {
			t.Errorf("non-canonical duration %q unexpectedly parsed", value)
		}
	}
}

func TestParseExpungePolicyBoundsIntegerDays(t *testing.T) {
	policy, err := ParseExpungePolicy("age:106751d")
	if err != nil || policy.Operand() != "106751d" {
		t.Fatalf("maximum day policy = %+v, %v", policy, err)
	}
	for _, input := range []string{"age:106752d", "age:999999999999999999999999999999999999d"} {
		if _, err := ParseExpungePolicy(input); err == nil {
			t.Fatalf("ParseExpungePolicy(%q) unexpectedly succeeded", input)
		}
	}
}

func insertRetentionTrip(t *testing.T, store *Store, id, status string, at int64) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO trips(id,status,started_at,ended_at,created_at) VALUES(?,?,?,?,?)`, id, status, at, at, at); err != nil {
		t.Fatal(err)
	}
}

func tripExists(t *testing.T, store *Store, id string) bool {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM trips WHERE id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count != 0
}

func TestExpungeAgeCascadeAndCounterIndependence(t *testing.T) {
	store := openTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	insertRetentionTrip(t, store, "old", "completed", now.Add(-48*time.Hour).Unix())
	insertRetentionTrip(t, store, "new", "completed", now.Add(-time.Hour).Unix())
	insertRetentionTrip(t, store, "active", "recording", now.Add(-48*time.Hour).Unix())
	if _, err := store.db.Exec(`INSERT INTO trip_points(trip_id,timestamp,latitude,longitude) VALUES('old',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureCounter(now.Unix(), 12, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResetCounterCommand("reset", now.Unix(), 12, true, false); err != nil {
		t.Fatal(err)
	}

	result, err := store.Expunge(ExpungePolicy{Kind: "age", Value: int64(24 * time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedTrips != 1 || tripExists(t, store, "old") || !tripExists(t, store, "new") || !tripExists(t, store, "active") {
		t.Fatalf("unexpected retained trips after age expunge: %+v", result)
	}
	if points, err := store.PointCount("old"); err != nil || points != 0 {
		t.Fatalf("cascaded points = %d, %v; want 0, nil", points, err)
	}
	if counter, err := store.Counter(); err != nil || counter == nil || counter.Generation != 1 {
		t.Fatalf("counter changed by history deletion: %+v, %v", counter, err)
	}
	var commands int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM trip_counter_commands`).Scan(&commands); err != nil || commands != 1 {
		t.Fatalf("counter commands changed by history deletion: %d, %v", commands, err)
	}
}

func TestExpungeCountOldestFirstBounded(t *testing.T) {
	store := openTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	for i := 0; i < 105; i++ {
		insertRetentionTrip(t, store, fmt.Sprintf("%03d", i), "completed", now.Add(time.Duration(i)*time.Second).Unix())
	}
	result, err := store.Expunge(ExpungePolicy{Kind: "count", Value: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedTrips != ExpungeBatchSize {
		t.Fatalf("DeletedTrips = %d, want bounded %d", result.DeletedTrips, ExpungeBatchSize)
	}
	for i := 0; i < 100; i++ {
		if tripExists(t, store, fmt.Sprintf("%03d", i)) {
			t.Errorf("old trip %d was not deleted", i)
		}
	}
	for i := 100; i < 105; i++ {
		if !tripExists(t, store, fmt.Sprintf("%03d", i)) {
			t.Errorf("newer trip %d was deleted before older trips", i)
		}
	}
}

func TestExpungeNeverAndInvalidClock(t *testing.T) {
	store := openTestDB(t)
	insertRetentionTrip(t, store, "completed", "completed", 1)
	if result, err := store.Expunge(ExpungePolicy{Kind: "never"}, time.Time{}); err != nil || result.DeletedTrips != 0 || !tripExists(t, store, "completed") {
		t.Fatalf("never policy result=%+v err=%v", result, err)
	}
	if _, err := store.Expunge(ExpungePolicy{Kind: "age", Value: int64(time.Hour)}, time.Time{}); err == nil {
		t.Fatal("age expunge with unavailable clock unexpectedly succeeded")
	}
	if _, err := store.Expunge(ExpungePolicy{Kind: "age", Value: int64(time.Hour)}, time.Unix(1, 0)); err == nil {
		t.Fatal("age expunge with untrusted epoch clock unexpectedly succeeded")
	}
	if !tripExists(t, store, "completed") {
		t.Fatal("invalid clock deleted a trip")
	}
}

func TestSizeExpungeReclaimsBeforeDeletingHistory(t *testing.T) {
	store := openTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	insertRetentionTrip(t, store, "completed", "completed", now.Unix())
	before, err := store.storageBytes()
	if err != nil {
		t.Fatal(err)
	}
	// The target accepts the main database but not its current WAL. A checkpoint
	// alone must therefore avoid deleting the only completed row.
	result, err := store.Expunge(ExpungePolicy{Kind: "size", Value: before.DBBytes}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedTrips != 0 || !tripExists(t, store, "completed") {
		t.Fatalf("reclamation deleted history: %+v", result)
	}
	if result.DBBytes+result.WALBytes > before.DBBytes {
		t.Fatalf("reclamation did not satisfy target: before=%+v after=%+v", before, result)
	}
}

func TestLegacyAutoVacuumMigrationDefersThenEnablesSizeRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`PRAGMA auto_vacuum=NONE; VACUUM; CREATE TABLE trips (id TEXT PRIMARY KEY, profile_id TEXT, status TEXT NOT NULL DEFAULT 'recording', started_at INTEGER, ended_at INTEGER, start_lat REAL, start_lon REAL, end_lat REAL, end_lon REAL, start_odometer INTEGER, end_odometer INTEGER, distance_m INTEGER NOT NULL DEFAULT 0, duration_s INTEGER NOT NULL DEFAULT 0, avg_speed REAL NOT NULL DEFAULT 0, max_speed REAL NOT NULL DEFAULT 0, point_count INTEGER NOT NULL DEFAULT 0, created_at INTEGER); INSERT INTO trips(id,status,started_at,ended_at,created_at) VALUES('old','completed',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	enabled, err := store.IncrementalVacuumEnabled()
	if err != nil || enabled {
		t.Fatalf("legacy mode enabled=%v err=%v", enabled, err)
	}
	if _, err := store.Expunge(ExpungePolicy{Kind: "size", Value: 0}, time.Now()); err != ErrIncrementalVacuumRequired || !tripExists(t, store, "old") {
		t.Fatalf("legacy size expunge was unsafe: err=%v exists=%v", err, tripExists(t, store, "old"))
	}
	if err := store.MigrateToIncrementalVacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	if enabled, err := store.IncrementalVacuumEnabled(); err != nil || !enabled {
		t.Fatalf("migration enabled=%v err=%v", enabled, err)
	}
}

func TestExpungeSizeCheckpointAndActiveSafety(t *testing.T) {
	store := openTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	insertRetentionTrip(t, store, "old", "abandoned", now.Add(-time.Hour).Unix())
	insertRetentionTrip(t, store, "active", "recording", now.Add(-time.Hour).Unix())
	result, err := store.Expunge(ExpungePolicy{Kind: "size", Value: 0}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedTrips != 1 || tripExists(t, store, "old") || !tripExists(t, store, "active") {
		t.Fatalf("size expunge did not preserve active trip: %+v", result)
	}
	if result.DBBytes <= 0 || result.WALBytes < 0 {
		t.Fatalf("invalid storage accounting: %+v", result)
	}
}
