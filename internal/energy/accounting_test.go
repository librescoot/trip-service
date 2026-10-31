package energy

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var epoch = time.Unix(1800000000, 0)

func pack(at time.Time, serial string, capacity int64) map[string]string {
	return map[string]string{"present": "true", "state": "active", "sample:valid": "true", "sample:timestamp": strconv.FormatInt(at.UnixMilli(), 10), "voltage": "53000", "remaining-capacity": strconv.FormatInt(capacity, 10), "full-capacity": "33000", "serial-number": serial}
}
func vehicle(state string) map[string]string { return map[string]string{"state": state} }
func accountant() *Accountant                { return New(Totals{Version: 1, Since: epoch}, epoch) }
func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBothPacksAndSignedCapacityEnergy(t *testing.T) {
	a := accountant()
	a.Observe(epoch, vehicle("ready-to-drive"), [2]map[string]string{pack(epoch, "A", 20000), pack(epoch, "B", 21000)})
	next := epoch.Add(40 * time.Second)
	p0, p1 := pack(next, "A", 19900), pack(next, "B", 20800)
	p0["voltage"] = "51000"
	a.Observe(next, vehicle("ready-to-drive"), [2]map[string]string{p0, p1})
	closeTo(t, a.Totals.Net(), 5200+10600)
	closeTo(t, a.Totals.Driving, a.Totals.Net())
	for i := 0; i < 10; i++ {
		a.Observe(next.Add(time.Duration(i)*time.Second), vehicle("ready-to-drive"), [2]map[string]string{p0, p1})
	}
	closeTo(t, a.Totals.Net(), 15800)
	next = next.Add(40 * time.Second)
	a.Observe(next, vehicle("ready-to-drive"), [2]map[string]string{pack(next, "A", 19910), pack(next, "B", 20800)})
	closeTo(t, a.Totals.Net(), 15280)
	if a.Totals.Intervals != 4 {
		t.Fatal(a.Totals)
	}
}

func TestPhaseChangesNeverDoubleCount(t *testing.T) {
	for _, state := range []string{"ready-to-drive", "parked", "stand-by", "unknown"} {
		t.Run(state, func(t *testing.T) {
			a := accountant()
			a.Observe(epoch, vehicle(state), [2]map[string]string{pack(epoch, "A", 20000), nil})
			next := epoch.Add(40 * time.Second)
			a.Observe(next, vehicle(state), [2]map[string]string{pack(next, "A", 19900), nil})
			closeTo(t, a.Totals.Net(), 5300)
			switch phase(state) {
			case "driving":
				closeTo(t, a.Totals.Driving, 5300)
			case "parked":
				closeTo(t, a.Totals.Parked, 5300)
			case "standby":
				closeTo(t, a.Totals.Standby, 5300)
			default:
				closeTo(t, a.Totals.Unassigned, 5300)
			}
		})
	}
	a := accountant()
	old := pack(epoch, "A", 20000)
	a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{old, nil})
	a.Observe(epoch.Add(10*time.Second), vehicle("ready-to-drive"), [2]map[string]string{old, nil})
	next := epoch.Add(40 * time.Second)
	a.Observe(next, vehicle("ready-to-drive"), [2]map[string]string{pack(next, "A", 19900), nil})
	closeTo(t, a.Totals.Unassigned, 5300)
	closeTo(t, a.Totals.Driving, 0)
	// A state timestamp reveals an intervening transition even if both polls
	// happen to see the same state.
	v := vehicle("ready-to-drive")
	v["state:timestamp"] = "changed"
	next = next.Add(40 * time.Second)
	a.Observe(next, v, [2]map[string]string{pack(next, "A", 19800), nil})
	closeTo(t, a.Totals.Unassigned, 10600)
}

