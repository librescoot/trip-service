package db

import (
	"fmt"
)

type TripPoint struct {
	ID        int64
	TripID    string
	Timestamp int64
	Latitude  float64
	Longitude float64
	Altitude  *float64
	Speed     *float64
	Course    *float64
	Odometer  *int64
}

func (s *Store) InsertPoints(points []TripPoint) error {
	if len(points) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO trip_points (trip_id, timestamp, latitude, longitude, altitude, speed, course, odometer)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, p := range points {
		if _, err := stmt.Exec(p.TripID, p.Timestamp, p.Latitude, p.Longitude, p.Altitude, p.Speed, p.Course, p.Odometer); err != nil {
			return fmt.Errorf("insert point: %w", err)
		}
	}

	return tx.Commit()
}

func (s *Store) GetPoints(tripID string) ([]TripPoint, error) {
	rows, err := s.db.Query(
		`SELECT id, trip_id, timestamp, latitude, longitude, altitude, speed, course, odometer
		 FROM trip_points WHERE trip_id = ? ORDER BY timestamp ASC`, tripID,
	)
	if err != nil {
		return nil, fmt.Errorf("get points: %w", err)
	}
	defer rows.Close()

	var points []TripPoint
	for rows.Next() {
		var p TripPoint
		if err := rows.Scan(&p.ID, &p.TripID, &p.Timestamp, &p.Latitude, &p.Longitude, &p.Altitude, &p.Speed, &p.Course, &p.Odometer); err != nil {
			return nil, fmt.Errorf("scan point: %w", err)
		}
		points = append(points, p)
	}
	return points, nil
}

func (s *Store) PointCount(tripID string) (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM trip_points WHERE trip_id = ?`, tripID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("point count: %w", err)
	}
	return count, nil
}
