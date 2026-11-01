package redis

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

func TestGPSSnapshotLogsFirstAcceptanceOnce(t *testing.T) {
	var logs bytes.Buffer
	w := &Watchers{gpsActive: true, log: slog.New(slog.NewTextHandler(&logs, nil)),
		gpsCallback: func(lat, lon, altitude, speed, course float64) error { return nil }}
	const valid = `{"active":true,"connected":true,"fix":"3d","state":"fix-established","latitude":"52.500000","longitude":"13.400000","altitude":"47.000000","speed":"36.000000","course":"90.000000"}`
	for i := 0; i < 2; i++ {
		if err := w.onGPSSnapshot(valid); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(logs.String(), "GPS snapshot intake active"); got != 1 {
		t.Fatalf("first-intake log count = %d, want 1", got)
	}
	if strings.Contains(logs.String(), "52.500000") || strings.Contains(logs.String(), "13.400000") {
		t.Fatal("GPS coordinates were logged")
	}
}

func TestGPSSnapshotStopWaitsForCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	callback := func(lat, lon, altitude, speed, course float64) error {
		calls.Add(1)
		close(entered)
		<-release
		return nil
	}
	w := &Watchers{
		vehicle: &ipc.HashWatcher{}, engine: &ipc.HashWatcher{}, profile: &ipc.HashWatcher{}, settings: &ipc.HashWatcher{},
		battery: [2]*ipc.HashWatcher{{}, {}}, gpsCallback: callback, gpsActive: true,
	}
	const valid = `{"active":true,"connected":true,"fix":"3d","state":"fix-established","latitude":"52.500000","longitude":"13.400000","altitude":"47.000000","speed":"36.000000","course":"90.000000"}`
	callbackDone := make(chan struct{})
	go func() { _ = w.onGPSSnapshot(valid); close(callbackDone) }()
	<-entered
	stopDone := make(chan struct{})
	go func() { w.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while GPS callback was active")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("GPS callback did not finish")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish")
	}
	if err := w.onGPSSnapshot(valid); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks after Stop: got %d, want 1", got)
	}
}
