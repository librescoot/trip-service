package energy

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

const maxSampleAge = 90 * time.Second
const maxInterval = 120 * time.Second

// Totals are signed net energy estimates. A capacity increase is negative
// consumption, not a separately measured recuperation counter.
type Totals struct {
	Version    int       `json:"version"`
	Since      time.Time `json:"since"`
	Driving    float64   `json:"driving_mwh"`
	Parked     float64   `json:"parked_mwh"`
	Standby    float64   `json:"standby_mwh"`
	Unassigned float64   `json:"unassigned_mwh"`
	Intervals  uint64    `json:"intervals"`
	Gaps       uint64    `json:"gaps"`
}

func (t Totals) Net() float64 { return t.Driving + t.Parked + t.Standby + t.Unassigned }

func (t Totals) validate() error {
	if t.Version != 1 || t.Since.IsZero() {
		return fmt.Errorf("invalid energy state version or start time")
	}
	for _, v := range []float64{t.Driving, t.Parked, t.Standby, t.Unassigned} {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e15 {
			return fmt.Errorf("invalid energy total")
		}
	}
	return nil
}

type Sample struct {
	Serial                  string
	At                      time.Time
	Voltage, Capacity, Full int64
}

type baseline struct {
	Sample
	phase string
	mixed bool
}

type Accountant struct {
	Totals     Totals
	started    time.Time
	slots      [2]*baseline
	phaseToken string
}

func New(t Totals, now time.Time) *Accountant {
	return &Accountant{Totals: t, started: now}
}

func phase(state string) string {
	switch state {
	case "ready-to-drive":
		return "driving"
	case "parked", "hop-on", "hop-on-learning", "waiting-seatbox":
		return "parked"
	case "stand-by", "hibernating", "hibernating-imminent", "hibernating-waiting":
		return "standby"
	default:
		return "unassigned"
	}
}

func readSample(fields map[string]string, now, started time.Time) (Sample, bool) {
	if fields["present"] != "true" || fields["state"] != "active" || fields["sample:valid"] != "true" || fields["serial-number"] == "" {
		return Sample{}, false
	}
	values := make([]int64, 4)
	for i, k := range []string{"sample:timestamp", "voltage", "remaining-capacity", "full-capacity"} {
		v, err := strconv.ParseInt(fields[k], 10, 64)
		if err != nil {
			return Sample{}, false
		}
		values[i] = v
	}
	s := Sample{Serial: fields["serial-number"], At: time.UnixMilli(values[0]), Voltage: values[1], Capacity: values[2], Full: values[3]}
	age := now.Sub(s.At)
	if s.At.Before(started) || age < 0 || age > maxSampleAge || s.Voltage < 20000 || s.Voltage > 65000 || s.Full <= 0 || s.Full > 100000 || s.Capacity < 0 || s.Capacity > s.Full {
		return Sample{}, false
	}
	return s, true
}

func (a *Accountant) breakSlot(i int) {
	if a.slots[i] != nil {
		a.Totals.Gaps++
		a.slots[i] = nil
	}
}

func (a *Accountant) Break() {
	for i := range a.slots {
		a.breakSlot(i)
	}
}

// Observe consumes one atomic Redis snapshot. Acquisition timestamps prevent
// repeated cached reads and state-only publications from creating intervals.
func (a *Accountant) Observe(now time.Time, vehicle map[string]string, packs [2]map[string]string) string {
	if packs[0]["present"] == "true" && packs[1]["present"] == "true" && packs[0]["serial-number"] != "" && packs[0]["serial-number"] == packs[1]["serial-number"] {
		a.Break()
		return "waiting"
	}
	token := vehicle["state"] + ":" + vehicle["state:timestamp"]
	if token != a.phaseToken {
		for _, b := range a.slots {
			if b != nil {
				b.mixed = true
			}
		}
		a.phaseToken = token
	}
	p := phase(vehicle["state"])
	present, usable := 0, 0
	for i, fields := range packs {
		if fields["present"] == "true" && fields["state"] != "idle" && fields["state"] != "asleep" {
			present++
		}
		s, ok := readSample(fields, now, a.started)
		if !ok {
			a.breakSlot(i)
			continue
		}
		usable++
		old := a.slots[i]
		if old == nil {
			a.slots[i] = &baseline{Sample: s, phase: p}
			continue
		}
		if s.Serial != old.Serial || s.Full != old.Full {
			a.breakSlot(i)
			a.slots[i] = &baseline{Sample: s, phase: p}
			continue
		}
		if s.At.Equal(old.At) {
			continue
		}
		dt := s.At.Sub(old.At)
		delta := old.Capacity - s.Capacity
		// Reject gauge resets/corrections larger than a 200 A pack could produce,
		// with a 2 mAh quantisation allowance. This is validation, not a power model.
		limit := 200000*dt.Hours() + 2
		if dt <= 0 || dt > maxInterval || math.Abs(float64(delta)) > limit {
			a.breakSlot(i)
			a.slots[i] = &baseline{Sample: s, phase: p}
			continue
		}
		mwh := float64(delta) * float64(old.Voltage+s.Voltage) / 2000
		bucket := p
		if old.mixed || old.phase != p {
			bucket = "unassigned"
		}
		switch bucket {
		case "driving":
			a.Totals.Driving += mwh
		case "parked":
			a.Totals.Parked += mwh
		case "standby":
			a.Totals.Standby += mwh
		default:
			a.Totals.Unassigned += mwh
		}
		a.Totals.Intervals++
		a.slots[i] = &baseline{Sample: s, phase: p}
	}
	if usable == 0 {
		return "waiting"
	}
	if usable < present {
		return "partial"
	}
	return "collecting"
}

func (a *Accountant) Fields(now time.Time, status string) map[string]any {
	f := map[string]any{
		"source": "bms-capacity-delta", "status": status, "since": a.Totals.Since.UTC().Format(time.RFC3339),
		"updated-at": strconv.FormatInt(now.UnixMilli(), 10), "intervals": strconv.FormatUint(a.Totals.Intervals, 10),
		"gaps": strconv.FormatUint(a.Totals.Gaps, 10), "known": strconv.FormatBool(a.Totals.Intervals > 0),
	}
	for k, v := range map[string]float64{"net": a.Totals.Net(), "driving": a.Totals.Driving, "parked": a.Totals.Parked, "standby": a.Totals.Standby, "unassigned": a.Totals.Unassigned} {
		f[k+":mwh"] = strconv.FormatFloat(v, 'f', 3, 64)
	}
	return f
}
