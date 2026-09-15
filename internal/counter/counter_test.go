package counter

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/db"
)

func testCounter(t *testing.T, at time.Time) (*Counter, *db.Store, *time.Time) {
	t.Helper()
	s, err := db.Open(filepath.Join(t.TempDir(), "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := at
	c := New(s)
	c.SetClock(func() time.Time { return now })
	if err := c.Hydrate(1000, true); err != nil {
		t.Fatal(err)
	}
	return c, s, &now
}
func TestRidePolicyKeepsParkedSession(t *testing.T) {
	c, _, now := testCounter(t, time.Date(2025, 1, 1, 10, 0, 0, 0, time.Local))
	c.VehicleState("ready-to-drive")
	c.Odometer(1100)
	*now = now.Add(time.Minute)
	c.VehicleState("parked")
	*now = now.Add(time.Minute)
	c.VehicleState("ready-to-drive")
	c.Odometer(1150)
	if got := c.Snapshot(); got.DistanceM != 150 || got.Generation != 1 {
		t.Fatalf("parked session = %+v", got)
	}
	c.VehicleState("stand-by")
	*now = now.Add(time.Minute)
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.DistanceM != 0 || got.Generation != 2 {
		t.Fatalf("new session = %+v", got)
	}
}
func TestRidePolicyKeepsHopOnSession(t *testing.T) {
	c, _, now := testCounter(t, time.Date(2025, 1, 1, 10, 0, 0, 0, time.Local))
	if err := c.VehicleState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	if err := c.Odometer(1100); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := c.VehicleState("hop-on"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := c.VehicleState("hop-on-learning"); err != nil {
		t.Fatal(err)
	}
	if err := c.VehicleState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	if err := c.Odometer(1150); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.DistanceM != 150 || got.Generation != 1 {
		t.Fatalf("hop-on session reset: %+v", got)
	}
}

func TestDayPolicyAnchorsAndNeverMovesBackward(t *testing.T) {
	c, _, now := testCounter(t, time.Date(2025, 1, 2, 10, 0, 0, 0, time.Local))
	c.Policy("day")
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("same-day policy transition reset: %+v", got)
	}
	c.VehicleState("stand-by")
	*now = now.Add(-24 * time.Hour)
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("backward date reset: %+v", got)
	}
	c.VehicleState("stand-by")
	*now = time.Date(2025, 1, 3, 10, 0, 0, 0, time.Local)
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.Generation != 1 || got.ResetReason != "day" {
		t.Fatalf("later day = %+v", got)
	}
}
func TestInvalidDailyAnchorSeedsWhenTimeBecomesTrustworthy(t *testing.T) {
	c, _, now := testCounter(t, time.Unix(0, 0))
	c.Policy("day")
	c.VehicleState("ready-to-drive")
	c.VehicleState("stand-by")
	*now = time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local)
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("first trustworthy day reset: %+v", got)
	}
	c.VehicleState("stand-by")
	*now = now.Add(24 * time.Hour)
	c.VehicleState("ready-to-drive")
	if got := c.Snapshot(); got.Generation != 1 {
		t.Fatalf("later date did not reset: %+v", got)
	}
}
func TestBatteryConfiguredSlotsAndReinsertion(t *testing.T) {
	c, _, _ := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	c.Policy("battery")
	c.SeedBattery(0, true, "a")
	c.Battery(0, false, "")
	c.Battery(0, true, "a")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("same pack reset: %+v", got)
	}
	c.Battery(1, true, "b")
	c.Battery(1, true, "c")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("disabled slot reset: %+v", got)
	}
	c.ConfigureDualBattery(true)
	c.SeedBattery(1, true, "c")
	if got := c.Snapshot(); got.Generation != 0 {
		t.Fatalf("dual mode toggle reset: %+v", got)
	}
	c.Battery(1, true, "d")
	if got := c.Snapshot(); got.Generation != 1 || got.ResetReason != "battery" {
		t.Fatalf("replacement = %+v", got)
	}
}
func TestManualCommandIsIdempotentAndBusy(t *testing.T) {
	c, _, _ := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	snap, r, err := c.ManualReset("id-1")
	if err != nil || r.Status != "ok" || snap.Generation != 1 || snap.ResetReason != "manual" {
		t.Fatalf("reset: %+v %+v %v", snap, r, err)
	}
	snap, r, err = c.ManualReset("id-1")
	if err != nil || r.Status != "ok" || snap.Generation != 1 {
		t.Fatalf("retry: %+v %+v %v", snap, r, err)
	}
	c.VehicleState("ready-to-drive")
	_, r, err = c.ManualReset("busy")
	if err != nil || r.Error != "busy" {
		t.Fatalf("busy: %+v %v", r, err)
	}
}
func TestAverageSpeedUsesKilometresPerHour(t *testing.T) {
	c, _, now := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	c.VehicleState("ready-to-drive")
	c.Odometer(1100)
	*now = now.Add(10 * time.Second)
	if got := c.Snapshot(); got.AverageSpeedKmh != 36 {
		t.Fatalf("average speed = %d, want 36", got.AverageSpeedKmh)
	}
}
func TestRestartCapturesFinalOdometerAndDiscardsGapDuration(t *testing.T) {
	c, s, now := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	c.VehicleState("ready-to-drive")
	c.Odometer(1000)
	*now = now.Add(10 * time.Minute)
	c.Checkpoint()
	c2 := New(s)
	*now = now.Add(24 * time.Hour)
	c2.SetClock(func() time.Time { return *now })
	if err := c2.Hydrate(1100, true); err != nil {
		t.Fatal(err)
	}
	if err := c2.Odometer(1100); err != nil {
		t.Fatal(err)
	}
	if err := c2.RecoverVehicleState("stand-by"); err != nil {
		t.Fatal(err)
	}
	got := c2.Snapshot()
	if got.DistanceM != 100 || got.DurationS != 600 {
		t.Fatalf("restart recovery = %+v", got)
	}
}
func TestInvalidOdometerAndTime(t *testing.T) {
	c, _, now := testCounter(t, time.Unix(0, 0))
	c.VehicleState("ready-to-drive")
	c.Odometer(1100)
	c.Odometer(900)
	*now = now.Add(time.Hour)
	c.VehicleState("parked")
	if got := c.Snapshot(); got.DistanceM != 100 || got.DurationS != 0 {
		t.Fatalf("invalid values = %+v", got)
	}
}

