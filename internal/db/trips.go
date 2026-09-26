package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type Trip struct {
	ID              string
	ProfileID       string
	Status          string
	StartedAt       int64
	EndedAt         int64
	ActiveStartedAt int64
	StartLat        float64
	StartLon        float64
	EndLat          float64
	EndLon          float64
	StartOdometer   int64
	EndOdometer     int64
	DistanceM       int64
	DurationS       int64
	AvgSpeed        float64
	MaxSpeed        float64
	PointCount      int64
	CreatedAt       int64
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) CreateTrip(profileID string, lat, lon float64, odometer int64) (*Trip, error) {
	now := time.Now().Unix()
	t := &Trip{ID: newID(), ProfileID: profileID, Status: "recording", StartedAt: now, ActiveStartedAt: now, StartLat: lat, StartLon: lon, StartOdometer: odometer, CreatedAt: now}
	_, err := s.db.Exec(`INSERT INTO trips (id, profile_id, status, started_at, active_started_at, start_lat, start_lon, start_odometer, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, t.ProfileID, t.Status, t.StartedAt, t.ActiveStartedAt, t.StartLat, t.StartLon, t.StartOdometer, t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert trip: %w", err)
	}
	return t, nil
}

// PauseTrip makes elapsed ready-to-drive time durable and leaves the open row
// protected from retention while the unlocked ride is paused.
func (s *Store) PauseTrip(id string, now int64) (*Trip, error) {
	if _, err := s.db.Exec(`UPDATE trips SET duration_s=duration_s + CASE WHEN active_started_at > 0 AND ? > active_started_at THEN ? - active_started_at ELSE 0 END,
		active_started_at=0 WHERE id=? AND status='recording'`, now, now, id); err != nil {
		return nil, fmt.Errorf("pause trip: %w", err)
	}
	return s.GetTrip(id)
}

// DiscardActiveTripInterval closes an uncheckpointed interval without adding
// it to duration. Recovery uses it to exclude the unobserved outage.
func (s *Store) DiscardActiveTripInterval(id string) (*Trip, error) {
	if _, err := s.db.Exec(`UPDATE trips SET active_started_at=0 WHERE id=? AND status='recording'`, id); err != nil {
		return nil, fmt.Errorf("discard trip interval: %w", err)
	}
	return s.GetTrip(id)
}

// ResumeTrip starts a new observed ready interval. It deliberately never
// counts the time between a crash and recovery.
func (s *Store) ResumeTrip(id string, now int64) (*Trip, error) {
	if _, err := s.db.Exec(`UPDATE trips SET active_started_at=CASE WHEN active_started_at=0 THEN ? ELSE active_started_at END
		WHERE id=? AND status='recording'`, now, id); err != nil {
		return nil, fmt.Errorf("resume trip: %w", err)
	}
	return s.GetTrip(id)
}

// CheckpointTrip retains the active state while persisting observed duration.
func (s *Store) CheckpointTrip(id string, now int64) (*Trip, error) {
	if _, err := s.db.Exec(`UPDATE trips SET duration_s=duration_s + CASE WHEN active_started_at > 0 AND ? > active_started_at THEN ? - active_started_at ELSE 0 END,
		active_started_at=CASE WHEN active_started_at > 0 THEN ? ELSE 0 END WHERE id=? AND status='recording'`, now, now, now, id); err != nil {
		return nil, fmt.Errorf("checkpoint trip: %w", err)
	}
	return s.GetTrip(id)
}

func (s *Store) CompleteTrip(id string, endLat, endLon float64, endOdometer int64, maxSpeed float64) (*Trip, error) {
	return s.CompleteTripAt(id, endLat, endLon, endOdometer, maxSpeed, time.Now().Unix())
}

func (s *Store) CompleteTripAt(id string, endLat, endLon float64, endOdometer int64, maxSpeed float64, now int64) (*Trip, error) {
	if _, err := s.db.Exec(`UPDATE trips SET duration_s=duration_s + CASE WHEN active_started_at > 0 AND ? > active_started_at THEN ? - active_started_at ELSE 0 END,
		active_started_at=0 WHERE id=? AND status='recording'`, now, now, id); err != nil {
		return nil, fmt.Errorf("finish trip duration: %w", err)
	}
	t, err := s.GetTrip(id)
	if err != nil {
		return nil, err
	}
	t.Status, t.EndedAt, t.EndLat, t.EndLon, t.EndOdometer, t.MaxSpeed = "completed", now, endLat, endLon, endOdometer, maxSpeed
	t.DistanceM = endOdometer - t.StartOdometer
	if t.DistanceM < 0 {
		t.DistanceM = 0
	}
	if t.DurationS > 0 {
		t.AvgSpeed = float64(t.DistanceM) / float64(t.DurationS)
	}
	_, err = s.db.Exec(`UPDATE trips SET status=?, ended_at=?, end_lat=?, end_lon=?, end_odometer=?, distance_m=?, duration_s=?, avg_speed=?, max_speed=? WHERE id=?`,
		t.Status, t.EndedAt, t.EndLat, t.EndLon, t.EndOdometer, t.DistanceM, t.DurationS, t.AvgSpeed, t.MaxSpeed, t.ID)
	if err != nil {
		return nil, fmt.Errorf("complete trip: %w", err)
	}
	return t, nil
}

func (s *Store) AbandonTrip(id string) error {
	_, err := s.db.Exec(`UPDATE trips SET status = 'abandoned', active_started_at=0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("abandon trip: %w", err)
	}
	return nil
}

