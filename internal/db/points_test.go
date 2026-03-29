package db

import (
	"testing"
)

func ptr[T any](v T) *T {
	return &v
}

func TestInsertPoints_Batch(t *testing.T) {
	store := openTestDB(t)

	trip, err := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	if err != nil {
		t.Fatalf("CreateTrip error: %v", err)
	}

	points := []TripPoint{
		{TripID: trip.ID, Timestamp: 1000, Latitude: 52.520, Longitude: 13.405, Speed: ptr(5.0), Odometer: ptr(int64(10010))},
		{TripID: trip.ID, Timestamp: 1001, Latitude: 52.521, Longitude: 13.406, Speed: ptr(8.0), Odometer: ptr(int64(10020))},
		{TripID: trip.ID, Timestamp: 1002, Latitude: 52.522, Longitude: 13.407, Speed: ptr(12.0), Altitude: ptr(35.0), Course: ptr(90.0), Odometer: ptr(int64(10035))},
	}

	if err := store.InsertPoints(points); err != nil {
		t.Fatalf("InsertPoints error: %v", err)
	}

	got, err := store.GetPoints(trip.ID)
	if err != nil {
		t.Fatalf("GetPoints error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("point count = %d, want 3", len(got))
	}

	if got[0].ID == 0 {
		t.Error("first point ID is 0, expected autoincrement")
	}
	if got[2].Altitude == nil || *got[2].Altitude != 35.0 {
		t.Errorf("point[2] altitude = %v, want 35.0", got[2].Altitude)
	}
	if got[2].Course == nil || *got[2].Course != 90.0 {
		t.Errorf("point[2] course = %v, want 90.0", got[2].Course)
	}
}

func TestInsertPoints_Empty(t *testing.T) {
	store := openTestDB(t)

	if err := store.InsertPoints(nil); err != nil {
		t.Fatalf("InsertPoints(nil) error: %v", err)
	}
	if err := store.InsertPoints([]TripPoint{}); err != nil {
		t.Fatalf("InsertPoints([]) error: %v", err)
	}
}

func TestGetPoints_OrderedByTimestamp(t *testing.T) {
	store := openTestDB(t)

	trip, _ := store.CreateTrip("profile-1", 52.520, 13.405, 10000)

	points := []TripPoint{
		{TripID: trip.ID, Timestamp: 1003, Latitude: 52.523, Longitude: 13.408},
		{TripID: trip.ID, Timestamp: 1001, Latitude: 52.521, Longitude: 13.406},
		{TripID: trip.ID, Timestamp: 1002, Latitude: 52.522, Longitude: 13.407},
	}
	store.InsertPoints(points)

	got, err := store.GetPoints(trip.ID)
	if err != nil {
		t.Fatalf("GetPoints error: %v", err)
	}

	for i := 1; i < len(got); i++ {
		if got[i].Timestamp < got[i-1].Timestamp {
			t.Errorf("points not ordered: [%d].Timestamp=%d < [%d].Timestamp=%d",
				i, got[i].Timestamp, i-1, got[i-1].Timestamp)
		}
	}
}

func TestPoints_CascadeDeleteWithTrip(t *testing.T) {
	store := openTestDB(t)

	trip, _ := store.CreateTrip("profile-1", 52.520, 13.405, 10000)
	store.InsertPoints([]TripPoint{
		{TripID: trip.ID, Timestamp: 1000, Latitude: 52.520, Longitude: 13.405},
		{TripID: trip.ID, Timestamp: 1001, Latitude: 52.521, Longitude: 13.406},
	})

	count, err := store.PointCount(trip.ID)
	if err != nil {
		t.Fatalf("PointCount error: %v", err)
	}
	if count != 2 {
		t.Fatalf("PointCount = %d, want 2", count)
	}

	_, err = store.db.Exec(`DELETE FROM trips WHERE id = ?`, trip.ID)
	if err != nil {
		t.Fatalf("delete trip error: %v", err)
	}

	count, err = store.PointCount(trip.ID)
	if err != nil {
		t.Fatalf("PointCount after delete error: %v", err)
	}
	if count != 0 {
		t.Errorf("PointCount after cascade = %d, want 0", count)
	}
}

func TestPointCount(t *testing.T) {
	store := openTestDB(t)

	trip, _ := store.CreateTrip("profile-1", 52.520, 13.405, 10000)

	count, _ := store.PointCount(trip.ID)
	if count != 0 {
		t.Errorf("empty PointCount = %d, want 0", count)
	}

	store.InsertPoints([]TripPoint{
		{TripID: trip.ID, Timestamp: 1000, Latitude: 52.520, Longitude: 13.405},
		{TripID: trip.ID, Timestamp: 1001, Latitude: 52.521, Longitude: 13.406},
		{TripID: trip.ID, Timestamp: 1002, Latitude: 52.522, Longitude: 13.407},
	})

	count, err := store.PointCount(trip.ID)
	if err != nil {
		t.Fatalf("PointCount error: %v", err)
	}
	if count != 3 {
		t.Errorf("PointCount = %d, want 3", count)
	}
}
