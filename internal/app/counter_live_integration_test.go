package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestCounterLiveSnapshotsWithoutOdometerEvents(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	wait := func(what string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
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
	setField := func(hash, field, value string) {
		t.Helper()
		if err := raw.HSet(ctx, hash, field, value).Err(); err != nil {
			t.Fatal(err)
		}
		if err := raw.Publish(ctx, hash, field).Err(); err != nil {
			t.Fatal(err)
		}
	}
	setField("vehicle", "state", "ready-to-drive")
	setField("engine-ecu", "odometer", "1000")
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
	wait("trip service readiness", func() bool { return raw.Get(ctx, "trip:ready").Val() == "1" })
	setField("engine-ecu", "odometer", "1100")
	wait("initial distance", func() bool { return raw.HGet(ctx, "trip:counter", "distance-m").Val() == "100" })

	previousDuration, previousAverage := int64(0), int64(0)
	for i := 0; i < 3; i++ {
		var fields map[string]string
		wait("live duration without odometer events", func() bool {
			fields, err = raw.HGetAll(ctx, "trip:counter").Result()
			duration, parseErr := strconv.ParseInt(fields["duration-s"], 10, 64)
			return err == nil && parseErr == nil && duration > previousDuration
		})
		duration, _ := strconv.ParseInt(fields["duration-s"], 10, 64)
		average, err := strconv.ParseInt(fields["average-speed-kmh"], 10, 64)
		wantAverage := (100*36 + duration*5) / (duration * 10)
		if err != nil || average != wantAverage || fields["distance-m"] != "100" || fields["status"] != "recording" {
			t.Fatalf("inconsistent live snapshot: %v", fields)
		}
		if i > 0 && average >= previousAverage {
			t.Fatalf("average speed did not update: %d >= %d", average, previousAverage)
		}
		previousDuration, previousAverage = duration, average
	}

	reader, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var durableDuration int64
	if err := reader.QueryRowContext(ctx, "SELECT duration_s FROM trip_counter WHERE id=1").Scan(&durableDuration); err != nil {
		t.Fatal(err)
	}
	if durableDuration != 0 {
		t.Fatalf("live updates checkpointed duration: %d", durableDuration)
	}

	setField("vehicle", "state", "parked")
	wait("paused counter", func() bool { return raw.HGet(ctx, "trip:counter", "status").Val() == "idle" })
	pausedDuration := raw.HGet(ctx, "trip:counter", "duration-s").Val()
	pausedAverage := raw.HGet(ctx, "trip:counter", "average-speed-kmh").Val()
	time.Sleep(2200 * time.Millisecond)
	if raw.HGet(ctx, "trip:counter", "duration-s").Val() != pausedDuration || raw.HGet(ctx, "trip:counter", "average-speed-kmh").Val() != pausedAverage {
		t.Fatal("counter continued while parked")
	}
	results := raw.Subscribe(ctx, "trip:command-result")
	defer results.Close()
	if _, err := results.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	command := fmt.Sprintf(`{"id":"live-reset","op":"counter.reset","source":"test","expires-at":%d}`, time.Now().Add(5*time.Second).UnixMilli())
	if err := raw.LPush(ctx, "scooter:trip", command).Err(); err != nil {
		t.Fatal(err)
	}
	resultCtx, cancelResult := context.WithTimeout(ctx, 3*time.Second)
	defer cancelResult()
	result, err := results.ReceiveMessage(resultCtx)
	if err != nil {
		t.Fatalf("reset result: %v, queued: %d", err, raw.LLen(ctx, "scooter:trip").Val())
	}
	if !strings.Contains(result.Payload, `"status":"ok"`) {
		t.Fatalf("reset result: %s", result.Payload)
	}
	wait("reset counter", func() bool { return raw.HGet(ctx, "trip:counter", "distance-m").Val() == "0" })
	if raw.HGet(ctx, "trip:counter", "duration-s").Val() != "0" {
		t.Fatal("reset duration is not zero")
	}
	setField("vehicle", "state", "ready-to-drive")
	wait("live counter after reset", func() bool { return raw.HGet(ctx, "trip:counter", "duration-s").Val() != "0" })
	if raw.HGet(ctx, "trip:counter", "average-speed-kmh").Val() != "0" {
		t.Fatal("pre-reset average restored by live publication")
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
	done <- nil
	stoppedDuration := raw.HGet(ctx, "trip:counter", "duration-s").Val()
	time.Sleep(1200 * time.Millisecond)
	if raw.HGet(ctx, "trip:counter", "duration-s").Val() != stoppedDuration {
		t.Fatal("live publication continued after shutdown")
	}
}
