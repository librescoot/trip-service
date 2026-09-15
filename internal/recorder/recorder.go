package recorder

import (
	"log/slog"
	"sync"
	"time"

	"github.com/librescoot/trip-service/internal/db"
	"github.com/librescoot/trip-service/internal/geo"
)

type Publisher interface {
	PublishTripStatus(status, id, profileID string, distanceM, durationS int64)
	PublishTripCompleted(id string, profileID string, distanceM, durationS int64, maxSpeed float64)
	ClearTrip()
}

type Recorder struct {
	store       *db.Store
	pub         Publisher
	currentTrip *db.Trip
	lastPoint   *RecordedPoint
	pointBuffer []db.TripPoint
	maxSpeed    float64
	onFinished  func()
	now         func() time.Time
	mu          sync.Mutex
}

func New(store *db.Store, pub Publisher) *Recorder {
	return &Recorder{store: store, pub: pub, now: time.Now}
}
func (r *Recorder) SetClock(now func() time.Time) { r.mu.Lock(); defer r.mu.Unlock(); r.now = now }
func (r *Recorder) SetOnFinished(fn func())       { r.mu.Lock(); defer r.mu.Unlock(); r.onFinished = fn }
func (r *Recorder) IsRecording() bool             { r.mu.Lock(); defer r.mu.Unlock(); return r.currentTrip != nil }
func (r *Recorder) IsCollecting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentTrip != nil && r.currentTrip.ActiveStartedAt > 0
}

func (r *Recorder) StartTrip(profileID string, lat, lon float64, odometer int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip != nil {
		return nil
	}
	trip, err := r.store.CreateTrip(profileID, lat, lon, odometer)
	if err != nil {
		return err
	}
	r.currentTrip, r.maxSpeed, r.pointBuffer, r.lastPoint = trip, 0, nil, nil
	r.pub.PublishTripStatus("recording", trip.ID, profileID, 0, 0)
	return nil
}

// PauseTrip preserves the unlocked ride but stops duration and GPS collection.
func (r *Recorder) PauseTrip() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil || r.currentTrip.ActiveStartedAt == 0 {
		return nil
	}
	if len(r.pointBuffer) > 0 {
		if err := r.store.InsertPoints(r.pointBuffer); err != nil {
			return err
		}
		r.pointBuffer = nil
	}
	trip, err := r.store.PauseTrip(r.currentTrip.ID, r.now().Unix())
	if err != nil {
		return err
	}
	r.currentTrip, r.lastPoint = trip, nil
	r.pub.PublishTripStatus("paused", trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS)
	return nil
}

func (r *Recorder) ResumeTrip() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil || r.currentTrip.ActiveStartedAt > 0 {
		return nil
	}
	trip, err := r.store.ResumeTrip(r.currentTrip.ID, r.now().Unix())
	if err != nil {
		return err
	}
	r.currentTrip, r.lastPoint = trip, nil
	r.pub.PublishTripStatus("recording", trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS)
	return nil
}

func (r *Recorder) Checkpoint() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil || r.currentTrip.ActiveStartedAt == 0 {
		return nil
	}
	trip, err := r.store.CheckpointTrip(r.currentTrip.ID, r.now().Unix())
	if err == nil {
		r.currentTrip = trip
	}
	return err
}

func (r *Recorder) EndTrip(lat, lon float64, odometer int64) error {
	r.mu.Lock()
	if r.currentTrip == nil {
		r.mu.Unlock()
		return nil
	}
	if len(r.pointBuffer) > 0 {
		if err := r.store.InsertPoints(r.pointBuffer); err != nil {
			r.mu.Unlock()
			return err
		}
		r.pointBuffer = nil
	}
	trip, err := r.store.CompleteTripAt(r.currentTrip.ID, lat, lon, odometer, r.maxSpeed, r.now().Unix())
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.pub.PublishTripCompleted(trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS, trip.MaxSpeed)
	r.pub.ClearTrip()
	r.currentTrip, r.lastPoint = nil, nil
	onFinished := r.onFinished
	r.mu.Unlock()
	if onFinished != nil {
		onFinished()
	}
	return nil
}

func (r *Recorder) AddPoint(lat, lon, altitude, speed, course float64, odometer, timestampMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil || r.currentTrip.ActiveStartedAt == 0 {
		return
	}
	current := RecordedPoint{Point: geo.Point{Lat: lat, Lon: lon}, Speed: speed, Course: course}
	if r.lastPoint != nil && !ShouldRecord(*r.lastPoint, current) {
		return
	}
	if speed > r.maxSpeed {
		r.maxSpeed = speed
	}
	point := db.TripPoint{TripID: r.currentTrip.ID, Timestamp: timestampMs, Latitude: lat, Longitude: lon, Speed: &speed, Course: &course}
	if altitude != 0 {
		point.Altitude = &altitude
	}
	if odometer != 0 {
		point.Odometer = &odometer
	}
	r.pointBuffer = append(r.pointBuffer, point)
	r.lastPoint = &current
	if len(r.pointBuffer) >= 10 {
		if err := r.store.InsertPoints(r.pointBuffer); err == nil {
			r.pointBuffer = nil
		}
	}
}

func (r *Recorder) FlushPoints() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pointBuffer) > 0 && r.currentTrip != nil {
		if err := r.store.InsertPoints(r.pointBuffer); err == nil {
			r.pointBuffer = nil
		}
	}
}

// RecoverFromState preserves a paused open row and resumes a Ready interval
// from recovery time, deliberately excluding the unobserved outage.
func (r *Recorder) RecoverFromState(state string) error {
	trip, err := r.store.GetRecordingTrip()
	if err != nil || trip == nil {
		return err
	}
	if trip.ActiveStartedAt != 0 {
		trip, err = r.store.DiscardActiveTripInterval(trip.ID)
		if err != nil {
			return err
		}
	}
	if state == "ready-to-drive" {
		trip, err = r.store.ResumeTrip(trip.ID, r.now().Unix())
		if err != nil {
			return err
		}
	} else if state != "parked" && state != "hop-on" && state != "hop-on-learning" {
		slog.Info("abandoning unfinished trip", "id", trip.ID)
		if err := r.store.AbandonTrip(trip.ID); err != nil {
			return err
		}
		r.mu.Lock()
		onFinished := r.onFinished
		r.mu.Unlock()
		if onFinished != nil {
			onFinished()
		}
		return nil
	}
	r.mu.Lock()
	r.currentTrip, r.maxSpeed, r.lastPoint = trip, trip.MaxSpeed, nil
	r.mu.Unlock()
	status := "paused"
	if trip.ActiveStartedAt > 0 {
		status = "recording"
	}
	r.pub.PublishTripStatus(status, trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS)
	slog.Info("resumed recording trip", "id", trip.ID)
	return nil
}

// RecoverFromCrash is retained for callers that only have the ready snapshot.
func (r *Recorder) RecoverFromCrash(vehicleReady bool) error {
	if vehicleReady {
		return r.RecoverFromState("ready-to-drive")
	}
	return r.RecoverFromState("stand-by")
}

func (r *Recorder) CurrentTripDuration() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil {
		return 0
	}
	d := r.currentTrip.DurationS
	now := r.now().Unix()
	if r.currentTrip.ActiveStartedAt > 0 && now > r.currentTrip.ActiveStartedAt {
		d += now - r.currentTrip.ActiveStartedAt
	}
	return d
}
