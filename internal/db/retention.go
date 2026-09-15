package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ExpungeBatchSize  = 100
	minTrustedUnix    = 1577836800
	maxExpungeAgeDays = int64(time.Duration(1<<63-1) / (24 * time.Hour))
)

var ErrIncrementalVacuumRequired = errors.New("incremental auto_vacuum migration required")

type ExpungePolicy struct {
	Kind    string
	Value   int64
	AgeDays string
}

func DefaultExpungePolicy() ExpungePolicy {
	return ExpungePolicy{Kind: "age", Value: int64(365 * 24 * time.Hour), AgeDays: "365"}
}
func (p ExpungePolicy) Operand() string {
	if p.Kind == "never" {
		return ""
	}
	if p.Kind == "age" {
		if p.AgeDays != "" {
			return p.AgeDays + "d"
		}
		return time.Duration(p.Value).String()
	}
	return strconv.FormatInt(p.Value, 10)
}

// ParseExpungePolicy accepts only transport-safe ASCII canonical values.
func ParseExpungePolicy(value string) (ExpungePolicy, error) {
	if value == "never" {
		return ExpungePolicy{Kind: "never"}, nil
	}
	kind, operand, ok := strings.Cut(value, ":")
	if !ok || operand == "" {
		return ExpungePolicy{}, errors.New("expected never, age:<duration>, count:<unsigned>, or size:<unsigned>")
	}
	switch kind {
	case "age":
		if days, ok := parseDays(operand); ok {
			return ExpungePolicy{Kind: kind, AgeDays: days}, nil
		}
		d, err := parseASCIIDuration(operand)
		if err != nil {
			return ExpungePolicy{}, fmt.Errorf("age must be a positive ASCII duration or integer days: %w", err)
		}
		return ExpungePolicy{Kind: kind, Value: int64(d)}, nil
	case "count", "size":
		if !canonicalUnsigned(operand) {
			return ExpungePolicy{}, fmt.Errorf("%s must be a canonical nonnegative decimal", kind)
		}
		n, err := strconv.ParseInt(operand, 10, 64)
		if err != nil {
			return ExpungePolicy{}, fmt.Errorf("%s exceeds int64", kind)
		}
		return ExpungePolicy{Kind: kind, Value: n}, nil
	default:
		return ExpungePolicy{}, errors.New("unknown expunge policy")
	}
}

func parseDays(value string) (string, bool) {
	if !strings.HasSuffix(value, "d") {
		return "", false
	}
	days := strings.TrimSuffix(value, "d")
	if !canonicalUnsigned(days) || days == "0" {
		return "", false
	}
	n, err := strconv.ParseInt(days, 10, 64)
	if err != nil || n > maxExpungeAgeDays {
		return "", false
	}
	return days, true
}

// parseASCIIDuration lexes the restricted Go duration grammar before using the
// standard overflow-safe duration parser. In particular, Go's Unicode µ alias
// is intentionally not accepted on this transport boundary.
func parseASCIIDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, errors.New("empty duration")
	}
	for i := 0; i < len(value); i++ {
		if value[i] > 0x7f {
			return 0, errors.New("non-ASCII duration")
		}
	}
	for i := 0; i < len(value); {
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		integerEnd := i
		if integerEnd == start {
			return 0, errors.New("missing duration integer")
		}
		if integerEnd-start > 1 && value[start] == '0' {
			return 0, errors.New("leading zero")
		}
		if i < len(value) && value[i] == '.' {
			i++
			fractionStart := i
			for i < len(value) && value[i] >= '0' && value[i] <= '9' {
				i++
			}
			if fractionStart == i {
				return 0, errors.New("empty fraction")
			}
		}
		unitStart := i
		for i < len(value) && value[i] >= 'a' && value[i] <= 'z' {
			i++
		}
		unit := value[unitStart:i]
		if unit != "ns" && unit != "us" && unit != "ms" && unit != "s" && unit != "m" && unit != "h" {
			return 0, errors.New("invalid duration unit")
		}
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, errors.New("duration must be positive whole nanoseconds")
	}
	return d, nil
}

func canonicalUnsigned(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value[0] == '0' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

type ExpungeResult struct{ DeletedTrips, DBBytes, WALBytes int64 }

// IncrementalVacuumEnabled reports whether size retention can reclaim pages
// without a blocking full rewrite.
func (s *Store) IncrementalVacuumEnabled() (bool, error) {
	var mode int
	if err := s.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return false, err
	}
	return mode == 2, nil
}

