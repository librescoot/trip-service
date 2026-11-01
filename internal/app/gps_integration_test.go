package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
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

func TestGPSSnapshotsRecordHistoryWithIsolatedRedis(t *testing.T) {
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	cmd := exec.Command(server, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	raw := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wait := func(what string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	wait("isolated Redis", func() bool {
		info, err := raw.Info(ctx, "server").Result()
		return err == nil && strings.Contains(info, "process_id:"+strconv.Itoa(cmd.Process.Pid)+"\r\n")
	})
	config, err := raw.ConfigGet(ctx, "dir").Result()
	if err != nil || config["dir"] != dir {
		t.Fatalf("redis isolation mismatch: %v %v", config, err)
	}
	if err := raw.HSet(ctx, "vehicle", "state", "parked").Err(); err != nil {
		t.Fatal(err)
	}
	if err := raw.HSet(ctx, "engine-ecu", "odometer", "1000").Err(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "trips.db")
	appCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	service := New(&Config{RedisAddr: addr, DBPath: dbPath, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go func() { done <- service.Run(appCtx) }()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("app shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("app did not stop")
		}
	}()
	wait("trip service readiness", func() bool {
		return raw.Get(ctx, "trip:ready").Val() == "1" && raw.Get(ctx, "trip:history:ready").Val() == "1"
	})
	setState := func(state string) {
		t.Helper()
		if err := raw.HSet(ctx, "vehicle", "state", state).Err(); err != nil {
			t.Fatal(err)
		}
		if err := raw.Publish(ctx, "vehicle", "state").Err(); err != nil {
			t.Fatal(err)
		}
	}
	setState("ready-to-drive")
	wait("recording trip", func() bool { return raw.HGet(ctx, "trip", "status").Val() == "recording" })

	payload := func(lat, lon string) map[string]any {
		return map[string]any{"active": true, "connected": true, "fix": "3d", "state": "fix-established",
			"latitude": lat, "longitude": lon, "altitude": "47.000000", "speed": "36.000000", "course": "90.000000",
			"timestamp": "2026-01-01T12:00:00Z", "updated": "2026-01-01T12:00:00Z"}
	}
	publishSnapshot := func(sample map[string]any) {
		t.Helper()
		// Production writes the hash without notifying gps, then publishes one complete snapshot.
		if err := raw.HSet(ctx, "gps", sample).Err(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		if err := raw.Publish(ctx, "gps:tpv", data).Err(); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []map[string]any{
		{"active": false}, {"connected": false}, {"fix": "none"}, {"state": "searching"},
		{"latitude": "NaN"}, {"longitude": "+Inf"}, {"latitude": "91"}, {"longitude": "181"},
		{"latitude": "0", "longitude": "0"}, {"speed": "-1"}, {"course": "NaN"},
	} {
		sample := payload("48.000000", "11.000000")
		for k, v := range change {
			sample[k] = v
		}
		publishSnapshot(sample)
	}
	missing := payload("48.000000", "11.000000")
	delete(missing, "longitude")
	publishSnapshot(missing)
	if err := raw.Publish(ctx, "gps:tpv", "{malformed").Err(); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Second).UnixMilli()
	for i := 0; i < 10; i++ {
		sample := payload(fmt.Sprintf("%.6f", 52.5+float64(i)*0.0001), "13.400000")
		if i == 4 {
			sample["fix"] = "2d"
		}
		if i >= 5 {
			// A GPS clock correction must not suppress later live positions.
			sample["timestamp"] = "2025-12-31T23:59:00Z"
		}
		if i == 9 {
			delete(sample, "timestamp")
		}
		publishSnapshot(sample)
		if i == 0 {
			publishSnapshot(sample)
		}
	}
	reader, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	wait("ten persisted GPS points", func() bool {
		var count int
		return reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM trip_points").Scan(&count) == nil && count >= 10
	})
	// A legacy recovery notification is not a second GPS observation.
	if err := raw.HSet(ctx, "gps", payload("49.000000", "12.000000")).Err(); err != nil {
		t.Fatal(err)
	}
	legacySubscribers, err := raw.Publish(ctx, "gps", "timestamp").Result()
	if err != nil || legacySubscribers != 0 {
		t.Fatalf("legacy GPS notification subscribers: %d, %v", legacySubscribers, err)
	}
	if err := raw.HSet(ctx, "engine-ecu", "odometer", "1150").Err(); err != nil {
		t.Fatal(err)
	}
	if err := raw.Publish(ctx, "engine-ecu", "odometer").Err(); err != nil {
		t.Fatal(err)
	}
	setState("parked")
	wait("paused trip", func() bool { return raw.HGet(ctx, "trip", "status").Val() == "paused" })
	publishSnapshot(payload("50.000000", "12.000000"))
	setState("stand-by")
	client, err := ipc.New(ipc.WithURL(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var page db.HistoryPage
	wait("completed history", func() bool {
		page, err = ipc.CallMethod[historyListRequest, db.HistoryPage](client, historyChannel, "history.list", historyListRequest{Limit: 10}, time.Second)
		return err == nil && len(page.Trips) == 1
	})
	trip := page.Trips[0]
	if trip.DistanceM != 150 || trip.PointCount != 10 {
		t.Fatalf("history trip: %+v", trip)
	}
	points, err := ipc.CallMethod[historyPointsRequest, db.HistoryPointsPage](client, historyChannel, "history.points", historyPointsRequest{ID: trip.ID, Limit: 20}, time.Second)
	if err != nil || len(points.Points) != 10 {
		t.Fatalf("history points: %+v, %v", points, err)
	}
	for i, p := range points.Points {
		wantLat := 52.5 + float64(i)*0.0001
		if math.Abs(p.Latitude-wantLat) > 1e-9 || p.Longitude != 13.4 || p.SpeedKmh == nil || *p.SpeedKmh != 36 || p.TimestampMs < start || p.TimestampMs > time.Now().Add(time.Second).UnixMilli() {
			t.Fatalf("point %d: %+v, want latitude %f and 36 km/h", i, p, wantLat)
		}
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app did not stop")
	}
	// The deferred cleanup must not receive a second completion.
	done <- nil
	count, err := raw.PubSubNumSub(ctx, "gps:tpv").Result()
	if err != nil || count["gps:tpv"] != 0 {
		t.Fatalf("GPS subscription after stop: %v, %v", count, err)
	}
	if subscribers, err := raw.Publish(ctx, "gps:tpv", "{}").Result(); err != nil || subscribers != 0 {
		t.Fatalf("GPS publish after stop: subscribers=%d, err=%v", subscribers, err)
	}
}
