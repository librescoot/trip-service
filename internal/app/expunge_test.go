package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/counter"
	"github.com/librescoot/trip-service/internal/db"
	"github.com/librescoot/trip-service/internal/recorder"
)

type expungeTestPublisher struct{}

func (expungeTestPublisher) PublishTripStatus(string, string, string, int64, int64)     {}
func (expungeTestPublisher) PublishTripCompleted(string, string, int64, int64, float64) {}
func (expungeTestPublisher) ClearTrip()                                                 {}

func openExpungeApp(t *testing.T) *App {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rec := recorder.New(store, expungeTestPublisher{})
	a := &App{store: store, recorder: rec, expungePolicy: db.ExpungePolicy{Kind: "count", Value: 0}}
	rec.SetOnFinished(a.runExpunge)
	return a
}

func TestExpungeStartupHydrationFailsClosed(t *testing.T) {
	for name, test := range map[string]struct {
		settings    map[string]string
		settingsErr error
	}{
		"read failure":  {settingsErr: fmt.Errorf("unavailable")},
		"invalid value": {settings: map[string]string{"trip.expunge": "age:0d"}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trips.db")
			store, err := db.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO trips(id,profile_id,status,started_at,ended_at,created_at) VALUES('old','','completed',1,1,1)`); err != nil {
				raw.Close()
				t.Fatal(err)
			}
			defer raw.Close()
			rec := recorder.New(store, expungeTestPublisher{})
			a := &App{store: store, recorder: rec, expungePolicy: db.ExpungePolicy{Kind: "never"}}
			a.hydrateExpungePolicy(test.settings, test.settingsErr)
			a.runExpunge()
			if a.expungePolicy.Kind != "never" || a.expungeLastError == "" {
				t.Fatalf("unsafe startup policy: %+v error=%q", a.expungePolicy, a.expungeLastError)
			}
			var count int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM trips WHERE id='old'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("fail-closed startup deleted old history: count=%d err=%v", count, err)
			}
		})
	}
	a := &App{expungePolicy: db.ExpungePolicy{Kind: "never"}}
	a.hydrateExpungePolicy(map[string]string{}, nil)
	if a.expungePolicy.Kind != "age" {
		t.Fatalf("absent setting did not select default: %+v", a.expungePolicy)
	}
}

func TestInvalidExpungePolicyRetainsLastValidPolicy(t *testing.T) {
	a := &App{expungePolicy: db.DefaultExpungePolicy()}
	if err := a.setExpungePolicy("count:3"); err != nil {
		t.Fatal(err)
	}
	want := a.expungePolicy
	if err := a.setExpungePolicy("count:03"); err == nil {
		t.Fatal("invalid policy unexpectedly succeeded")
	}
	if a.expungePolicy != want || a.expungeLastError == "" {
		t.Fatalf("invalid policy did not retain valid policy: policy=%+v error=%q", a.expungePolicy, a.expungeLastError)
	}
}

func TestCanonicalRideLifecycleKeepsOneOpenHistoryEntry(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pub := &lifecyclePublisher{}
	rec := recorder.New(store, pub)
	ctr := counter.New(store)
	if err := ctr.Hydrate(100, true); err != nil {
		t.Fatal(err)
	}
	a := &App{store: store, recorder: rec, counter: ctr, log: slog.Default(), lastOdo: 100, odoKnown: true}
	for _, state := range []string{"parked", "ready-to-drive", "parked", "hop-on", "parked", "ready-to-drive"} {
		if err := a.handleVehicleState(state); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
	open, err := store.GetRecordingTrip()
	if err != nil || open == nil {
		t.Fatalf("open ride = %+v, %v", open, err)
	}
	// Move past the plausibility floor so the lifecycle closes a real ride.
	if err := a.handleOdometer(500); err != nil {
		t.Fatal(err)
	}
	if err := a.handleVehicleState("stand-by"); err != nil {
		t.Fatal(err)
	}
	if pub.completed != 1 {
		t.Fatalf("completion events = %d, want one", pub.completed)
	}
	trips, err := store.ListTrips("", 10, 0)
	if err != nil || len(trips) != 1 || trips[0].Status != "completed" {
		t.Fatalf("history = %+v, %v", trips, err)
	}
	if err := a.handleVehicleState("stand-by"); err != nil {
		t.Fatal(err)
	}
	if pub.completed != 1 {
		t.Fatalf("repeated lock published %d events", pub.completed)
	}
}

type lifecyclePublisher struct{ completed int }

func (p *lifecyclePublisher) PublishTripStatus(string, string, string, int64, int64) {}
func (p *lifecyclePublisher) PublishTripCompleted(string, string, int64, int64, float64) {
	p.completed++
}
func (p *lifecyclePublisher) ClearTrip() {}

func TestReadyTripWaitsForFirstValidOdometer(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec := recorder.New(store, expungeTestPublisher{})
	ctr := counter.New(store)
	if err := ctr.Hydrate(0, false); err != nil {
		t.Fatal(err)
	}
	a := &App{store: store, recorder: rec, counter: ctr, log: slog.Default()}
	if err := a.handleVehicleState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	if rec.IsRecording() || !a.rideStartPending {
		t.Fatal("trip started without a valid odometer baseline")
	}
	if err := a.handleOdometer(1234); err != nil {
		t.Fatal(err)
	}
	trip, err := store.GetRecordingTrip()
	if err != nil || trip == nil || trip.StartOdometer != 1234 {
		t.Fatalf("deferred trip = %+v, %v; want start odometer 1234", trip, err)
	}
}

func TestCommandDeadlineIsShortLived(t *testing.T) {
	now := int64(1_700_000_000_000)
	for _, deadline := range []int64{now, now - 1} {
		if !expiredCommandDeadline(deadline, now) {
			t.Errorf("deadline %d should be classified as expired", deadline)
		}
	}
	for _, deadline := range []int64{0, now + 1, now + 60_001} {
		if expiredCommandDeadline(deadline, now) {
			t.Errorf("deadline %d unexpectedly classified as expired", deadline)
		}
	}
	for _, deadline := range []int64{0, now, now - 1, now + 60_001} {
		if validCommandDeadline(deadline, now) {
			t.Errorf("deadline %d unexpectedly valid", deadline)
		}
	}
	for _, deadline := range []int64{now + 1, now + 15_000, now + 60_000} {
		if !validCommandDeadline(deadline, now) {
			t.Errorf("deadline %d unexpectedly invalid", deadline)
		}
	}
}

func TestReadyStatePreventsLegacyVacuumAdmission(t *testing.T) {
	a := &App{vacuumVehicleReady: true}
	called := false
	a.migrateVacuum = func(context.Context) error {
		called = true
		return nil
	}
	if err := a.migrateLegacyVacuum(); err != context.Canceled {
		t.Fatalf("migration result = %v, want cancellation", err)
	}
	if called {
		t.Fatal("legacy vacuum started after Ready transition was admitted")
	}
}

func TestReadyTransitionCancelsLegacyVacuumWithoutWaiting(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rec := recorder.New(store, expungeTestPublisher{})
	ctr := counter.New(store)
	if err := ctr.Hydrate(0, true); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	a := &App{store: store, recorder: rec, counter: ctr, log: slog.Default(), odoKnown: true, migrateVacuum: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	go func() { finished <- a.migrateLegacyVacuum() }()
	<-started
	begin := time.Now()
	if err := a.handleVehicleState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("ride start waited for cancelled vacuum: %s", elapsed)
	}
	if err := <-finished; err != context.Canceled {
		t.Fatalf("migration result = %v, want cancellation", err)
	}
	if !rec.IsRecording() {
		t.Fatal("ready transition did not start the ride")
	}
}

func TestExpungeStartupPeriodicAndCompletionSeams(t *testing.T) {
	a := openExpungeApp(t)
	first, err := a.store.CreateTrip("p", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CompleteTrip(first.ID, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	a.runExpunge() // startup seam
	if _, err := a.store.GetTrip(first.ID); err == nil {
		t.Fatal("startup expunge did not delete completed trip")
	}

	second, err := a.store.CreateTrip("p", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CompleteTrip(second.ID, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	a.runExpunge() // periodic idle seam
	if _, err := a.store.GetTrip(second.ID); err == nil {
		t.Fatal("periodic expunge did not delete completed trip")
	}

	if err := a.recorder.StartTrip("p", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.recorder.EndTrip(0, 0, 1000); err != nil {
		t.Fatal(err)
	}
	if trip, err := a.store.GetRecordingTrip(); err != nil || trip != nil {
		t.Fatalf("completion retention seam left a recording trip: %+v, %v", trip, err)
	}
	trips, err := a.store.ListTrips("", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 0 {
		t.Fatalf("completion expunge did not delete retained history: %+v", trips)
	}
}
