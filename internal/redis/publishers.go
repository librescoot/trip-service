package redis

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/trip-service/internal/counter"
)

// ReadyTTL is intentionally longer than the periodic refresh interval.
const ReadyTTL = 90 * time.Second

type ExpungeSnapshot struct {
	Policy, Value, Status, LastError                    string
	LastRun, DeletedTrips, DBBytes, WALBytes, UpdatedAt int64
}

type Publishers struct {
	trip, counter, expunge *ipc.HashPublisher
	client                 *ipc.Client
	counterMu              sync.Mutex
}

func NewPublishers(client *ipc.Client) *Publishers {
	return &Publishers{trip: client.NewHashPublisher("trip"), counter: client.NewHashPublisher("trip:counter"), expunge: client.NewHashPublisher("trip:expunge"), client: client}
}
func (p *Publishers) PublishTripStatus(status, id, profileID string, distanceM, durationS int64) {
	p.trip.SetMany(map[string]any{"status": status, "id": id, "profile-id": profileID, "distance-m": fmt.Sprintf("%d", distanceM), "duration-s": fmt.Sprintf("%d", durationS)})
}
func (p *Publishers) PublishTripCompleted(id, profileID string, distanceM, durationS int64, maxSpeed float64) {
	p.client.Publish("trip:completed", fmt.Sprintf("%s:%s:%d:%d:%.1f", id, profileID, distanceM, durationS, maxSpeed))
}
func (p *Publishers) ClearTrip() { p.trip.ReplaceAll(map[string]any{"status": "idle"}) }
func counterFields(c counter.Snapshot) map[string]any {
	return map[string]any{"api-version": "1", "distance-m": strconv.FormatInt(c.DistanceM, 10), "duration-s": strconv.FormatInt(c.DurationS, 10), "average-speed-kmh": strconv.FormatInt(c.AverageSpeedKmh, 10), "reset-policy": c.ResetPolicy, "reset-at": strconv.FormatInt(c.ResetAt, 10), "reset-reason": c.ResetReason, "generation": strconv.FormatInt(c.Generation, 10), "status": c.Status, "updated-at": strconv.FormatInt(c.UpdatedAt, 10)}
}

// PublishCounter synchronously writes one complete, ordered counter snapshot.
func (p *Publishers) PublishCounter(c counter.Snapshot) error {
	p.counterMu.Lock()
	defer p.counterMu.Unlock()
	return p.counter.SetMany(counterFields(c), ipc.Sync())
}
func (p *Publishers) publishCommandResult(id, op, status, message string) error {
	if len(message) > 256 {
		message = message[:256]
	}
	_, err := p.client.Publish("trip:command-result", fmt.Sprintf(`{"id":%q,"op":%q,"status":%q,"error":%q}`, id, op, status, message), ipc.Sync())
	return err
}
func (p *Publishers) PublishCommandResult(id, op, status, message string) error {
	p.counterMu.Lock()
	defer p.counterMu.Unlock()
	return p.publishCommandResult(id, op, status, message)
}

// PublishCounterAndCommandResult prevents a result from overtaking its snapshot.
func (p *Publishers) PublishCounterAndCommandResult(c counter.Snapshot, id, op, status, message string) error {
	p.counterMu.Lock()
	defer p.counterMu.Unlock()
	if err := p.counter.SetMany(counterFields(c), ipc.Sync()); err != nil {
		return err
	}
	return p.publishCommandResult(id, op, status, message)
}
func expungeFields(s ExpungeSnapshot) map[string]any {
	if len(s.LastError) > 256 {
		s.LastError = s.LastError[:256]
	}
	return map[string]any{
		"api-version": "1", "policy": s.Policy, "value": s.Value, "status": s.Status,
		"last-run": strconv.FormatInt(s.LastRun, 10), "last-error": s.LastError,
		"deleted-trips": strconv.FormatInt(s.DeletedTrips, 10), "db-bytes": strconv.FormatInt(s.DBBytes, 10),
		"wal-bytes": strconv.FormatInt(s.WALBytes, 10), "updated-at": strconv.FormatInt(s.UpdatedAt, 10),
	}
}
func (p *Publishers) PublishExpunge(s ExpungeSnapshot) error {
	return p.expunge.SetMany(expungeFields(s), ipc.Sync())
}
func (p *Publishers) PublishReady() error { return p.client.Set("trip:ready", "1", ReadyTTL) }
func (p *Publishers) ClearReady() error   { _, err := p.client.Del("trip:ready"); return err }