func TestTerminalOdometerIsAccountedBeforeVehicleStops(t *testing.T) {
	c, _, _ := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	if err := c.VehicleState("ready-to-drive"); err != nil {
		t.Fatal(err)
	}
	if err := c.Odometer(1090); err != nil {
		t.Fatal(err)
	}
	if err := c.VehicleStateWithOdometer("parked", 1100, true); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.DistanceM != 100 {
		t.Fatalf("terminal odometer was lost: %+v", got)
	}
}

func TestInvalidPolicyRetainsPersistedPolicy(t *testing.T) {
	c, store, now := testCounter(t, time.Date(2025, 1, 1, 1, 0, 0, 0, time.Local))
	if err := c.Policy(PolicyManual); err != nil {
		t.Fatal(err)
	}
	if err := c.Policy("invalid"); err == nil {
		t.Fatal("invalid policy unexpectedly succeeded")
	}
	if got := c.Snapshot(); got.ResetPolicy != PolicyManual {
		t.Fatalf("invalid live update changed policy: %+v", got)
	}
	c2 := New(store)
	c2.SetClock(func() time.Time { return *now })
	if err := c2.Hydrate(1000, true); err != nil {
		t.Fatal(err)
	}
	if err := c2.Policy("invalid"); err == nil {
		t.Fatal("invalid restart policy unexpectedly succeeded")
	}
	if got := c2.Snapshot(); got.ResetPolicy != PolicyManual {
		t.Fatalf("invalid restart policy changed persisted policy: %+v", got)
	}
}