func TestInvalidDataAndStaleCacheStayUnknown(t *testing.T) {
	for key, value := range map[string]string{"present": "false", "state": "idle", "sample:valid": "false", "sample:timestamp": "bad", "voltage": "0", "remaining-capacity": "-1", "full-capacity": "0", "serial-number": ""} {
		t.Run(key, func(t *testing.T) {
			a := accountant()
			p := pack(epoch, "A", 20000)
			p[key] = value
			if status := a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{p, nil}); status != "waiting" {
				t.Fatal(status)
			}
			if a.Fields(epoch, "waiting")["known"] != "false" {
				t.Fatal("unknown represented as zero")
			}
		})
	}
	a := accountant()
	p := pack(epoch, "A", 20000)
	a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{p, nil})
	next := epoch.Add(91 * time.Second)
	a.Observe(next, vehicle("stand-by"), [2]map[string]string{p, nil})
	if a.Totals.Gaps != 1 {
		t.Fatal(a.Totals)
	}
	a.Observe(next, vehicle("stand-by"), [2]map[string]string{pack(next, "A", 19000), nil})
	closeTo(t, a.Totals.Net(), 0)
	if _, ok := readSample(pack(epoch.Add(time.Second), "A", 20000), epoch, epoch); ok {
		t.Fatal("future acquisition accepted")
	}
}

func TestSwapRemovalRestartAndGaugeReset(t *testing.T) {
	for _, kind := range []string{"swap", "full", "jump", "absence", "reconnect", "long-gap", "clock-backward", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			a := accountant()
			a.Observe(epoch, vehicle("parked"), [2]map[string]string{pack(epoch, "A", 20000), nil})
			next := epoch.Add(40 * time.Second)
			p := pack(next, "A", 19900)
			var other map[string]string
			switch kind {
			case "swap":
				p["serial-number"] = "B"
			case "full":
				p["full-capacity"] = "34000"
			case "jump":
				p["remaining-capacity"] = "1000"
			case "absence":
				a.Observe(epoch.Add(time.Second), vehicle("parked"), [2]map[string]string{})
			case "reconnect":
				a.Break()
			case "long-gap":
				next = epoch.Add(5 * time.Minute)
				p = pack(next, "A", 19900)
			case "clock-backward":
				next = epoch.Add(-time.Second)
				p = pack(next, "A", 19900)
			case "duplicate":
				other = pack(next, "A", 19900)
			}
			a.Observe(next, vehicle("parked"), [2]map[string]string{p, other})
			closeTo(t, a.Totals.Net(), 0)
			if a.Totals.Gaps == 0 {
				t.Fatal("missing discontinuity")
			}
		})
	}
	a := New(Totals{Version: 1, Since: epoch, Driving: 1234, Intervals: 1}, epoch.Add(time.Minute))
	a.Observe(epoch.Add(time.Minute), vehicle("parked"), [2]map[string]string{pack(epoch, "A", 19000), nil})
	if a.slots[0] != nil {
		t.Fatal("retained pre-start sample accepted")
	}
	next := epoch.Add(80 * time.Second)
	a.Observe(next, vehicle("parked"), [2]map[string]string{pack(next, "A", 18000), nil})
	closeTo(t, a.Totals.Net(), 1234)
}

func TestZeroAndPartialCoverage(t *testing.T) {
	a := accountant()
	a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{pack(epoch, "A", 20000), nil})
	next := epoch.Add(40 * time.Second)
	bad := pack(next, "B", 20000)
	bad["sample:valid"] = "false"
	status := a.Observe(next, vehicle("stand-by"), [2]map[string]string{pack(next, "A", 20000), bad})
	if status != "partial" || a.Fields(next, status)["known"] != "true" {
		t.Fatal(status, a.Totals)
	}
	closeTo(t, a.Totals.Net(), 0)
}

func TestDurabilityAndCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "energy.json")
	state, err := load(path, epoch)
	if err != nil {
		t.Fatal(err)
	}
	state.Driving = 123.456
	state.Parked = -10
	state.Intervals = 10
	if err = save(path, state); err != nil {
		t.Fatal(err)
	}
	got, err := load(path, epoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != state {
		t.Fatalf("%+v != %+v", got, state)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for _, raw := range []string{"{", `{"version":2}`, `{"version":1,"since":"2026-01-01T00:00:00Z","driving_mwh":1e20}`} {
		if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = load(path, epoch); err == nil {
			t.Fatal("corrupt accounting silently reset")
		}
	}
}
