package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/trip-service/internal/counter"
	"github.com/librescoot/trip-service/internal/db"
	"github.com/librescoot/trip-service/internal/recorder"
	tripRedis "github.com/librescoot/trip-service/internal/redis"
)

type Config struct {
	RedisAddr, DBPath string
	Logger            *slog.Logger
}
type App struct {
	cfg                                              *Config
	log                                              *slog.Logger
	store                                            *db.Store
	recorder                                         *recorder.Recorder
	counter                                          *counter.Counter
	watchers                                         *tripRedis.Watchers
	pubs                                             *tripRedis.Publishers
	commandQueue                                     *ipc.QueueHandler[string]
	mu                                               sync.Mutex
	counterOpMu                                      sync.Mutex
	profileID                                        string
	lastLat, lastLon, lastAlt, lastSpeed, lastCourse float64
	lastOdo                                          int64
	odoKnown                                         bool
	vehicleState                                     string
	rideStartPending                                 bool
	engineOdometer                                   func() (int64, bool)
	batteries                                        [2]counter.Battery
	dualBattery                                      bool
	flushTicker                                      *time.Ticker
	expungeMu                                        sync.Mutex
	expungePolicy                                    db.ExpungePolicy
	expungeLastRun                                   int64
	expungeLastError                                 string
	expungeRunError                                  string
	vacuumMu                                         sync.Mutex
	vacuumCancel                                     context.CancelFunc
	vacuumVehicleReady                               bool
	migrateVacuum                                    func(context.Context) error
}

