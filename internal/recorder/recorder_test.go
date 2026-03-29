package recorder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/db"
)

type fakePublisher struct {
	tripStatus  string
	tripID      string
	completedID string
}

func (f *fakePublisher) PublishTripStatus(status, id, profileID string, distanceM, durationS int64) {
	f.tripStatus = status
	f.tripID = id
}
func (f *fakePublisher) PublishTripCompleted(id string, profileID string, distanceM, durationS int64, maxSpeed float64) {
	f.completedID = id
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
	if err := rec2.RecoverFromCrash(false); err != nil {
		t.Fatalf("error: %v", err)
	}

	trip, _ := store.GetTrip(tripID)
	if trip.Status != "abandoned" {
		t.Errorf("status = %q, want abandoned", trip.Status)
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
