package energy

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestServiceWithIsolatedRedis(t *testing.T) {
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server required for integration test")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "r.sock")
	cmd := exec.Command(server, "--port", "0", "--save", "", "--appendonly", "no", "--unixsocket", socket, "--unixsocketperm", "700", "--dir", dir)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: -1})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wait := func(label string, ok func() bool) {
		t.Helper()
		until := time.Now().Add(4 * time.Second)
		for !ok() {
			if time.Now().After(until) {
				t.Fatalf("timeout: %s", label)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait("redis", func() bool { return client.Ping(ctx).Err() == nil })
	cfg, err := client.ConfigGet(ctx, "dir").Result()
	if err != nil || cfg["dir"] != dir || client.Options().Network != "unix" || client.Options().Addr != socket {
		t.Fatal("Redis isolation failed", cfg, err)
	}
	if err = client.HSet(ctx, "vehicle", vehicle("stand-by")).Err(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "energy.json")
	done := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { done <- Run(runCtx, client, path, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	wait("startup", func() bool { return client.HGet(ctx, "energy", "status").Val() == "waiting" })
	first := time.Now()
	if err = client.HSet(ctx, "battery:0", pack(first, "A", 20000)).Err(); err != nil {
		t.Fatal(err)
	}
	wait("baseline", func() bool { return client.HGet(ctx, "energy", "status").Val() == "collecting" })
	if client.HGet(ctx, "energy", "known").Val() != "false" {
		t.Fatal("baseline already treated as an interval")
	}
	if err = client.HSet(ctx, "battery:0", pack(time.Now(), "A", 19999)).Err(); err != nil {
		t.Fatal(err)
	}
	wait("energy", func() bool { return client.HGet(ctx, "energy", "net:mwh").Val() == "53.000" })
	if client.HGet(ctx, "energy", "standby:mwh").Val() != "53.000" {
		t.Fatal("incorrect phase")
	}
	saved, err := load(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	closeTo(t, saved.Net(), 53)
	// Wrong-type input must break coverage, not retain an apparently live sample.
	if err = client.Del(ctx, "battery:0").Err(); err != nil {
		t.Fatal(err)
	}
	if err = client.Set(ctx, "battery:0", "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	wait("read error", func() bool { return client.HGet(ctx, "energy", "status").Val() == "unavailable" })
	closeTo(t, saved.Net(), 53)
	stop()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
}
