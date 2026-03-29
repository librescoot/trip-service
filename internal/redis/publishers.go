package redis

import (
	"fmt"

	ipc "github.com/librescoot/redis-ipc"
)

type Publishers struct {
	trip   *ipc.HashPublisher
	client *ipc.Client
}

func NewPublishers(client *ipc.Client) *Publishers {
	return &Publishers{
		trip:   client.NewHashPublisher("trip"),
		client: client,
	}
}

func (p *Publishers) PublishTripStatus(status, id, profileID string, distanceM, durationS int64) {
	p.trip.SetMany(map[string]any{
		"status":     status,
		"id":         id,
		"profile-id": profileID,
		"distance-m": fmt.Sprintf("%d", distanceM),
		"duration-s": fmt.Sprintf("%d", durationS),
	})
}

func (p *Publishers) PublishTripCompleted(id string, profileID string, distanceM, durationS int64, maxSpeed float64) {
	msg := fmt.Sprintf("%s:%s:%d:%d:%.1f", id, profileID, distanceM, durationS, maxSpeed)
	p.client.Publish("trip:completed", msg)
}

func (p *Publishers) ClearTrip() {
	p.trip.ReplaceAll(map[string]any{"status": "idle"})
}
