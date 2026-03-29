package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode=wal",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %q: %w", p, err)
		}
	}

	store := &Store{db: db, path: path}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS trips (
			id TEXT PRIMARY KEY,
			profile_id TEXT,
			status TEXT NOT NULL DEFAULT 'recording',
			started_at INTEGER,
			ended_at INTEGER,
			start_lat REAL,
			start_lon REAL,
			end_lat REAL,
			end_lon REAL,
			start_odometer INTEGER,
			end_odometer INTEGER,
			distance_m INTEGER NOT NULL DEFAULT 0,
			duration_s INTEGER NOT NULL DEFAULT 0,
			avg_speed REAL NOT NULL DEFAULT 0,
			max_speed REAL NOT NULL DEFAULT 0,
			point_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER
		);

		CREATE TABLE IF NOT EXISTS trip_points (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trip_id TEXT NOT NULL REFERENCES trips(id) ON DELETE CASCADE,
			timestamp INTEGER,
			latitude REAL NOT NULL,
			longitude REAL NOT NULL,
			altitude REAL,
			speed REAL,
			course REAL,
			odometer INTEGER
		);

		CREATE INDEX IF NOT EXISTS idx_trips_profile ON trips(profile_id);
		CREATE INDEX IF NOT EXISTS idx_trips_started ON trips(started_at);
		CREATE INDEX IF NOT EXISTS idx_points_trip ON trip_points(trip_id);
	`)
	return err
}