func New(cfg *Config) *App { return &App{cfg: cfg, log: cfg.Logger} }
func (a *App) Run(ctx context.Context) error {
	a.log.Info("starting trip-service", "redis", a.cfg.RedisAddr, "db", a.cfg.DBPath)
	store, err := db.Open(a.cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	a.store = store
	client, err := ipc.New(ipc.WithURL(a.cfg.RedisAddr), ipc.WithCodec(ipc.StringCodec{}), ipc.WithLogger(a.log))
	if err != nil {
		return fmt.Errorf("create redis client: %w", err)
	}
	defer client.Close()
	a.pubs = tripRedis.NewPublishers(client)
	a.recorder = recorder.New(store, a.pubs)
	a.recorder.SetOnFinished(func() { a.runExpunge() })
	a.counter = counter.New(store)
	a.expungePolicy = db.ExpungePolicy{Kind: "never"}
	a.engineOdometer = func() (int64, bool) {
		value, err := client.HGet("engine-ecu", "odometer")
		if err != nil {
			return 0, false
		}
		odometer, err := strconv.ParseInt(value, 10, 64)
		return odometer, err == nil && odometer >= 0
	}
	// Reconcile the synchronous odometer while persisted active state still
	// exists; then discard only the unknowable downtime duration.
	engine, _ := client.HGetAll("engine-ecu")
	if odo, e := strconv.ParseInt(engine["odometer"], 10, 64); e == nil && odo >= 0 {
		a.lastOdo, a.odoKnown = odo, true
	}
	if err := a.counter.Hydrate(a.lastOdo, a.odoKnown); err != nil {
		return fmt.Errorf("hydrate counter: %w", err)
	}
	if a.odoKnown {
		if err := a.counter.Odometer(a.lastOdo); err != nil {
			return fmt.Errorf("reconcile counter odometer: %w", err)
		}
	}
	settings, settingsErr := client.HGetAll("settings")
	if settingsErr != nil {
		a.log.Warn("read settings", "error", settingsErr)
		settings = nil
	}
	a.dualBattery = parseBool(settings["scooter.dual-battery"])
	for slot := 0; slot < 2; slot++ {
		b, _ := client.HGetAll("battery:" + strconv.Itoa(slot))
		a.batteries[slot] = counter.Battery{Present: parseBool(b["present"]), Serial: b["serial-number"]}
	}
	if err := a.counter.ConfigureDualBattery(a.dualBattery); err != nil {
		return fmt.Errorf("hydrate dual battery: %w", err)
	}
	if settingsErr == nil {
		if raw, ok := settings["trip.counter-reset"]; ok {
			if err := a.counter.Policy(raw); err != nil {
				a.log.Warn("invalid counter reset policy", "error", err)
			}
		} else if err := a.counter.Policy(counter.PolicyRide); err != nil {
			return fmt.Errorf("hydrate default counter policy: %w", err)
		}
	}
	a.hydrateExpungePolicy(settings, settingsErr)
	if err := a.seedConfiguredBatteries(); err != nil {
		return fmt.Errorf("hydrate batteries: %w", err)
	}
	vehicle, _ := client.HGetAll("vehicle")
	vehicleState := vehicle["state"]
	vehicleReady := vehicleState == "ready-to-drive"
	a.mu.Lock()
	a.vehicleState = vehicleState
	a.mu.Unlock()
	a.vacuumMu.Lock()
	a.vacuumVehicleReady = vehicleReady
	a.vacuumMu.Unlock()
	if err := a.counter.RecoverVehicleState(vehicleState); err != nil {
		return fmt.Errorf("hydrate vehicle: %w", err)
	}
	if err := a.publishCounter(); err != nil {
		return fmt.Errorf("publish counter: %w", err)
	}
	if err := a.recorder.RecoverFromState(vehicleState); err != nil {
		a.log.Warn("crash recovery failed", "error", err)
	}
	if pid, e := client.HGet("profile", "active.id"); e == nil {
		a.profileID = pid
	}
	a.watchers = tripRedis.NewWatchers(client, &tripRedis.TripCallbacks{OnVehicleState: a.handleVehicleState, OnSpeedUpdate: a.handleSpeed, OnOdometer: a.handleOdometer, OnGPSUpdate: a.handleGPS, OnProfileID: a.handleProfileID, OnResetPolicy: a.handleResetPolicy, OnExpungePolicy: a.handleExpungePolicy, OnDualBattery: a.handleDualBattery, OnBattery: a.handleBattery}, a.log)
	if err := a.watchers.Start(); err != nil {
		return fmt.Errorf("start watchers: %w", err)
	}
	a.commandQueue = ipc.HandleRequests(client, "scooter:trip", a.handleCommand)
	a.flushTicker = time.NewTicker(30 * time.Second)
	go func() {
		for range a.flushTicker.C {
			a.recorder.FlushPoints()
			if err := a.recorder.Checkpoint(); err != nil {
				a.log.Error("checkpoint trip", "error", err)
			}
			a.counterOpMu.Lock()
			if err := a.counter.Checkpoint(); err != nil {
				a.log.Error("checkpoint counter", "error", err)
			} else if err := a.publishCounter(); err != nil {
				a.log.Error("publish counter", "error", err)
			}
			if err := a.pubs.PublishReady(); err != nil {
				a.log.Error("refresh trip ready", "error", err)
			}
			a.counterOpMu.Unlock()
			a.runExpunge()
		}
	}()
	a.runExpunge()
	if vehicleReady && !a.recorder.IsRecording() && !a.startNewTrip() {
		a.mu.Lock()
		a.rideStartPending = true
		a.mu.Unlock()
	}
	if err := a.pubs.PublishReady(); err != nil {
		return fmt.Errorf("publish trip ready: %w", err)
	}
	a.log.Info("trip-service ready")
	<-ctx.Done()
	a.log.Info("shutting down")
	a.flushTicker.Stop()
	a.commandQueue.Stop()
	a.watchers.Stop()
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	if err := a.counter.Checkpoint(); err != nil {
		a.log.Warn("checkpoint counter", "error", err)
	}
	if err := a.publishCounter(); err != nil {
		a.log.Warn("publish counter", "error", err)
	}
	if err := a.pubs.ClearReady(); err != nil {
		a.log.Warn("clear trip ready", "error", err)
	}
	if err := a.recorder.Checkpoint(); err != nil {
		a.log.Warn("checkpoint trip", "error", err)
	}
	a.recorder.FlushPoints()
	return nil
}
func (a *App) handleVehicleState(state string) error {
	ready := state == "ready-to-drive"
	a.vacuumMu.Lock()
	a.vacuumVehicleReady = ready
	cancel := a.vacuumCancel
	a.vacuumMu.Unlock()
	if ready && cancel != nil {
		cancel()
	}
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	a.log.Info("vehicle state", "state", state)
	a.mu.Lock()
	a.vehicleState = state
	if !ready {
		a.rideStartPending = false
	}
	a.mu.Unlock()
	var odometer int64
	var odometerValid bool
	if state != "ready-to-drive" {
		a.mu.Lock()
		odometer, odometerValid = a.lastOdo, a.odoKnown
		a.mu.Unlock()
		if a.engineOdometer != nil {
			if value, ok := a.engineOdometer(); ok {
				odometer, odometerValid = value, true
				a.mu.Lock()
				a.lastOdo, a.odoKnown = value, true
				a.mu.Unlock()
			}
		}
	}
	if err := a.counter.VehicleStateWithOdometer(state, odometer, odometerValid); err != nil {
		return err
	}
	if err := a.publishCounter(); err != nil {
		return err
	}
	switch state {
	case "ready-to-drive":
		if a.recorder.IsRecording() {
			if err := a.recorder.ResumeTrip(); err != nil {
				return err
			}
			a.mu.Lock()
			a.rideStartPending = false
			a.mu.Unlock()
		} else if !a.startNewTrip() {
			a.mu.Lock()
			a.rideStartPending = true
			a.mu.Unlock()
		}
	case "parked", "hop-on", "hop-on-learning":
		if err := a.recorder.PauseTrip(); err != nil {
			return err
		}
	default:
		if a.recorder.IsRecording() {
			a.mu.Lock()
			lat, lon, odo := a.lastLat, a.lastLon, a.lastOdo
			a.mu.Unlock()
			if err := a.recorder.EndTrip(lat, lon, odo); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *App) publishCounter() error {
	if a.pubs == nil {
		return nil
	}
	return a.pubs.PublishCounter(a.counter.Snapshot())
}

func (a *App) handleSpeed(v float64) error { a.mu.Lock(); a.lastSpeed = v; a.mu.Unlock(); return nil }
func (a *App) handleOdometer(v int64) error {
	if v < 0 {
		return fmt.Errorf("invalid negative odometer: %d", v)
	}
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	a.mu.Lock()
	a.lastOdo, a.odoKnown = v, true
	startPending := a.rideStartPending && a.vehicleState == "ready-to-drive"
	if startPending {
		a.rideStartPending = false
	}
	a.mu.Unlock()
	if err := a.counter.Odometer(v); err != nil {
		return err
	}
	if startPending && !a.recorder.IsRecording() && !a.startNewTrip() {
		a.mu.Lock()
		a.rideStartPending = true
		a.mu.Unlock()
	}
	return a.publishCounter()
}
func (a *App) handleGPS(lat, lon, alt, speed, course float64) error {
	a.mu.Lock()
	a.lastLat, a.lastLon, a.lastAlt, a.lastCourse = lat, lon, alt, course
	odo := a.lastOdo
	a.mu.Unlock()
	if a.recorder.IsCollecting() && speed >= 5 {
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
func (a *App) handleResetPolicy(policy string) error {
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	if err := a.counter.Policy(policy); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(policy), counter.PolicyBattery) {
		if err := a.seedConfiguredBatteries(); err != nil {
			return err
		}
	}
	return a.publishCounter()
}
func (a *App) handleExpungePolicy(policy string) error {
	if err := a.setExpungePolicy(policy); err != nil {
		a.log.Warn("invalid trip expunge policy", "error", err)
		return nil
	}
	a.runExpunge()
	return nil
}

func (a *App) handleDualBattery(enabled bool) error {
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	a.mu.Lock()
	a.dualBattery = enabled
	a.mu.Unlock()
	if err := a.counter.ConfigureDualBattery(enabled); err != nil {
		return err
	}
	if enabled {
		if err := a.seedBattery(1); err != nil {
			return err
		}
	}
	return a.publishCounter()
}
func (a *App) handleBattery(slot int, present bool, serial string) error {
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	a.mu.Lock()
	a.batteries[slot] = counter.Battery{Present: present, Serial: serial}
	a.mu.Unlock()
	if err := a.counter.Battery(slot, present, serial); err != nil {
		return err
	}
	return a.publishCounter()
}
func (a *App) seedConfiguredBatteries() error {
	if err := a.seedBattery(0); err != nil {
		return err
	}
	a.mu.Lock()
	dual := a.dualBattery
	a.mu.Unlock()
	if dual {
		return a.seedBattery(1)
	}
	return nil
}
func (a *App) seedBattery(slot int) error {
	a.mu.Lock()
	b := a.batteries[slot]
	a.mu.Unlock()
	return a.counter.SeedBattery(slot, b.Present, b.Serial)
}

type counterCommand struct {
	ID        string `json:"id"`
	Op        string `json:"op"`
	Source    string `json:"source"`
	ExpiresAt int64  `json:"expires-at"`
}

func (a *App) handleCommand(raw string) error {
	a.counterOpMu.Lock()
	defer a.counterOpMu.Unlock()
	var cmd counterCommand
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return a.pubs.PublishCommandResult("", "counter.reset", "error", "invalid")
	}
	if cmd.Op != "counter.reset" {
		return a.pubs.PublishCommandResult(cmd.ID, cmd.Op, "error", "invalid")
	}
	now := time.Now().UnixMilli()
	if expiredCommandDeadline(cmd.ExpiresAt, now) {
		// A timed-out retry may reuse the same id with a fresh deadline. Do not
		// let this stale attempt race the retry's correlated result.
		return nil
	}
	if !validCommandDeadline(cmd.ExpiresAt, now) {
		return a.pubs.PublishCommandResult(cmd.ID, cmd.Op, "error", "timeout")
	}
	snap, result, err := a.counter.ManualReset(cmd.ID)
	if err != nil {
		publishErr := a.pubs.PublishCommandResult(cmd.ID, cmd.Op, "error", "internal")
		if publishErr != nil {
			return fmt.Errorf("counter reset: %w; publish internal result: %v", err, publishErr)
		}
		return nil
	}
	return a.pubs.PublishCounterAndCommandResult(snap, result.ID, result.Op, result.Status, result.Error)
}
func expiredCommandDeadline(expiresAt, now int64) bool {
	return expiresAt > 0 && expiresAt <= now
}

func validCommandDeadline(expiresAt, now int64) bool {
	return expiresAt > now && expiresAt <= now+int64(time.Minute/time.Millisecond)
}

func (a *App) startNewTrip() bool {
	a.mu.Lock()
	pid := a.profileID
	lat, lon, odo, odoKnown := a.lastLat, a.lastLon, a.lastOdo, a.odoKnown
	a.mu.Unlock()
	if !odoKnown {
		a.log.Warn("deferring trip start until odometer baseline is available")
		return false
	}
	if err := a.recorder.StartTrip(pid, lat, lon, odo); err != nil {
		a.log.Error("start trip", "error", err)
		return false
	}
	return true
}

// hydrateExpungePolicy fails closed: only an absent setting selects the
// retention default. Read failures and malformed persisted values retain never.
func (a *App) hydrateExpungePolicy(settings map[string]string, settingsErr error) {
	if settingsErr != nil {
		a.setStartupExpungeError("settings unavailable")
		return
	}
	raw, ok := settings["trip.expunge"]
	if !ok {
		a.expungeMu.Lock()
		a.expungePolicy = db.DefaultExpungePolicy()
		a.expungeLastError, a.expungeRunError = "", ""
		a.publishExpungeLocked("idle", 0, 0, 0)
		a.expungeMu.Unlock()
		return
	}
	policy, err := db.ParseExpungePolicy(raw)
	if err != nil {
		a.setStartupExpungeError(err.Error())
		return
	}
	a.expungeMu.Lock()
	a.expungePolicy = policy
	a.expungeLastError, a.expungeRunError = "", ""
	a.publishExpungeLocked("idle", 0, 0, 0)
	a.expungeMu.Unlock()
}

func (a *App) setStartupExpungeError(message string) {
	a.expungeMu.Lock()
	defer a.expungeMu.Unlock()
	a.expungePolicy = db.ExpungePolicy{Kind: "never"}
	a.expungeLastError, a.expungeRunError = message, ""
	a.publishExpungeLocked("error", 0, 0, 0)
}

func (a *App) setExpungePolicy(value string) error {
	policy, err := db.ParseExpungePolicy(value)
	a.expungeMu.Lock()
	defer a.expungeMu.Unlock()
	if err != nil {
		a.expungeLastError = err.Error()
		a.publishExpungeLocked("error", 0, 0, 0)
		return err
	}
	a.expungePolicy = policy
	a.expungeLastError = ""
	a.expungeRunError = ""
	a.publishExpungeLocked("idle", 0, 0, 0)
	return nil
}

// runExpunge performs at most one batch. The periodic trigger supplies later
// batches and the shared lock prevents a recording trip starting mid-pass.
func (a *App) runExpunge() {
	a.expungeMu.Lock()
	defer a.expungeMu.Unlock()
	if a.recorder == nil || a.store == nil {
		return
	}
	if a.recorder.IsRecording() {
		a.publishExpungeLocked("deferred", 0, 0, 0)
		return
	}
	a.publishExpungeLocked("running", 0, 0, 0)
	if a.expungePolicy.Kind == "size" {
		enabled, err := a.store.IncrementalVacuumEnabled()
		if err != nil {
			a.expungeRunError = err.Error()
			a.publishExpungeLocked("deferred", 0, 0, 0)
			return
		}
		if !enabled {
			if err := a.migrateLegacyVacuum(); err != nil {
				a.expungeRunError = err.Error()
				a.publishExpungeLocked("deferred", 0, 0, 0)
				return
			}
			if a.recorder.IsRecording() {
				a.publishExpungeLocked("deferred", 0, 0, 0)
				return
			}
		}
	}
	result, err := a.store.Expunge(a.expungePolicy, time.Now())
	if err != nil {
		a.expungeRunError = err.Error()
		a.publishExpungeLocked("deferred", result.DeletedTrips, result.DBBytes, result.WALBytes)
		a.log.Warn("expunge trips deferred", "error", err)
		return
	}
	a.expungeLastRun = time.Now().Unix()
	a.expungeRunError = ""
	if a.expungeLastError != "" {
		a.publishExpungeLocked("error", result.DeletedTrips, result.DBBytes, result.WALBytes)
		return
	}
	a.publishExpungeLocked("idle", result.DeletedTrips, result.DBBytes, result.WALBytes)
}

func (a *App) migrateLegacyVacuum() error {
	ctx, cancel := context.WithCancel(context.Background())
	a.vacuumMu.Lock()
	if a.vacuumVehicleReady {
		a.vacuumMu.Unlock()
		cancel()
		return context.Canceled
	}
	// Admission and cancellation registration are atomic with the Ready
	// transition, so a ride cannot slip into the pre-registration gap.
	a.vacuumCancel = cancel
	a.vacuumMu.Unlock()
	defer func() {
		a.vacuumMu.Lock()
		if a.vacuumCancel != nil {
			a.vacuumCancel = nil
		}
		a.vacuumMu.Unlock()
		cancel()
	}()
	if a.migrateVacuum != nil {
		return a.migrateVacuum(ctx)
	}
	return a.store.MigrateToIncrementalVacuum(ctx)
}

func (a *App) publishExpungeLocked(status string, deleted, mainBytes, walBytes int64) {
	if a.pubs == nil {
		return
	}
	lastError := a.expungeLastError
	if lastError == "" {
		lastError = a.expungeRunError
	}
	if err := a.pubs.PublishExpunge(tripRedis.ExpungeSnapshot{Policy: a.expungePolicy.Kind, Value: a.expungePolicy.Operand(), Status: status, LastRun: a.expungeLastRun, LastError: lastError, DeletedTrips: deleted, DBBytes: mainBytes, WALBytes: walBytes, UpdatedAt: time.Now().Unix()}); err != nil {
		a.log.Warn("publish trip expunge", "error", err)
	}
}

func parseBool(v string) bool { return v == "1" || strings.EqualFold(v, "true") }
