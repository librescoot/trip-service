// Package counter owns the persistent vehicle-wide counter policy.
package counter

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/librescoot/trip-service/internal/db"
)

const (
	PolicyRide    = "ride"
	PolicyDay     = "day"
	PolicyBattery = "battery"
	PolicyManual  = "manual"
)

type Battery struct {
	Present bool
	Serial  string
}
type Snapshot struct {
	DistanceM, DurationS, AverageSpeedKmh int64
	ResetPolicy                           string
	ResetAt                               int64
	ResetReason                           string
	Generation                            int64
	Status                                string
	UpdatedAt                             int64
}
type Counter struct {
	store *db.Store
	mu    sync.Mutex
	state *db.Counter
	now   func() time.Time
}

func New(store *db.Store) *Counter               { return &Counter{store: store, now: time.Now} }
func (c *Counter) SetClock(now func() time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = now }
func (c *Counter) Hydrate(odometer int64, valid bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, e := c.store.EnsureCounter(c.now().Unix(), odometer, valid)
	if e == nil {
		c.state = s
	}
	return e
}
func (c *Counter) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked(c.now().Unix())
}

func (c *Counter) Policy(value string) error {
	next, ok := parsePolicy(value)
	if !ok {
		return fmt.Errorf("invalid counter reset policy %q", value)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	now := c.now().Unix()
	if c.state.ResetPolicy != next {
		c.state.ResetPolicy = next
		if next == PolicyDay && trustworthyTime(now) {
			c.state.DailyAnchor = localDate(now)
		}
	}
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}
func (c *Counter) ConfigureDualBattery(enabled bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	c.state.DualBattery = enabled
	c.state.UpdatedAt = c.now().Unix()
	return c.store.SaveCounter(c.state)
}

// SeedBattery records an identity without interpreting it as a replacement.
func (c *Counter) SeedBattery(slot int, present bool, serial string) error {
	return c.battery(slot, present, serial, true)
}
func (c *Counter) Battery(slot int, present bool, serial string) error {
	return c.battery(slot, present, serial, false)
}
func (c *Counter) battery(slot int, present bool, serial string, seed bool) error {
	if slot < 0 || slot > 1 {
		return fmt.Errorf("invalid battery slot")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	if slot == 1 && !c.state.DualBattery {
		return nil
	}
	now := c.now().Unix()
	serial = strings.TrimSpace(serial)
	old := c.state.BatterySerial[slot]
	if present && serial != "" {
		if !seed && old != "" && old != serial && c.state.ResetPolicy == PolicyBattery {
			c.resetLocked(now, "battery")
		}
		c.state.BatterySerial[slot] = serial
	}
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}
func (c *Counter) Odometer(odometer int64) error {
	if odometer < 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	if c.state.LastOdometer != nil && odometer < *c.state.LastOdometer {
		return nil
	}
	now := c.now().Unix()
	if c.state.Active && c.state.LastOdometer != nil {
		c.state.DistanceM += odometer - *c.state.LastOdometer
	}
	if c.state.Active && c.state.ReadyStartedAt == 0 && trustworthyTime(now) {
		c.state.ReadyStartedAt = now
	}
	v := odometer
	c.state.LastOdometer = &v
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}

// RecoverVehicleState discards the unavailable interval since the last durable
// checkpoint while applying the startup vehicle snapshot.
func (c *Counter) RecoverVehicleState(state string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	now := c.now().Unix()
	if c.state.Active {
		c.state.ReadyStartedAt = 0
	}
	if state == "ready-to-drive" {
		if !c.state.Active {
			c.startReadyLocked(now)
		} else if trustworthyTime(now) {
			c.state.ReadyStartedAt = now
		}
	} else {
		c.state.Active = false
		c.state.ReadyStartedAt = 0
		if !pausedRideState(state) {
			c.state.SessionOpen = false
		}
	}
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}
func (c *Counter) VehicleState(state string) error {
	return c.VehicleStateWithOdometer(state, 0, false)
}

// VehicleStateWithOdometer atomically accounts for the terminal odometer
// snapshot before ending an active ready-to-drive interval.
func (c *Counter) VehicleStateWithOdometer(state string, odometer int64, odometerValid bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	now := c.now().Unix()
	if state == "ready-to-drive" {
		if !c.state.Active {
			c.startReadyLocked(now)
		}
	} else {
		if odometerValid && odometer >= 0 && (c.state.LastOdometer == nil || odometer >= *c.state.LastOdometer) {
			if c.state.Active && c.state.LastOdometer != nil {
				c.state.DistanceM += odometer - *c.state.LastOdometer
			}
			v := odometer
			c.state.LastOdometer = &v
		}
		c.checkpointDurationLocked(now)
		if !pausedRideState(state) {
			c.state.SessionOpen = false
		}
	}
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}
func pausedRideState(state string) bool {
	return state == "parked" || state == "hop-on" || state == "hop-on-learning"
}

func (c *Counter) startReadyLocked(now int64) {
	if !c.state.SessionOpen {
		if c.shouldResetRideLocked(now) {
			c.resetLocked(now, c.state.ResetPolicy)
		}
		c.state.SessionOpen = true
	}
	c.state.Active = true
	if trustworthyTime(now) {
		c.state.ReadyStartedAt = now
	}
}

// Checkpoint makes elapsed Ready-to-Drive time durable without ending it.
func (c *Counter) Checkpoint() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	now := c.now().Unix()
	if c.state.Active {
		c.accrueDurationLocked(now)
		if trustworthyTime(now) {
			c.state.ReadyStartedAt = now
		}
	}
	c.state.UpdatedAt = now
	return c.store.SaveCounter(c.state)
}
func (c *Counter) ManualReset(id string) (Snapshot, db.CommandResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureLocked()
	id = strings.TrimSpace(id)
	now := c.now().Unix()
	if id == "" {
		return c.snapshotLocked(now), db.CommandResult{Op: "counter.reset", Status: "error", Error: "invalid"}, nil
	}
	odo := int64(0)
	valid := false
	if c.state.LastOdometer != nil {
		odo = *c.state.LastOdometer
		valid = true
	}
	s, r, e := c.store.ResetCounterCommand(id, now, odo, valid, c.state.Active)
	if e != nil {
		return Snapshot{}, r, e
	}
	c.state = s
	return c.snapshotLocked(now), r, nil
}
func (c *Counter) shouldResetRideLocked(now int64) bool {
	switch c.state.ResetPolicy {
	case PolicyRide:
		return true
	case PolicyDay:
		if !trustworthyTime(now) {
			return false
		}
		today := localDate(now)
		if c.state.DailyAnchor == "" {
			c.state.DailyAnchor = today
			return false
		}
		return today > c.state.DailyAnchor
	default:
		return false
	}
}
func (c *Counter) resetLocked(now int64, reason string) {
	was := c.state.Active
	c.checkpointDurationLocked(now)
	c.state.DistanceM, c.state.DurationS, c.state.ResetAt, c.state.ResetReason, c.state.Generation = 0, 0, now, reason, c.state.Generation+1
	if c.state.ResetPolicy == PolicyDay && trustworthyTime(now) {
		c.state.DailyAnchor = localDate(now)
	}
	if was {
		c.state.Active = true
		if trustworthyTime(now) {
			c.state.ReadyStartedAt = now
		}
	}
}
func (c *Counter) accrueDurationLocked(now int64) {
	if c.state.Active && c.state.ReadyStartedAt > 0 && now > c.state.ReadyStartedAt {
		c.state.DurationS += now - c.state.ReadyStartedAt
	}
}
func (c *Counter) checkpointDurationLocked(now int64) {
	c.accrueDurationLocked(now)
	c.state.Active = false
	c.state.ReadyStartedAt = 0
}
func (c *Counter) snapshotLocked(now int64) Snapshot {
	c.ensureLocked()
	d := c.state.DurationS
	if c.state.Active && c.state.ReadyStartedAt > 0 && now > c.state.ReadyStartedAt {
		d += now - c.state.ReadyStartedAt
	}
	avg := int64(0)
	if d > 0 {
		avg = (c.state.DistanceM*36 + d*5) / (d * 10)
	}
	status := "idle"
	if c.state.Active {
		status = "recording"
	}
	return Snapshot{c.state.DistanceM, d, avg, c.state.ResetPolicy, c.state.ResetAt, c.state.ResetReason, c.state.Generation, status, c.state.UpdatedAt}
}
func (c *Counter) ensureLocked() {
	if c.state == nil {
		panic("counter used before Hydrate")
	}
}
func parsePolicy(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case PolicyRide:
		return PolicyRide, true
	case PolicyDay:
		return PolicyDay, true
	case PolicyBattery:
		return PolicyBattery, true
	case PolicyManual:
		return PolicyManual, true
	default:
		return "", false
	}
}
func trustworthyTime(v int64) bool { return v >= 1577836800 }
func localDate(v int64) string     { return time.Unix(v, 0).In(time.Local).Format("2006-01-02") }
