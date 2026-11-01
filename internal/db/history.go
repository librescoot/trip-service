package db

import (
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidCursor = errors.New("invalid cursor")
var ErrHistoryNotFound = errors.New("trip not found")

type HistoryTrip struct {
	ID                     string   `json:"id"`
	ProfileID              string   `json:"profile_id"`
	StartedAtS             int64    `json:"started_at_s"`
	EndedAtS               int64    `json:"ended_at_s"`
	DistanceM              int64    `json:"distance_m"`
	ReadyDurationS         int64    `json:"ready_duration_s"`
	AverageSpeedKmh        *float64 `json:"average_speed_kmh"`
	MaxRecordedGPSSpeedKmh *float64 `json:"max_recorded_gps_speed_kmh"`
	PointCount             int64    `json:"point_count"`
}

type HistoryPage struct {
	Trips      []HistoryTrip `json:"trips"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type historyCursor struct {
	Version  int    `json:"v"`
	Snapshot int64  `json:"s"`
	Before   int64  `json:"b"`
	Profile  string `json:"p,omitempty"`
}

func validTripID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func encodeHistoryCursor(c historyCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseHistoryCursor(raw, profile string) (historyCursor, error) {
	if len(raw) > 512 {
		return historyCursor{}, ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, ErrInvalidCursor
	}
	var c historyCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Profile != profile || c.Snapshot < 1 || c.Before < 1 || c.Before > c.Snapshot {
		return historyCursor{}, ErrInvalidCursor
	}
	return c, nil
}

const historySelect = `t.id, COALESCE(t.profile_id,''), COALESCE(t.started_at,0), COALESCE(t.ended_at,0),
 t.distance_m, t.duration_s, t.avg_speed, (SELECT MAX(p.speed) FROM trip_points p WHERE p.trip_id=t.id),
 (SELECT COUNT(*) FROM trip_points p WHERE p.trip_id=t.id)`

func scanHistory(row interface{ Scan(...any) error }, extra ...any) (HistoryTrip, error) {
	var trip HistoryTrip
	var avg float64
	var max sql.NullFloat64
	args := []any{&trip.ID, &trip.ProfileID, &trip.StartedAtS, &trip.EndedAtS, &trip.DistanceM, &trip.ReadyDurationS, &avg, &max, &trip.PointCount}
	args = append(args, extra...)
	if err := row.Scan(args...); err != nil {
		return trip, err
	}
	if trip.ReadyDurationS > 0 {
		value := avg * 3.6
		trip.AverageSpeedKmh = &value
	}
	if max.Valid {
		trip.MaxRecordedGPSSpeedKmh = &max.Float64
	}
	return trip, nil
}

func (s *Store) ListHistory(profile, rawCursor string, limit int) (HistoryPage, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 50 || len(profile) > 128 {
		return HistoryPage{}, errors.New("invalid request")
	}
	c := historyCursor{Version: 1, Profile: profile}
	if rawCursor == "" {
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM trip_history_order`).Scan(&c.Snapshot); err != nil {
			return HistoryPage{}, err
		}
		c.Before = c.Snapshot + 1
	} else {
		var err error
		c, err = parseHistoryCursor(rawCursor, profile)
		if err != nil {
			return HistoryPage{}, err
		}
	}
	result := HistoryPage{Trips: []HistoryTrip{}}
	if c.Snapshot == 0 {
		return result, nil
	}
	rows, err := s.db.Query(`SELECT `+historySelect+`, h.seq FROM trip_history_order h JOIN trips t ON t.id=h.trip_id
        WHERE t.status='completed' AND h.seq<=? AND h.seq<? AND (?='' OR t.profile_id=?) ORDER BY h.seq DESC LIMIT ?`, c.Snapshot, c.Before, profile, profile, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var lastSeq int64
	for rows.Next() {
		var seq int64
		trip, err := scanHistory(rows, &seq)
		if err != nil {
			return HistoryPage{}, err
		}
		if len(result.Trips) == limit {
			result.NextCursor = encodeHistoryCursor(historyCursor{Version: 1, Snapshot: c.Snapshot, Before: lastSeq, Profile: profile})
			break
		}
		result.Trips = append(result.Trips, trip)
		lastSeq = seq
	}
	return result, rows.Err()
}

func (s *Store) HistoryTrip(id string) (HistoryTrip, error) {
	if !validTripID(id) {
		return HistoryTrip{}, ErrHistoryNotFound
	}
	trip, err := scanHistory(s.db.QueryRow(`SELECT `+historySelect+` FROM trips t WHERE t.id=? AND t.status='completed'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryTrip{}, ErrHistoryNotFound
	}
	if err != nil {
		return HistoryTrip{}, fmt.Errorf("get history trip: %w", err)
	}
	return trip, nil
}

type HistoryPoint struct {
	ID          int64    `json:"id"`
	TimestampMs int64    `json:"timestamp_ms"`
	Latitude    float64  `json:"latitude"`
	Longitude   float64  `json:"longitude"`
	Altitude    *float64 `json:"altitude,omitempty"`
	SpeedKmh    *float64 `json:"speed_kmh,omitempty"`
}
type HistoryPointsPage struct {
	Points      []HistoryPoint `json:"points"`
	NextAfterID int64          `json:"next_after_id,omitempty"`
}

func (s *Store) HistoryPoints(id string, after int64, limit int) (HistoryPointsPage, error) {
	if !validTripID(id) {
		return HistoryPointsPage{}, ErrHistoryNotFound
	}
	if limit == 0 {
		limit = 100
	}
	if after < 0 || limit < 1 || limit > 256 {
		return HistoryPointsPage{}, errors.New("invalid request")
	}
	rows, err := s.db.Query(`SELECT p.id, p.timestamp, p.latitude, p.longitude, p.altitude, p.speed
		FROM trips t LEFT JOIN trip_points p ON p.trip_id=t.id AND p.id>?
		WHERE t.id=? AND t.status='completed' ORDER BY p.id LIMIT ?`, after, id, limit+1)
	if err != nil {
		return HistoryPointsPage{}, err
	}
	defer rows.Close()
	page := HistoryPointsPage{Points: []HistoryPoint{}}
	found := false
	for rows.Next() {
		found = true
		var pointID, timestamp sql.NullInt64
		var latitude, longitude, altitude, speed sql.NullFloat64
		if err := rows.Scan(&pointID, &timestamp, &latitude, &longitude, &altitude, &speed); err != nil {
			return HistoryPointsPage{}, err
		}
		if !pointID.Valid {
			break
		}
		if len(page.Points) == limit {
			page.NextAfterID = page.Points[len(page.Points)-1].ID
			break
		}
		p := HistoryPoint{ID: pointID.Int64, TimestampMs: timestamp.Int64, Latitude: latitude.Float64, Longitude: longitude.Float64}
		if altitude.Valid {
			p.Altitude = &altitude.Float64
		}
		if speed.Valid {
			p.SpeedKmh = &speed.Float64
		}
		page.Points = append(page.Points, p)
	}
	if err := rows.Err(); err != nil {
		return HistoryPointsPage{}, err
	}
	if !found {
		return HistoryPointsPage{}, ErrHistoryNotFound
	}
	return page, nil
}
