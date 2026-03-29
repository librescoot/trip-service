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
	mu          sync.Mutex
}

func New(store *db.Store, pub Publisher) *Recorder {
	return &Recorder{store: store, pub: pub}
}

func (r *Recorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentTrip != nil
}

func (r *Recorder) StartTrip(profileID string, lat, lon float64, odometer int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	trip, err := r.store.CreateTrip(profileID, lat, lon, odometer)
	if err != nil {
		return err
	}

	r.currentTrip = trip
	r.maxSpeed = 0
	r.pointBuffer = nil
	r.lastPoint = nil
	r.pub.PublishTripStatus("recording", trip.ID, profileID, 0, 0)
	return nil
}

func (r *Recorder) EndTrip(lat, lon float64, odometer int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.currentTrip == nil {
		return nil
	}

	if len(r.pointBuffer) > 0 {
		r.store.InsertPoints(r.pointBuffer)
		r.pointBuffer = nil
	}

	trip, err := r.store.CompleteTrip(r.currentTrip.ID, lat, lon, odometer, r.maxSpeed)
	if err != nil {
		return err
	}

	r.pub.PublishTripCompleted(trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS, trip.MaxSpeed)
	r.pub.ClearTrip()
	r.currentTrip = nil
	r.lastPoint = nil
	return nil
}

func (r *Recorder) AddPoint(lat, lon, altitude, speed, course float64, odometer, timestampMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.currentTrip == nil {
		return
	}

	current := RecordedPoint{
		Point:  geo.Point{Lat: lat, Lon: lon},
		Speed:  speed,
		Course: course,
	}

	if r.lastPoint != nil && !ShouldRecord(*r.lastPoint, current) {
		return
	}

	if speed > r.maxSpeed {
		r.maxSpeed = speed
	}

	point := db.TripPoint{
		TripID:    r.currentTrip.ID,
		Timestamp: timestampMs,
		Latitude:  lat,
		Longitude: lon,
		Speed:     &speed,
		Course:    &course,
	}
	if altitude != 0 {
		point.Altitude = &altitude
	}
	if odometer != 0 {
		point.Odometer = &odometer
	}

	r.pointBuffer = append(r.pointBuffer, point)
	r.lastPoint = &current

	if len(r.pointBuffer) >= 10 {
		r.store.InsertPoints(r.pointBuffer)
		r.pointBuffer = nil
	}
}

func (r *Recorder) FlushPoints() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pointBuffer) > 0 && r.currentTrip != nil {
		r.store.InsertPoints(r.pointBuffer)
		r.pointBuffer = nil
	}
}

func (r *Recorder) RecoverFromCrash(vehicleReady bool) error {
	trip, err := r.store.GetRecordingTrip()
	if err != nil {
		return err
	}
	if trip == nil {
		return nil
	}

	if vehicleReady {
		r.mu.Lock()
		r.currentTrip = trip
		r.maxSpeed = trip.MaxSpeed
		r.mu.Unlock()
		r.pub.PublishTripStatus("recording", trip.ID, trip.ProfileID, trip.DistanceM, trip.DurationS)
		slog.Info("resumed recording trip", "id", trip.ID)
		return nil
	}

	slog.Info("abandoning unfinished trip", "id", trip.ID)
	return r.store.AbandonTrip(trip.ID)
}

func (r *Recorder) CurrentTripDuration() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentTrip == nil {
		return 0
	}
	return time.Now().Unix() - r.currentTrip.StartedAt
}
