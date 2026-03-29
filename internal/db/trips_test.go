package db

import (
	"testing"
)

func TestCreateTrip(t *testing.T) {
	store := openTestDB(t)

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	if trip.ID == "" {
		t.Error("trip ID is empty")
	}
	if len(trip.ID) != 32 {
		t.Errorf("trip ID length = %d, want 32", len(trip.ID))
	}
	if trip.ProfileID != "profile-1" {
		t.Errorf("ProfileID = %q, want %q", trip.ProfileID, "profile-1")
	}
	if trip.Status != "recording" {
		t.Errorf("Status = %q, want %q", trip.Status, "recording")
	}
	if trip.StartLat != 52.520 {
		t.Errorf("StartLat = %f, want 52.520", trip.StartLat)
	}
	if trip.StartLon != 13.405 {
		t.Errorf("StartLon = %f, want 13.405", trip.StartLon)
	}
	if trip.StartOdometer != 10000 {
		t.Errorf("StartOdometer = %d, want 10000", trip.StartOdometer)
	}
}

func TestCompleteTrip(t *testing.T) {
	store := openTestDB(t)

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	completed, err := store.CompleteTrip(trip.ID, 52.516, 13.377, 12500, 15.5)
	if err != nil {
		t.Fatalf("CompleteTrip error: %v", err)
	}

	if completed.Status != "completed" {
		t.Errorf("Status = %q, want %q", completed.Status, "completed")
	}
	if completed.DistanceM != 2500 {
		t.Errorf("DistanceM = %d, want 2500", completed.DistanceM)
	}
	if completed.MaxSpeed != 15.5 {
		t.Errorf("MaxSpeed = %f, want 15.5", completed.MaxSpeed)
	}
	if completed.EndLat != 52.516 {
		t.Errorf("EndLat = %f, want 52.516", completed.EndLat)
	}
	if completed.EndOdometer != 12500 {
		t.Errorf("EndOdometer = %d, want 12500", completed.EndOdometer)
	}
	if completed.DurationS < 0 {
		t.Errorf("DurationS = %d, want >= 0", completed.DurationS)
	}
	if completed.EndedAt < completed.StartedAt {
		t.Errorf("EndedAt (%d) < StartedAt (%d)", completed.EndedAt, completed.StartedAt)
	}
}

func TestAbandonTrip(t *testing.T) {
	store := openTestDB(t)

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	if err := store.AbandonTrip(trip.ID); err != nil {
		t.Fatalf("AbandonTrip error: %v", err)
	}

	got, err := store.GetTrip(trip.ID)
	if err != nil {
		t.Fatalf("GetTrip error: %v", err)
	}
	if got.Status != "abandoned" {
		t.Errorf("Status = %q, want %q", got.Status, "abandoned")
	}
}

func TestGetRecordingTrip(t *testing.T) {
	store := openTestDB(t)

	got, err := store.GetRecordingTrip()
	if err != nil {
		t.Fatalf("GetRecordingTrip error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty DB, got %+v", got)
	}

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	got, err = store.GetRecordingTrip()
	if err != nil {
		t.Fatalf("GetRecordingTrip error: %v", err)
	}
	if got == nil {
		t.Fatal("expected recording trip, got nil")
	}
	if got.ID != trip.ID {
		t.Errorf("ID = %q, want %q", got.ID, trip.ID)
	}
}

func TestListTrips(t *testing.T) {
	store := openTestDB(t)

	store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	store.CreateTrip("profile-1", 52.516, 13.377, 12000)
	store.CreateTrip("profile-2", 48.856, 2.352, 5000)

	all, err := store.ListTrips("", 100, 0)
	if err != nil {
		t.Fatalf("ListTrips all error: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all trips count = %d, want 3", len(all))
	}

	filtered, err := store.ListTrips("profile-1", 100, 0)
	if err != nil {
		t.Fatalf("ListTrips filtered error: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("profile-1 trips count = %d, want 2", len(filtered))
	}

	limited, err := store.ListTrips("", 1, 0)
	if err != nil {
		t.Fatalf("ListTrips limited error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limited trips count = %d, want 1", len(limited))
	}
}

func TestUpdateTripStats(t *testing.T) {
	store := openTestDB(t)

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	if err := store.UpdateTripStats(trip.ID, 150, 22.3); err != nil {
		t.Fatalf("UpdateTripStats error: %v", err)
	}

	got, err := store.GetTrip(trip.ID)
	if err != nil {
		t.Fatalf("GetTrip error: %v", err)
	}
	if got.PointCount != 150 {
		t.Errorf("PointCount = %d, want 150", got.PointCount)
	}
	if got.MaxSpeed != 22.3 {
		t.Errorf("MaxSpeed = %f, want 22.3", got.MaxSpeed)
	}
}
