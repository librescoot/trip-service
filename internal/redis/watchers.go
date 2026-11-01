package redis

import (
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

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
	vehicle, engine, profile, settings *ipc.HashWatcher
	battery                            [2]*ipc.HashWatcher
	client                             *ipc.Client
	gpsCallback                        func(lat, lon, altitude, speed, course float64) error
	gpsSubscription                    *ipc.Subscription[string]
	lifecycleMu, gpsMu                 sync.Mutex
	started, gpsActive, gpsLogged      bool
	log                                *slog.Logger
}

func NewWatchers(client *ipc.Client, cb *TripCallbacks, log *slog.Logger) *Watchers {
	w := &Watchers{client: client, gpsCallback: cb.OnGPSUpdate, log: log}
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
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.started {
		return nil
	}
	// Counter inputs must be synchronized before live GPS callbacks begin.
	for _, watcher := range []*ipc.HashWatcher{w.engine, w.vehicle, w.settings, w.battery[0], w.battery[1], w.profile} {
		if err := watcher.StartWithSync(); err != nil {
			w.stop()
			return err
		}
	}
	w.gpsMu.Lock()
	w.gpsActive = true
	subscription, err := ipc.Subscribe[string](w.client, "gps:tpv", w.onGPSSnapshot)
	if err != nil {
		w.gpsActive = false
		w.gpsMu.Unlock()
		w.stop()
		return err
	}
	w.gpsSubscription = subscription
	w.gpsMu.Unlock()
	w.started = true
	return nil
}

func (w *Watchers) Stop() {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	w.stop()
}

func (w *Watchers) stop() {
	w.gpsMu.Lock()
	w.gpsActive = false
	subscription := w.gpsSubscription
	w.gpsSubscription = nil
	w.gpsMu.Unlock()
	if subscription != nil {
		_ = subscription.Unsubscribe()
	}
	for _, watcher := range []*ipc.HashWatcher{w.vehicle, w.engine, w.profile, w.settings, w.battery[0], w.battery[1]} {
		_ = watcher.Stop()
	}
	w.started = false
}

// gps:tpv is a live complete snapshot. GPS wall time can jump, so it is not
// used as an ordering gate; repeated positions are filtered by the recorder.
func (w *Watchers) onGPSSnapshot(payload string) error {
	w.gpsMu.Lock()
	defer w.gpsMu.Unlock()
	if !w.gpsActive || w.gpsCallback == nil {
		return nil
	}
	var sample struct {
		Active, Connected bool
		Fix, State        string
		Latitude          string
		Longitude         string
		Altitude          string
		Speed             string
		Course            string
	}
	if json.Unmarshal([]byte(payload), &sample) != nil || !sample.Active || !sample.Connected ||
		(sample.Fix != "2d" && sample.Fix != "3d") || sample.State != "fix-established" {
		return nil
	}
	values := [5]float64{}
	for i, value := range []string{sample.Latitude, sample.Longitude, sample.Altitude, sample.Speed, sample.Course} {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil
		}
		values[i] = n
	}
	if values[0] < -90 || values[0] > 90 || values[1] < -180 || values[1] > 180 ||
		(values[0] == 0 && values[1] == 0) || values[3] < 0 {
		return nil
	}
	err := w.gpsCallback(values[0], values[1], values[2], values[3], values[4])
	if err == nil && !w.gpsLogged {
		w.gpsLogged = true
		if w.log != nil {
			w.log.Info("GPS snapshot intake active")
		}
	}
	return err
}
