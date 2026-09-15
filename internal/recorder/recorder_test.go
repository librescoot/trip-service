package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/db"
)

type fakePublisher struct {
	tripStatus      string
	tripID          string
	completedID     string
	completedEvents int
}

func (f *fakePublisher) PublishTripStatus(status, id, profileID string, distanceM, durationS int64) {
	f.tripStatus = status
	f.tripID = id
}
func (f *fakePublisher) PublishTripCompleted(id string, profileID string, distanceM, durationS int64, maxSpeed float64) {
	f.completedID = id
	f.completedEvents++
}
func (f *fakePublisher) ClearTrip() {
	f.tripStatus = "idle"
	f.tripID = ""
}

func setupTestRecorder(t *testing.T) (*Recorder, *fakePublisher, *db.Store) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	pub := &fakePublisher{}
	return New(store, pub), pub, store
}

func TestRecorder_StartTrip(t *testing.T) {
	rec, pub, _ := setupTestRecorder(t)
	err := rec.StartTrip("profile-1", 52.52, 13.40, 10000)
	if err != nil {
		t.Fatalf("StartTrip error: %v", err)
	}
	if !rec.IsRecording() {
		t.Error("expected recording state")
	}
	if pub.tripStatus != "recording" {
		t.Errorf("status = %q, want recording", pub.tripStatus)
	}
}

func TestRecorder_EndTrip(t *testing.T) {
	rec, pub, _ := setupTestRecorder(t)
	rec.StartTrip("p1", 52.52, 13.40, 10000)
	err := rec.EndTrip(52.53, 13.41, 15000)
	if err != nil {
		t.Fatalf("EndTrip error: %v", err)
	}
	if rec.IsRecording() {
		t.Error("expected idle state")
	}
	if pub.completedID == "" {
		t.Error("expected completed event")
	}
}

func TestRecorder_EndTripCallsFinishedHookAfterRecordingStops(t *testing.T) {
	rec, _, _ := setupTestRecorder(t)
	rec.StartTrip("p1", 0, 0, 0)
	called := false
	rec.SetOnFinished(func() {
		called = true
		if rec.IsRecording() {
			t.Error("finished hook ran while recording")
		}
	})
	if err := rec.EndTrip(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("finished hook was not called")
	}
}

func TestRecorder_EndTrip_NotRecording(t *testing.T) {
	rec, _, _ := setupTestRecorder(t)
	if err := rec.EndTrip(0, 0, 0); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRecorder_AddPoint(t *testing.T) {
	rec, _, store := setupTestRecorder(t)
	rec.StartTrip("p1", 52.520, 13.400, 10000)

	rec.AddPoint(52.521, 13.401, 0, 25.0, 90.0, 10050, time.Now().UnixMilli())
	rec.AddPoint(52.522, 13.402, 0, 30.0, 91.0, 10100, time.Now().UnixMilli())
	rec.FlushPoints()

	count, _ := store.PointCount(rec.currentTrip.ID)
	if count != 2 {
		t.Errorf("point count = %d, want 2", count)
	}
}

func TestRecorder_CrashRecovery_Abandon(t *testing.T) {
	rec, _, store := setupTestRecorder(t)
	rec.StartTrip("p1", 0, 0, 0)
	tripID := rec.currentTrip.ID

	rec2 := New(store, &fakePublisher{})
	called := false
	rec2.SetOnFinished(func() { called = true })
	if err := rec2.RecoverFromCrash(false); err != nil {
		t.Fatalf("error: %v", err)
	}

	trip, _ := store.GetTrip(tripID)
	if trip.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned", trip.Status)
	}
	if !called {
		t.Error("abandonment did not call finished hook")
	}
}

func TestRecorder_PauseResumeKeepsOneRideAndSeparatesPoints(t *testing.T) {
	rec, pub, store := setupTestRecorder(t)
	now := time.Now().Truncate(time.Second)
	rec.SetClock(func() time.Time { return now })
	if err := rec.StartTrip("p1", 0, 0, 100); err != nil {
		t.Fatal(err)
	}
	id := rec.currentTrip.ID
	rec.AddPoint(1, 1, 0, 10, 0, 110, 1)
	now = now.Add(10 * time.Second)
	if err := rec.PauseTrip(); err != nil {
		t.Fatal(err)
	}
	if rec.IsCollecting() || pub.tripStatus != "paused" {
		t.Fatalf("pause state collecting=%v status=%q", rec.IsCollecting(), pub.tripStatus)
	}
	rec.AddPoint(2, 2, 0, 10, 0, 120, 2)
	now = now.Add(time.Hour)
	if err := rec.ResumeTrip(); err != nil {
		t.Fatal(err)
	}
	rec.AddPoint(1, 1, 0, 10, 0, 130, 3) // accepted after pause; no adaptive bridge
	now = now.Add(20 * time.Second)
	if err := rec.EndTrip(1, 1, 130); err != nil {
		t.Fatal(err)
	}
	trip, err := store.GetTrip(id)
	if err != nil {
		t.Fatal(err)
	}
	if trip.Status != "completed" || trip.DurationS != 30 {
		t.Fatalf("completed trip = %+v, want one 30-second ride", trip)
	}
	if points, err := store.PointCount(id); err != nil || points != 2 {
		t.Fatalf("points = %d, %v; want two active-only points", points, err)
	}
	if pub.completedEvents != 1 {
		t.Fatalf("completion events = %d, want one", pub.completedEvents)
	}
}

func TestRecorder_CrashRecoveryDoesNotCountReadyOutage(t *testing.T) {
	rec, _, store := setupTestRecorder(t)
	now := time.Now().Truncate(time.Second)
	rec.SetClock(func() time.Time { return now })
	if err := rec.StartTrip("p1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := rec.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// A new recorder sees an active row after an hour-long outage. Recovery
	// must restart the active interval rather than charge the outage to the ride.
	now = now.Add(time.Hour)
	recovered := New(store, &fakePublisher{})
	recovered.SetClock(func() time.Time { return now })
	if err := recovered.RecoverFromState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := recovered.EndTrip(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	trips, err := store.ListTrips("", 1, 0)
	if err != nil || len(trips) != 1 || trips[0].DurationS != 20 {
		t.Fatalf("recovered duration = %+v, %v; want 20 seconds", trips, err)
	}
}

func TestRecorder_CrashRecoveryPreservesPausedRideStates(t *testing.T) {
	for _, state := range []string{"parked", "hop-on", "hop-on-learning"} {
		t.Run(state, func(t *testing.T) {
			rec, _, store := setupTestRecorder(t)
			if err := rec.StartTrip("p1", 0, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := rec.PauseTrip(); err != nil {
				t.Fatal(err)
			}
			recovered := New(store, &fakePublisher{})
			if err := recovered.RecoverFromState(state); err != nil {
				t.Fatal(err)
			}
			if !recovered.IsRecording() || recovered.IsCollecting() {
				t.Fatalf("%s recovery did not retain paused ride", state)
			}
		})
	}
}

func TestRecorder_CrashRecovery_Resume(t *testing.T) {
	rec, _, store := setupTestRecorder(t)
	rec.StartTrip("p1", 0, 0, 0)

	rec2 := New(store, &fakePublisher{})
	if err := rec2.RecoverFromCrash(true); err != nil {
		t.Fatalf("error: %v", err)
	}
	if !rec2.IsRecording() {
		t.Error("expected recording after resume")
	}
}
