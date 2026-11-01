package app

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/librescoot/trip-service/internal/db"
	"github.com/redis/go-redis/v9"
)

func TestHistoryRPCWithIsolatedRedis(t *testing.T) {
	redisServer, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	cmd := exec.Command(redisServer, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	raw := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		info, e := raw.Info(ctx, "server").Result()
		if e == nil {
			if !strings.Contains(info, "process_id:"+strconv.Itoa(cmd.Process.Pid)+"\r\n") {
				t.Fatal("redis process mismatch")
			}
			break
		}
		if ctx.Err() != nil {
			t.Fatal("isolated redis failed to start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	config, err := raw.ConfigGet(ctx, "dir").Result()
	if err != nil || config["dir"] != dir {
		t.Fatalf("redis isolation mismatch: %v %v", config, err)
	}
	store, err := db.Open(filepath.Join(dir, "trips.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	trip, err := store.CreateTrip("", 0, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPoints([]db.TripPoint{{TripID: trip.ID, Timestamp: 1001, Latitude: 52.5, Longitude: 13.4}, {TripID: trip.ID, Timestamp: 1002, Latitude: 52.6, Longitude: 13.5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTripAt(trip.ID, 0, 0, 1200, 0, trip.StartedAt+100); err != nil {
		t.Fatal(err)
	}
	service, err := ipc.New(ipc.WithURL(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	server := newHistoryServer(service, store)
	server.Start()
	defer server.Stop()
	client, err := ipc.New(ipc.WithURL(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	page, err := ipc.CallMethod[historyListRequest, db.HistoryPage](client, historyChannel, "history.list", historyListRequest{Limit: 1}, 3*time.Second)
	if err != nil || len(page.Trips) != 1 || page.Trips[0].ID != trip.ID {
		t.Fatalf("history RPC: %+v %v", page, err)
	}
	detail, err := ipc.CallMethod[historyTripRequest, db.HistoryTrip](client, historyChannel, "history.get", historyTripRequest{ID: trip.ID}, 3*time.Second)
	if err != nil || detail.ID != trip.ID || detail.PointCount != 2 {
		t.Fatalf("history detail: %+v %v", detail, err)
	}
	points, err := ipc.CallMethod[historyPointsRequest, db.HistoryPointsPage](client, historyChannel, "history.points", historyPointsRequest{ID: trip.ID, Limit: 1}, 3*time.Second)
	if err != nil || len(points.Points) != 1 || points.NextAfterID == 0 {
		t.Fatalf("history points page: %+v %v", points, err)
	}
	next, err := ipc.CallMethod[historyPointsRequest, db.HistoryPointsPage](client, historyChannel, "history.points", historyPointsRequest{ID: trip.ID, Limit: 1, AfterID: points.NextAfterID}, 3*time.Second)
	if err != nil || len(next.Points) != 1 || next.NextAfterID != 0 || next.Points[0].Latitude != 52.6 {
		t.Fatalf("history points continuation: %+v %v", next, err)
	}
}
