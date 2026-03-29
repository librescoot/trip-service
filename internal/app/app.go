package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/librescoot/trip-service/internal/db"
	"github.com/librescoot/trip-service/internal/recorder"
	tripRedis "github.com/librescoot/trip-service/internal/redis"
	ipc "github.com/librescoot/redis-ipc"
)

type Config struct {
	RedisAddr string
	DBPath    string
	Logger    *slog.Logger
}

type App struct {
	cfg      *Config
	log      *slog.Logger
	store    *db.Store
	recorder *recorder.Recorder
	watchers *tripRedis.Watchers
	pubs     *tripRedis.Publishers

	mu          sync.Mutex
	profileID   string
	lastLat     float64
	lastLon     float64
	lastAlt     float64
	lastSpeed   float64
	lastCourse  float64
	lastOdo     int64
	flushTicker *time.Ticker
}

func New(cfg *Config) *App {
	return &App{
		cfg: cfg,
		log: cfg.Logger,
	}
}

func (a *App) Run(ctx context.Context) error {
	a.log.Info("starting trip-service", "redis", a.cfg.RedisAddr, "db", a.cfg.DBPath)

	store, err := db.Open(a.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	a.store = store

	client, err := ipc.New(
		ipc.WithURL(a.cfg.RedisAddr),
		ipc.WithCodec(ipc.StringCodec{}),
		ipc.WithLogger(a.log),
	)
	if err != nil {
		return fmt.Errorf("create redis client: %w", err)
	}
	defer client.Close()

	a.pubs = tripRedis.NewPublishers(client)
	a.recorder = recorder.New(store, a.pubs)

	vehicleState, _ := client.HGet("vehicle", "state")
	vehicleReady := vehicleState == "ready-to-drive"
	if err := a.recorder.RecoverFromCrash(vehicleReady); err != nil {
		a.log.Warn("crash recovery failed", "error", err)
	}

	if pid, err := client.HGet("profile", "active.id"); err == nil {
		a.profileID = pid
	}

	a.watchers = tripRedis.NewWatchers(client, &tripRedis.TripCallbacks{
		OnVehicleState: a.handleVehicleState,
		OnSpeedUpdate:  a.handleSpeed,
		OnOdometer:     a.handleOdometer,
		OnGPSUpdate:    a.handleGPS,
		OnProfileID:    a.handleProfileID,
	}, a.log)

	if err := a.watchers.Start(); err != nil {
		return fmt.Errorf("start watchers: %w", err)
	}

	a.flushTicker = time.NewTicker(30 * time.Second)
	go func() {
		for range a.flushTicker.C {
			a.recorder.FlushPoints()
		}
	}()

	if vehicleReady && !a.recorder.IsRecording() {
		a.startNewTrip()
	}

	a.log.Info("trip-service ready")
	<-ctx.Done()

	a.log.Info("shutting down")
	a.flushTicker.Stop()
	a.watchers.Stop()
	a.recorder.FlushPoints()
	return nil
}

func (a *App) handleVehicleState(state string) error {
	a.log.Info("vehicle state", "state", state)

	switch state {
	case "ready-to-drive":
		if !a.recorder.IsRecording() {
			a.startNewTrip()
		}
	default:
		if a.recorder.IsRecording() {
			a.mu.Lock()
			lat, lon, odo := a.lastLat, a.lastLon, a.lastOdo
			a.mu.Unlock()
			if err := a.recorder.EndTrip(lat, lon, odo); err != nil {
				a.log.Error("end trip", "error", err)
			}
		}
	}
	return nil
}

func (a *App) handleSpeed(speedKmh float64) error {
	a.mu.Lock()
	a.lastSpeed = speedKmh
	a.mu.Unlock()
	return nil
}

func (a *App) handleOdometer(meters int64) error {
	a.mu.Lock()
	a.lastOdo = meters
	a.mu.Unlock()
	return nil
}

func (a *App) handleGPS(lat, lon, alt, speed, course float64) error {
	a.mu.Lock()
	a.lastLat = lat
	a.lastLon = lon
	a.lastAlt = alt
	a.lastCourse = course
	odo := a.lastOdo
	a.mu.Unlock()

	if a.recorder.IsRecording() && speed >= 5 {
		a.recorder.AddPoint(lat, lon, alt, speed, course, odo, time.Now().UnixMilli())
	}

	return nil
}

func (a *App) handleProfileID(id string) error {
	a.mu.Lock()
	a.profileID = id
	a.mu.Unlock()
	return nil
}

func (a *App) startNewTrip() {
	a.mu.Lock()
	pid := a.profileID
	lat, lon, odo := a.lastLat, a.lastLon, a.lastOdo
	a.mu.Unlock()

	if err := a.recorder.StartTrip(pid, lat, lon, odo); err != nil {
		a.log.Error("start trip", "error", err)
	}
}
