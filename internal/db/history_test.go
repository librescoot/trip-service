package db

import (
	"errors"
	"testing"
)

func completedHistoryTrip(t *testing.T, s *Store, profile string) *Trip {
	t.Helper()
	trip, err := s.CreateTrip(profile, 1, 2, 1000)
	if err != nil {
		t.Fatal(err)
	}
	trip, err = s.CompleteTripAt(trip.ID, 1, 2, 2200, 30, trip.StartedAt+120)
	if err != nil {
		t.Fatal(err)
	}
	return trip
}

func TestHistoryPaginationStableDuringInsertAndDelete(t *testing.T) {
	s := openTestDB(t)
	first := completedHistoryTrip(t, s, "a")
	middle := completedHistoryTrip(t, s, "a")
	newest := completedHistoryTrip(t, s, "a")
	page, err := s.ListHistory("", "", 1)
	if err != nil || len(page.Trips) != 1 || page.Trips[0].ID != newest.ID || page.NextCursor == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	newer := completedHistoryTrip(t, s, "a")
	if err := s.DeleteTrip(middle.ID); err != nil {
		t.Fatal(err)
	}
	page2, err := s.ListHistory("", page.NextCursor, 1)
	if err != nil || len(page2.Trips) != 1 || page2.Trips[0].ID != first.ID || page2.NextCursor != "" {
		t.Fatalf("second page: %+v %v", page2, err)
	}
	fresh, err := s.ListHistory("", "", 50)
	if err != nil || fresh.Trips[0].ID != newer.ID {
		t.Fatalf("fresh: %+v %v", fresh, err)
	}
	if _, err := s.ListHistory("b", page.NextCursor, 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("wrong filter: %v", err)
	}
	if _, err := s.HistoryTrip(middle.ID); !errors.Is(err, ErrHistoryNotFound) {
		t.Fatalf("deleted trip: %v", err)
	}
	if _, err := s.ListHistory("", "garbage", 1); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestHistoryMigrationKeepsCompletedRowsAndNeverReusesSequence(t *testing.T) {
	s := openTestDB(t)
	first := completedHistoryTrip(t, s, "a")
	second := completedHistoryTrip(t, s, "a")
	if _, err := s.db.Exec(`DROP TRIGGER trip_history_complete; DROP TABLE trip_history_order`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListHistory("", "", 50)
	if err != nil || len(page.Trips) != 2 {
		t.Fatalf("migration: %+v %v", page, err)
	}
	if err := s.DeleteTrip(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTrip(second.ID); err != nil {
		t.Fatal(err)
	}
	newTrip := completedHistoryTrip(t, s, "a")
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM trip_history_order WHERE trip_id=?`, newTrip.ID).Scan(&seq); err != nil || seq <= 2 {
		t.Fatalf("sequence reused: %d %v", seq, err)
	}
}

func TestHistoryPointsAndUnits(t *testing.T) {
	s := openTestDB(t)
	trip := completedHistoryTrip(t, s, "a")
	if err := s.InsertPoints([]TripPoint{{TripID: trip.ID, Timestamp: 2, Latitude: 1, Longitude: 2}, {TripID: trip.ID, Timestamp: 1, Latitude: 3, Longitude: 4}}); err != nil {
		t.Fatal(err)
	}
	detail, err := s.HistoryTrip(trip.ID)
	if err != nil || detail.PointCount != 2 || detail.AverageSpeedKmh == nil || *detail.AverageSpeedKmh != 36 {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	page, err := s.HistoryPoints(trip.ID, 0, 1)
	if err != nil || len(page.Points) != 1 || page.Points[0].TimestampMs != 2 || page.NextAfterID == 0 {
		t.Fatalf("points: %+v %v", page, err)
	}
	next, err := s.HistoryPoints(trip.ID, page.NextAfterID, 1)
	if err != nil || len(next.Points) != 1 || next.Points[0].TimestampMs != 1 || next.NextAfterID != 0 {
		t.Fatalf("next: %+v %v", next, err)
	}
}
