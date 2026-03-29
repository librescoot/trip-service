package recorder

import (
	"testing"
	"time"

	"github.com/librescoot/trip-service/internal/geo"
)

func TestIntervalForSpeed(t *testing.T) {
	tests := []struct {
		speed float64
		want  time.Duration
	}{
		{0, 0},
		{3, 0},
		{10, 5 * time.Second},
		{30, 8 * time.Second},
		{50, 15 * time.Second},
	}
	for _, tt := range tests {
		got := IntervalForSpeed(tt.speed)
		if got != tt.want {
			t.Errorf("IntervalForSpeed(%f) = %v, want %v", tt.speed, got, tt.want)
		}
	}
}

func TestShouldRecord_MinDistance(t *testing.T) {
	last := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	current := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	if ShouldRecord(last, current) {
		t.Error("should not record point within minimum distance")
	}
}

func TestShouldRecord_FarEnough(t *testing.T) {
	last := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	current := RecordedPoint{Point: geo.Point{Lat: 52.521, Lon: 13.401}, Speed: 22, Course: 92}
	if !ShouldRecord(last, current) {
		t.Error("should record point beyond minimum distance")
	}
}

func TestShouldRecordImmediate_HeadingChange(t *testing.T) {
	last := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	current := RecordedPoint{Point: geo.Point{Lat: 52.521, Lon: 13.401}, Speed: 20, Course: 120}
	if !ShouldRecordImmediate(last, current) {
		t.Error("should trigger immediate record on heading change > 15")
	}
}

func TestShouldRecordImmediate_SpeedChange(t *testing.T) {
	last := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	current := RecordedPoint{Point: geo.Point{Lat: 52.521, Lon: 13.401}, Speed: 35, Course: 91}
	if !ShouldRecordImmediate(last, current) {
		t.Error("should trigger immediate record on speed change > 10")
	}
}

func TestShouldRecordImmediate_NoTrigger(t *testing.T) {
	last := RecordedPoint{Point: geo.Point{Lat: 52.520, Lon: 13.400}, Speed: 20, Course: 90}
	current := RecordedPoint{Point: geo.Point{Lat: 52.521, Lon: 13.401}, Speed: 22, Course: 92}
	if ShouldRecordImmediate(last, current) {
		t.Error("should not trigger immediate when no heading/speed change")
	}
}
