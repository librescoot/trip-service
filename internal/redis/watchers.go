package redis

import (
	"log/slog"
	"strconv"
	"strings"

	ipc "github.com/librescoot/redis-ipc"
)

type TripCallbacks struct {
	OnVehicleState  func(state string) error
	OnSpeedUpdate   func(speedKmh float64) error
	OnOdometer      func(meters int64) error
	OnGPSUpdate     func(lat, lon, altitude, speed, course float64) error
	OnProfileID     func(id string) error
	OnResetPolicy   func(policy string) error
	OnExpungePolicy func(policy string) error
	OnDualBattery   func(enabled bool) error
	OnBattery       func(slot int, present bool, serial string) error
}

type Watchers struct {
	vehicle, engine, gps, profile, settings *ipc.HashWatcher
	battery                                 [2]*ipc.HashWatcher
	log                                     *slog.Logger
}

func NewWatchers(client *ipc.Client, cb *TripCallbacks, log *slog.Logger) *Watchers {
	w := &Watchers{log: log}
	w.vehicle = client.NewHashWatcher("vehicle")
	w.vehicle.OnField("state", func(v string) error { return cb.OnVehicleState(v) })
	w.engine = client.NewHashWatcher("engine-ecu")
	w.engine.OnField("speed", func(v string) error {
		n, e := strconv.ParseFloat(v, 64)
		if e != nil {
			return nil
		}
		return cb.OnSpeedUpdate(n)
	})
	w.engine.OnField("odometer", func(v string) error {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n < 0 {
			return nil
		}
		return cb.OnOdometer(n)
	})
	w.gps = client.NewHashWatcher("gps")
	w.gps.OnAny(func(field, value string) error {
		lat, _ := w.gps.Fetch("latitude")
		lon, _ := w.gps.Fetch("longitude")
		alt, _ := w.gps.Fetch("altitude")
		spd, _ := w.gps.Fetch("speed")
		crs, _ := w.gps.Fetch("course")
		la, e1 := strconv.ParseFloat(lat, 64)
		lo, e2 := strconv.ParseFloat(lon, 64)
		if e1 != nil || e2 != nil {
			return nil
		}
		al, _ := strconv.ParseFloat(alt, 64)
		sp, _ := strconv.ParseFloat(spd, 64)
		co, _ := strconv.ParseFloat(crs, 64)
		return cb.OnGPSUpdate(la, lo, al, sp, co)
	})
	w.profile = client.NewHashWatcher("profile")
	w.profile.OnField("active.id", func(v string) error { return cb.OnProfileID(v) })
	w.settings = client.NewHashWatcher("settings")
	w.settings.OnField("trip.counter-reset", func(v string) error {
		if cb.OnResetPolicy == nil {
			return nil
		}
		return cb.OnResetPolicy(v)
	})
	w.settings.OnField("trip.expunge", func(v string) error {
		if cb.OnExpungePolicy == nil {
			return nil
		}
		return cb.OnExpungePolicy(v)
	})
	w.settings.OnField("scooter.dual-battery", func(v string) error {
		if cb.OnDualBattery == nil {
			return nil
		}
		return cb.OnDualBattery(strings.EqualFold(v, "true") || v == "1")
	})
	for i := range w.battery {
		slot := i
		w.battery[i] = client.NewHashWatcher("battery:" + strconv.Itoa(i))
		update := func(string) error {
			values, err := w.battery[slot].FetchAll()
			if err != nil {
				return err
			}
			present := strings.EqualFold(values["present"], "true") || values["present"] == "1"
			if cb.OnBattery == nil {
				return nil
			}
			return cb.OnBattery(slot, present, values["serial-number"])
		}
		w.battery[i].OnField("present", update)
		w.battery[i].OnField("serial-number", update)
	}
	return w
}
func (w *Watchers) Start() error {
	// Every counter input uses a synchronized snapshot so startup recovery never
	// runs against a partially observed vehicle state.
	for _, watcher := range []*ipc.HashWatcher{w.engine, w.vehicle, w.settings, w.battery[0], w.battery[1], w.gps, w.profile} {
		if err := watcher.StartWithSync(); err != nil {
			return err
		}
	}
	return nil
}
func (w *Watchers) Stop() {
	for _, watcher := range []*ipc.HashWatcher{w.vehicle, w.engine, w.gps, w.profile, w.settings, w.battery[0], w.battery[1]} {
		_ = watcher.Stop()
	}
}
