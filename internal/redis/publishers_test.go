package redis

import (
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/counter"
)

func TestReadyTTLExceedsRefreshInterval(t *testing.T) {
	if ReadyTTL <= 30*time.Second {
		t.Fatalf("ReadyTTL = %s, must exceed 30s refresh", ReadyTTL)
	}
}
func TestCounterFieldsUseExternalUnits(t *testing.T) {
	fields := counterFields(counter.Snapshot{DistanceM: 12, DurationS: 3, AverageSpeedKmh: 14})
	if fields["distance-m"] != "12" || fields["duration-s"] != "3" || fields["average-speed-kmh"] != "14" {
		t.Fatalf("fields = %#v", fields)
	}
}
func TestExpungeFieldsAreCompleteAndBoundErrors(t *testing.T) {
	fields := expungeFields(ExpungeSnapshot{Policy: "count", Value: "0", Status: "idle", LastRun: 1, DeletedTrips: 2, DBBytes: 3, WALBytes: 4, UpdatedAt: 5, LastError: string(make([]byte, 300))})
	for _, key := range []string{"api-version", "policy", "value", "status", "last-run", "last-error", "deleted-trips", "db-bytes", "wal-bytes", "updated-at"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing %s from %#v", key, fields)
		}
	}
	if fields["api-version"] != "1" || fields["deleted-trips"] != "2" || len(fields["last-error"].(string)) != 256 {
		t.Fatalf("fields = %#v", fields)
	}
}