// DeleteTrip removes a history row and its points (cascade) for rides that
// failed the plausibility filter before they were ever completed.
func (s *Store) DeleteTrip(id string) error {
	_, err := s.db.Exec(`DELETE FROM trips WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete trip: %w", err)
	}
	return nil
}

const tripColumns = `id, profile_id, status, started_at, ended_at, active_started_at,
	start_lat, start_lon, end_lat, end_lon, start_odometer, end_odometer,
	distance_m, duration_s, avg_speed, max_speed, point_count, created_at`

func scanTrip(row interface{ Scan(...any) error }) (*Trip, error) {
	t := &Trip{}
	var endedAt, endOdometer sql.NullInt64
	var endLat, endLon sql.NullFloat64
	err := row.Scan(&t.ID, &t.ProfileID, &t.Status, &t.StartedAt, &endedAt, &t.ActiveStartedAt,
		&t.StartLat, &t.StartLon, &endLat, &endLon, &t.StartOdometer, &endOdometer,
		&t.DistanceM, &t.DurationS, &t.AvgSpeed, &t.MaxSpeed, &t.PointCount, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.EndedAt, t.EndLat, t.EndLon, t.EndOdometer = endedAt.Int64, endLat.Float64, endLon.Float64, endOdometer.Int64
	return t, nil
}

func (s *Store) GetTrip(id string) (*Trip, error) {
	t, err := scanTrip(s.db.QueryRow(`SELECT `+tripColumns+` FROM trips WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get trip %s: %w", id, err)
	}
	return t, nil
}

func (s *Store) GetRecordingTrip() (*Trip, error) {
	t, err := scanTrip(s.db.QueryRow(`SELECT ` + tripColumns + ` FROM trips WHERE status = 'recording' LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recording trip: %w", err)
	}
	return t, nil
}

func (s *Store) ListTrips(profileID string, limit, offset int) ([]Trip, error) {
	query := `SELECT ` + tripColumns + ` FROM trips`
	args := []any{limit, offset}
	if profileID != "" {
		query += ` WHERE profile_id = ?`
		args = []any{profileID, limit, offset}
	}
	query += ` ORDER BY started_at DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list trips: %w", err)
	}
	defer rows.Close()
	var trips []Trip
	for rows.Next() {
		t, err := scanTrip(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trip: %w", err)
		}
		trips = append(trips, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trips: %w", err)
	}
	return trips, nil
}

func (s *Store) UpdateTripStats(id string, pointCount int64, maxSpeed float64) error {
	_, err := s.db.Exec(`UPDATE trips SET point_count = ?, max_speed = ? WHERE id = ?`, pointCount, maxSpeed, id)
	if err != nil {
		return fmt.Errorf("update trip stats: %w", err)
	}
	return nil
}