// MigrateToIncrementalVacuum is intentionally called only by the idle app
// path. ExecContext lets a ready transition interrupt VACUUM promptly.
func (s *Store) MigrateToIncrementalVacuum(ctx context.Context) error {
	enabled, err := s.IncrementalVacuumEnabled()
	if err != nil || enabled {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA auto_vacuum=incremental"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	enabled, err = s.IncrementalVacuumEnabled()
	if err != nil {
		return err
	}
	if !enabled {
		return ErrIncrementalVacuumRequired
	}
	return nil
}

// Expunge deletes one bounded oldest-first batch. Open rows are never
// eligible. Size policy first attempts bounded non-destructive reclamation.
func (s *Store) Expunge(policy ExpungePolicy, now time.Time) (ExpungeResult, error) {
	result, err := s.storageBytes()
	if err != nil {
		return result, err
	}
	if policy.Kind == "never" {
		return result, nil
	}
	if policy.Kind == "age" && !trustedWallTime(now) {
		return result, errors.New("wall clock unavailable")
	}
	if (policy.Kind == "count" || policy.Kind == "size") && policy.Value < 0 {
		return result, errors.New("retention value must not be negative")
	}
	if policy.Kind == "size" {
		enabled, err := s.IncrementalVacuumEnabled()
		if err != nil {
			return result, err
		}
		if !enabled {
			return result, ErrIncrementalVacuumRequired
		}
		if err := s.safeReclaim(); err != nil {
			return result, err
		}
		result, err = s.storageBytes()
		if err != nil {
			return result, err
		}
		if result.DBBytes+result.WALBytes <= policy.Value {
			return result, nil
		}
	}
	query, args, err := expungeQuery(policy, now)
	if err != nil {
		return result, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(query, args...)
	if err != nil {
		return result, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM trips WHERE id=? AND status IN ('completed','abandoned')`, id); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.DeletedTrips = int64(len(ids))
	if policy.Kind == "size" {
		if err := s.safeReclaim(); err != nil {
			return result, err
		}
	}
	return s.withStorage(result)
}

// safeReclaim uses SQLite's bounded checkpoint attempt and no more than 100
// incremental-vacuum pages before retention considers deleting history.
func (s *Store) safeReclaim() error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("sqlite checkpoint busy")
	}
	_, err := s.db.Exec("PRAGMA incremental_vacuum(100)")
	return err
}

func trustedWallTime(now time.Time) bool { return !now.IsZero() && now.Unix() >= minTrustedUnix }
func expungeQuery(policy ExpungePolicy, now time.Time) (string, []any, error) {
	const eligible = "status IN ('completed','abandoned')"
	const timestamp = "COALESCE(NULLIF(ended_at, 0), NULLIF(started_at, 0), created_at)"
	switch policy.Kind {
	case "age":
		cutoff, ok := ageCutoff(policy, now)
		if !ok {
			return "SELECT id FROM trips WHERE 1=0", nil, nil
		}
		return `SELECT id FROM trips WHERE ` + eligible + ` AND ` + timestamp + ` < ? ORDER BY ` + timestamp + ` ASC, id ASC LIMIT ?`, []any{cutoff, ExpungeBatchSize}, nil
	case "count":
		return `SELECT id FROM (SELECT id, ` + timestamp + ` AS retained_at FROM trips WHERE ` + eligible + ` ORDER BY retained_at DESC, id DESC LIMIT -1 OFFSET ?) ORDER BY retained_at ASC, id ASC LIMIT ?`, []any{policy.Value, ExpungeBatchSize}, nil
	case "size":
		return `SELECT id FROM trips WHERE ` + eligible + ` ORDER BY ` + timestamp + ` ASC, id ASC LIMIT ?`, []any{ExpungeBatchSize}, nil
	default:
		return "", nil, errors.New("unknown expunge policy")
	}
}
func ageCutoff(policy ExpungePolicy, now time.Time) (int64, bool) {
	if policy.AgeDays == "" {
		if policy.Value <= 0 || now.IsZero() {
			return 0, false
		}
		return now.Add(-time.Duration(policy.Value)).Unix(), true
	}
	days, err := strconv.ParseInt(policy.AgeDays, 10, 64)
	if err != nil || days <= 0 || days > maxExpungeAgeDays {
		return 0, false
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour).Unix(), true
}
func (s *Store) withStorage(result ExpungeResult) (ExpungeResult, error) {
	bytes, err := s.storageBytes()
	if err != nil {
		return result, err
	}
	result.DBBytes, result.WALBytes = bytes.DBBytes, bytes.WALBytes
	return result, nil
}
func (s *Store) storageBytes() (ExpungeResult, error) {
	var pageCount, pageSize int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return ExpungeResult{}, err
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return ExpungeResult{}, err
	}
	mainSize := pageCount * pageSize
	if info, err := os.Stat(s.path); err == nil && info.Size() > mainSize {
		mainSize = info.Size()
	}
	var walSize int64
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		walSize = info.Size()
	} else if !os.IsNotExist(err) {
		return ExpungeResult{}, err
	}
	return ExpungeResult{DBBytes: mainSize, WALBytes: walSize}, nil
}
