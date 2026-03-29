package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type Trip struct {
	ID            string
	ProfileID     string
	Status        string
	StartedAt     int64
	EndedAt       int64
	StartLat      float64
	StartLon      float64
	EndLat        float64
	EndLon        float64
	StartOdometer int64
	EndOdometer   int64
	DistanceM     int64
	DurationS     int64
	AvgSpeed      float64
	MaxSpeed      float64
	PointCount    int64
	CreatedAt     int64
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) CreateTrip(profileID string, lat, lon float64, odometer int64) (*Trip, error) {
	now := time.Now().Unix()
	t := &Trip{
		ID:            newID(),
		ProfileID:     profileID,
		Status:        "recording",
		StartedAt:     now,
		StartLat:      lat,
		StartLon:      lon,
		StartOdometer: odometer,
		CreatedAt:     now,
	}

	_, err := s.db.Exec(
		`INSERT INTO trips (id, profile_id, status, started_at, start_lat, start_lon, start_odometer, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProfileID, t.Status, t.StartedAt, t.StartLat, t.StartLon, t.StartOdometer, t.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert trip: %w", err)
	}

	return t, nil
}

func (s *Store) CompleteTrip(id string, endLat, endLon float64, endOdometer int64, maxSpeed float64) (*Trip, error) {
	t, err := s.GetTrip(id)
	if err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	t.Status = "completed"
	t.EndedAt = now
	t.EndLat = endLat
	t.EndLon = endLon
	t.EndOdometer = endOdometer
	t.MaxSpeed = maxSpeed

	t.DistanceM = endOdometer - t.StartOdometer
	if t.DistanceM < 0 {
		t.DistanceM = 0
	}

	t.DurationS = now - t.StartedAt
	if t.DurationS < 0 {
		t.DurationS = 0
	}

	if t.DurationS > 0 {
		t.AvgSpeed = float64(t.DistanceM) / float64(t.DurationS)
	}

	_, err = s.db.Exec(
		`UPDATE trips SET status = ?, ended_at = ?, end_lat = ?, end_lon = ?, end_odometer = ?,
		 distance_m = ?, duration_s = ?, avg_speed = ?, max_speed = ?
		 WHERE id = ?`,
		t.Status, t.EndedAt, t.EndLat, t.EndLon, t.EndOdometer,
		t.DistanceM, t.DurationS, t.AvgSpeed, t.MaxSpeed, t.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("complete trip: %w", err)
	}

	return t, nil
}

func (s *Store) AbandonTrip(id string) error {
	_, err := s.db.Exec(`UPDATE trips SET status = 'abandoned' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("abandon trip: %w", err)
	}
	return nil
}

func (s *Store) GetTrip(id string) (*Trip, error) {
	t := &Trip{}
	var endedAt, endOdometer sql.NullInt64
	var endLat, endLon sql.NullFloat64

	err := s.db.QueryRow(
		`SELECT id, profile_id, status, started_at, ended_at,
		 start_lat, start_lon, end_lat, end_lon,
		 start_odometer, end_odometer,
		 distance_m, duration_s, avg_speed, max_speed, point_count, created_at
		 FROM trips WHERE id = ?`, id,
	).Scan(
		&t.ID, &t.ProfileID, &t.Status, &t.StartedAt, &endedAt,
		&t.StartLat, &t.StartLon, &endLat, &endLon,
		&t.StartOdometer, &endOdometer,
		&t.DistanceM, &t.DurationS, &t.AvgSpeed, &t.MaxSpeed, &t.PointCount, &t.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get trip %s: %w", id, err)
	}

	t.EndedAt = endedAt.Int64
	t.EndLat = endLat.Float64
	t.EndLon = endLon.Float64
	t.EndOdometer = endOdometer.Int64
	return t, nil
}

func (s *Store) GetRecordingTrip() (*Trip, error) {
	t := &Trip{}
	var endedAt, endOdometer sql.NullInt64
	var endLat, endLon sql.NullFloat64

	err := s.db.QueryRow(
		`SELECT id, profile_id, status, started_at, ended_at,
		 start_lat, start_lon, end_lat, end_lon,
		 start_odometer, end_odometer,
		 distance_m, duration_s, avg_speed, max_speed, point_count, created_at
		 FROM trips WHERE status = 'recording' LIMIT 1`,
	).Scan(
		&t.ID, &t.ProfileID, &t.Status, &t.StartedAt, &endedAt,
		&t.StartLat, &t.StartLon, &endLat, &endLon,
		&t.StartOdometer, &endOdometer,
		&t.DistanceM, &t.DurationS, &t.AvgSpeed, &t.MaxSpeed, &t.PointCount, &t.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recording trip: %w", err)
	}

	t.EndedAt = endedAt.Int64
	t.EndLat = endLat.Float64
	t.EndLon = endLon.Float64
	t.EndOdometer = endOdometer.Int64
	return t, nil
}

func (s *Store) ListTrips(profileID string, limit, offset int) ([]Trip, error) {
	var rows *sql.Rows
	var err error

	query := `SELECT id, profile_id, status, started_at, ended_at,
		 start_lat, start_lon, end_lat, end_lon,
		 start_odometer, end_odometer,
		 distance_m, duration_s, avg_speed, max_speed, point_count, created_at
		 FROM trips`

	if profileID != "" {
		query += ` WHERE profile_id = ?`
		query += ` ORDER BY started_at DESC LIMIT ? OFFSET ?`
		rows, err = s.db.Query(query, profileID, limit, offset)
	} else {
		query += ` ORDER BY started_at DESC LIMIT ? OFFSET ?`
		rows, err = s.db.Query(query, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("list trips: %w", err)
	}
	defer rows.Close()

	var trips []Trip
	for rows.Next() {
		var t Trip
		var endedAt, endOdometer sql.NullInt64
		var endLat, endLon sql.NullFloat64

		if err := rows.Scan(
			&t.ID, &t.ProfileID, &t.Status, &t.StartedAt, &endedAt,
			&t.StartLat, &t.StartLon, &endLat, &endLon,
			&t.StartOdometer, &endOdometer,
			&t.DistanceM, &t.DurationS, &t.AvgSpeed, &t.MaxSpeed, &t.PointCount, &t.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan trip: %w", err)
		}

		t.EndedAt = endedAt.Int64
		t.EndLat = endLat.Float64
		t.EndLon = endLon.Float64
		t.EndOdometer = endOdometer.Int64
		trips = append(trips, t)
	}
	return trips, nil
}

func (s *Store) UpdateTripStats(id string, pointCount int64, maxSpeed float64) error {
	_, err := s.db.Exec(
		`UPDATE trips SET point_count = ?, max_speed = ? WHERE id = ?`,
		pointCount, maxSpeed, id,
	)
	if err != nil {
		return fmt.Errorf("update trip stats: %w", err)
	}
	return nil
}
