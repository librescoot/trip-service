package energy

import (
	"testing"
	"time"
)

func TestLargestPhaseShare(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []struct {
			at    int
			state string
		}
		driving, parked, standby, unassigned float64
	}{
		{name: "new phase dominates", changes: []struct {
			at    int
			state string
		}{{10, "ready-to-drive"}}, driving: 5300},
		{name: "old phase dominates", changes: []struct {
			at    int
			state string
		}{{30, "ready-to-drive"}}, standby: 5300},
		{name: "tie", changes: []struct {
			at    int
			state string
		}{{20, "ready-to-drive"}}, unassigned: 5300},
		{name: "repeated phase is aggregated", changes: []struct {
			at    int
			state string
		}{{2, "ready-to-drive"}, {14, "parked"}, {25, "ready-to-drive"}}, driving: 5300},
		{name: "largest need not exceed half", changes: []struct {
			at    int
			state string
		}{{18, "parked"}, {30, "ready-to-drive"}}, standby: 5300},
		{name: "unknown dominates", changes: []struct {
			at    int
			state string
		}{{5, "unknown"}}, unassigned: 5300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := accountant()
			old := pack(epoch, "A", 20000)
			a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{old, nil})
			state := "stand-by"
			for _, change := range tc.changes {
				state = change.state
				a.Observe(epoch.Add(time.Duration(change.at)*time.Second), vehicle(state), [2]map[string]string{old, nil})
			}
			end := epoch.Add(40 * time.Second)
			a.Observe(end, vehicle(state), [2]map[string]string{pack(end, "A", 19900), nil})
			closeTo(t, a.Totals.Net(), 5300)
			closeTo(t, a.Totals.Driving, tc.driving)
			closeTo(t, a.Totals.Parked, tc.parked)
			closeTo(t, a.Totals.Standby, tc.standby)
			closeTo(t, a.Totals.Unassigned, tc.unassigned)
			if a.Totals.Intervals != 1 {
				t.Fatal(a.Totals)
			}
		})
	}
}

func TestPhaseTransitionTimestampAndDelayedSamples(t *testing.T) {
	t.Run("transition precedes observation", func(t *testing.T) {
		a := accountant()
		old := pack(epoch, "A", 20000)
		a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{old, nil})
		v := vehicle("ready-to-drive")
		v["state:timestamp"] = epoch.Add(10 * time.Second).Format(time.RFC3339Nano)
		a.Observe(epoch.Add(30*time.Second), v, [2]map[string]string{old, nil})
		end := epoch.Add(40 * time.Second)
		a.Observe(end, v, [2]map[string]string{pack(end, "A", 19900), nil})
		closeTo(t, a.Totals.Driving, 5300)
	})
	t.Run("sample precedes latest transition", func(t *testing.T) {
		a := accountant()
		old := pack(epoch, "A", 20000)
		a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{old, nil})
		v := vehicle("ready-to-drive")
		v["state:timestamp"] = epoch.Add(45 * time.Second).Format(time.RFC3339Nano)
		a.Observe(epoch.Add(50*time.Second), v, [2]map[string]string{pack(epoch.Add(40*time.Second), "A", 19900), nil})
		closeTo(t, a.Totals.Standby, 5300)
		closeTo(t, a.Totals.Unassigned, 0)
	})
	t.Run("different states in same bucket", func(t *testing.T) {
		a := accountant()
		old := pack(epoch, "A", 20000)
		a.Observe(epoch, vehicle("parked"), [2]map[string]string{old, nil})
		a.Observe(epoch.Add(20*time.Second), vehicle("hop-on"), [2]map[string]string{old, nil})
		end := epoch.Add(40 * time.Second)
		a.Observe(end, vehicle("hop-on"), [2]map[string]string{pack(end, "A", 20100), nil})
		closeTo(t, a.Totals.Parked, -5300)
		closeTo(t, a.Totals.Net(), -5300)
	})
}

func TestPhaseHistoryRetentionAndUnknownExcursion(t *testing.T) {
	var timeline phaseTimeline
	for i := 0; i <= 1000; i++ {
		state := "stand-by"
		if i%2 == 1 {
			state = "ready-to-drive"
		}
		timeline.observe(epoch.Add(time.Duration(i)*time.Second), epoch, vehicle(state))
	}
	if len(timeline.changes) > 212 {
		t.Fatalf("unbounded history: %d", len(timeline.changes))
	}
	// The oldest usable interval can end 90 seconds ago and span 120 seconds.
	if got := timeline.largestShare(epoch.Add(790*time.Second), epoch.Add(910*time.Second)); got != "unassigned" {
		t.Fatal(got)
	}
	if got := timeline.largestShare(epoch.Add(791*time.Second), epoch.Add(910*time.Second)); got != "driving" {
		t.Fatal(got)
	}

	timeline = phaseTimeline{}
	timeline.observe(epoch, epoch, vehicle("stand-by"))
	v := vehicle("stand-by")
	v["state:timestamp"] = epoch.Add(30 * time.Second).Format(time.RFC3339Nano)
	timeline.observe(epoch.Add(40*time.Second), epoch, v)
	if got := timeline.largestShare(epoch, epoch.Add(40*time.Second)); got != "unassigned" {
		t.Fatal(got)
	}
	if got := timeline.largestShare(epoch.Add(30*time.Second), epoch.Add(40*time.Second)); got != "standby" {
		t.Fatal(got)
	}
}

func TestInvalidTransitionTimeUsesObservation(t *testing.T) {
	for _, timestamp := range []string{"", "bad", epoch.Add(time.Hour).Format(time.RFC3339Nano), epoch.Add(-time.Second).Format(time.RFC3339Nano)} {
		t.Run(timestamp, func(t *testing.T) {
			var timeline phaseTimeline
			timeline.observe(epoch, epoch, vehicle("stand-by"))
			v := vehicle("ready-to-drive")
			v["state:timestamp"] = timestamp
			timeline.observe(epoch.Add(30*time.Second), epoch, v)
			if got := timeline.largestShare(epoch, epoch.Add(40*time.Second)); got != "standby" {
				t.Fatal(got)
			}
		})
	}
}

func TestEachPackUsesItsOwnInterval(t *testing.T) {
	a := accountant()
	first := pack(epoch, "A", 20000)
	a.Observe(epoch, vehicle("stand-by"), [2]map[string]string{first, nil})
	second := pack(epoch.Add(20*time.Second), "B", 20000)
	a.Observe(epoch.Add(20*time.Second), vehicle("stand-by"), [2]map[string]string{first, second})
	a.Observe(epoch.Add(25*time.Second), vehicle("ready-to-drive"), [2]map[string]string{first, second})
	first = pack(epoch.Add(40*time.Second), "A", 19900)
	a.Observe(epoch.Add(40*time.Second), vehicle("ready-to-drive"), [2]map[string]string{first, second})
	second = pack(epoch.Add(60*time.Second), "B", 19900)
	a.Observe(epoch.Add(60*time.Second), vehicle("ready-to-drive"), [2]map[string]string{first, second})
	closeTo(t, a.Totals.Standby, 5300)
	closeTo(t, a.Totals.Driving, 5300)
	closeTo(t, a.Totals.Net(), 10600)
	if a.Totals.Intervals != 2 {
		t.Fatal(a.Totals)
	}
}
