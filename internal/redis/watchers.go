package redis

import (
	"log/slog"
	"strconv"

	ipc "github.com/librescoot/redis-ipc"
)

type TripCallbacks struct {
	OnVehicleState func(state string) error
	OnSpeedUpdate  func(speedKmh float64) error
	OnOdometer     func(meters int64) error
	OnGPSUpdate    func(lat, lon, altitude, speed, course float64) error
	OnProfileID    func(id string) error
}

type Watchers struct {
	vehicle *ipc.HashWatcher
	engine  *ipc.HashWatcher
	gps     *ipc.HashWatcher
	profile *ipc.HashWatcher
	log     *slog.Logger
}

func NewWatchers(client *ipc.Client, cb *TripCallbacks, log *slog.Logger) *Watchers {
	w := &Watchers{log: log}

	w.vehicle = client.NewHashWatcher("vehicle")
	w.vehicle.OnField("state", func(value string) error {
		return cb.OnVehicleState(value)
	})

	w.engine = client.NewHashWatcher("engine-ecu")
	w.engine.OnField("speed", func(value string) error {
		speed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil
		}
		return cb.OnSpeedUpdate(speed)
	})
	w.engine.OnField("odometer", func(value string) error {
		odo, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil
		}
		return cb.OnOdometer(odo)
	})

	w.gps = client.NewHashWatcher("gps")
	w.gps.OnAny(func(field, value string) error {
		lat, _ := w.gps.Fetch("latitude")
		lon, _ := w.gps.Fetch("longitude")
		alt, _ := w.gps.Fetch("altitude")
		spd, _ := w.gps.Fetch("speed")
		crs, _ := w.gps.Fetch("course")

		latF, err1 := strconv.ParseFloat(lat, 64)
		lonF, err2 := strconv.ParseFloat(lon, 64)
		if err1 != nil || err2 != nil {
			return nil
		}
		altF, _ := strconv.ParseFloat(alt, 64)
		spdF, _ := strconv.ParseFloat(spd, 64)
		crsF, _ := strconv.ParseFloat(crs, 64)

		return cb.OnGPSUpdate(latF, lonF, altF, spdF, crsF)
	})

	w.profile = client.NewHashWatcher("profile")
	w.profile.OnField("active.id", func(value string) error {
		return cb.OnProfileID(value)
	})

	return w
}

func (w *Watchers) Start() error {
	if err := w.vehicle.StartWithSync(); err != nil {
		return err
	}
	if err := w.engine.Start(); err != nil {
		return err
	}
	if err := w.gps.Start(); err != nil {
		return err
	}
	if err := w.profile.Start(); err != nil {
		return err
	}
	return nil
}

func (w *Watchers) Stop() {
	w.vehicle.Stop()
	w.engine.Stop()
	w.gps.Stop()
	w.profile.Stop()
}
