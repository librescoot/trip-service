package app

import (
	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/trip-service/internal/db"
)

const historyChannel = "trip:history:rpc"

type historyListRequest struct {
	Profile string `json:"profile,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}
type historyTripRequest struct {
	ID string `json:"id"`
}
type historyPointsRequest struct {
	ID      string `json:"id"`
	AfterID int64  `json:"after_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func newHistoryServer(client *ipc.Client, store *db.Store) *ipc.CallServer {
	server := ipc.NewCallServer(client, historyChannel, ipc.WithCallServerConcurrency(2))
	ipc.RegisterCall(server, "history.list", func(r historyListRequest) (db.HistoryPage, error) {
		return store.ListHistory(r.Profile, r.Cursor, r.Limit)
	})
	ipc.RegisterCall(server, "history.get", func(r historyTripRequest) (db.HistoryTrip, error) {
		return store.HistoryTrip(r.ID)
	})
	ipc.RegisterCall(server, "history.points", func(r historyPointsRequest) (db.HistoryPointsPage, error) {
		return store.HistoryPoints(r.ID, r.AfterID, r.Limit)
	})
	return server
}
