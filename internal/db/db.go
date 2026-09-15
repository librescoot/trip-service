package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// A single connection keeps foreign-key enforcement and VACUUM serialized.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	pragmas := []string{
		"PRAGMA auto_vacuum=incremental",
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
			active_started_at INTEGER NOT NULL DEFAULT 0,
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

		CREATE TABLE IF NOT EXISTS trip_counter (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			distance_m INTEGER NOT NULL DEFAULT 0,
			duration_s INTEGER NOT NULL DEFAULT 0,
			reset_policy TEXT NOT NULL DEFAULT 'ride',
			reset_at INTEGER NOT NULL DEFAULT 0,
			reset_reason TEXT NOT NULL DEFAULT 'initial',
			generation INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			active INTEGER NOT NULL DEFAULT 0,
			ready_started_at INTEGER NOT NULL DEFAULT 0,
			last_odometer INTEGER,
			session_open INTEGER NOT NULL DEFAULT 0,
			battery_0_serial TEXT NOT NULL DEFAULT '',
			battery_1_serial TEXT NOT NULL DEFAULT '',
			daily_anchor TEXT NOT NULL DEFAULT '',
			dual_battery INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS trip_counter_commands (
			id TEXT PRIMARY KEY,
			op TEXT NOT NULL,
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_trip_counter_commands_created ON trip_counter_commands(created_at);
	`)
	if err != nil {
		return err
	}
	for table, columns := range map[string][]string{
		"trip_counter": {"daily_anchor TEXT NOT NULL DEFAULT ''", "dual_battery INTEGER NOT NULL DEFAULT 0"},
		"trips":        {"active_started_at INTEGER NOT NULL DEFAULT 0"},
	} {
		for _, column := range columns {
			if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
				return err
			}
		}
	}
	return nil
}
