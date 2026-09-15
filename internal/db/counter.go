package db

import (
	"database/sql"
	"fmt"
	"time"
)

// Counter is the persistent vehicle-wide trip counter state. Distance and
// duration contain durable checkpoints; callers project an active interval.
type Counter struct {
	DistanceM      int64
	DurationS      int64
	ResetPolicy    string
	ResetAt        int64
	ResetReason    string
	Generation     int64
	UpdatedAt      int64
	Active         bool
	ReadyStartedAt int64
	LastOdometer   *int64
	SessionOpen    bool
	BatterySerial  [2]string
	DailyAnchor    string
	DualBattery    bool
}

type CommandResult struct{ ID, Op, Status, Error string }

func (s *Store) EnsureCounter(now, odometer int64, odometerValid bool) (*Counter, error) {
	last := any(nil)
	if odometerValid {
		last = odometer
	}
	_, err := s.db.Exec(`INSERT INTO trip_counter (id, reset_at, updated_at, last_odometer)
		VALUES (1, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, now, now, last)
	if err != nil {
		return nil, fmt.Errorf("create counter: %w", err)
	}
	return s.Counter()
}

func (s *Store) Counter() (*Counter, error) { return readCounter(s.db.QueryRow) }

type rowQuery func(string, ...any) *sql.Row

func readCounter(query rowQuery) (*Counter, error) {
	var c Counter
	var active, session, dual int
	var odo sql.NullInt64
	err := query(`SELECT distance_m, duration_s, reset_policy, reset_at, reset_reason,
		generation, updated_at, active, ready_started_at, last_odometer, session_open,
		battery_0_serial, battery_1_serial, daily_anchor, dual_battery FROM trip_counter WHERE id = 1`).Scan(
		&c.DistanceM, &c.DurationS, &c.ResetPolicy, &c.ResetAt, &c.ResetReason, &c.Generation, &c.UpdatedAt,
		&active, &c.ReadyStartedAt, &odo, &session, &c.BatterySerial[0], &c.BatterySerial[1], &c.DailyAnchor, &dual)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read counter: %w", err)
	}
	c.Active, c.SessionOpen, c.DualBattery = active != 0, session != 0, dual != 0
	if odo.Valid {
		v := odo.Int64
		c.LastOdometer = &v
	}
	return &c, nil
}

func (s *Store) SaveCounter(c *Counter) error {
	_, err := saveCounter(s.db.Exec, c)
	if err != nil {
		return fmt.Errorf("save counter: %w", err)
	}
	return nil
}

type execQuery func(string, ...any) (sql.Result, error)

func saveCounter(exec execQuery, c *Counter) (sql.Result, error) {
	last := any(nil)
	if c.LastOdometer != nil {
		last = *c.LastOdometer
	}
	return exec(`UPDATE trip_counter SET distance_m=?, duration_s=?, reset_policy=?, reset_at=?, reset_reason=?,
		generation=?, updated_at=?, active=?, ready_started_at=?, last_odometer=?, session_open=?,
		battery_0_serial=?, battery_1_serial=?, daily_anchor=?, dual_battery=? WHERE id=1`,
		c.DistanceM, c.DurationS, c.ResetPolicy, c.ResetAt, c.ResetReason, c.Generation, c.UpdatedAt, boolInt(c.Active),
		c.ReadyStartedAt, last, boolInt(c.SessionOpen), c.BatterySerial[0], c.BatterySerial[1], c.DailyAnchor, boolInt(c.DualBattery))
}

// ResetCounterCommand commits both an idempotency record and successful reset
// state in one transaction. Every newly recorded outcome prunes old records.
func (s *Store) ResetCounterCommand(id string, now, odometer int64, odometerValid, active bool) (*Counter, CommandResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, CommandResult{}, err
	}
	defer tx.Rollback()
	var r CommandResult
	err = tx.QueryRow(`SELECT id,op,status,error FROM trip_counter_commands WHERE id=?`, id).Scan(&r.ID, &r.Op, &r.Status, &r.Error)
	if err == nil {
		c, e := readCounter(tx.QueryRow)
		if e != nil {
			return nil, r, e
		}
		return c, r, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return nil, r, err
	}
	r = CommandResult{ID: id, Op: "counter.reset"}
	c, err := readCounter(tx.QueryRow)
	if err != nil {
		return nil, r, err
	}
	if active {
		r.Status, r.Error = "error", "busy"
	} else {
		counterReset(c, now, odometer, odometerValid, "manual")
		if _, err := saveCounter(tx.Exec, c); err != nil {
			return nil, r, err
		}
		r.Status = "ok"
	}
	if _, err = tx.Exec(`INSERT INTO trip_counter_commands(id,op,status,error,created_at) VALUES(?,?,?,?,?)`, r.ID, r.Op, r.Status, r.Error, now); err != nil {
		return nil, r, err
	}
	if err = pruneCommands(tx, now, id); err != nil {
		return nil, r, err
	}
	return c, r, tx.Commit()
}

func pruneCommands(tx *sql.Tx, now int64, keep string) error {
	if _, err := tx.Exec(`DELETE FROM trip_counter_commands WHERE created_at < ? AND id != ?`, now-int64((30*24*time.Hour).Seconds()), keep); err != nil {
		return err
	}
	// A count cap protects the table if the clock is stalled or invalid.
	_, err := tx.Exec(`DELETE FROM trip_counter_commands WHERE id IN (
		SELECT id FROM trip_counter_commands ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET 10000
	) AND id != ?`, keep)
	return err
}
func counterReset(c *Counter, now, odometer int64, valid bool, reason string) {
	c.DistanceM, c.DurationS, c.ResetAt, c.ResetReason, c.Generation, c.UpdatedAt = 0, 0, now, reason, c.Generation+1, now
	if valid {
		v := odometer
		c.LastOdometer = &v
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
