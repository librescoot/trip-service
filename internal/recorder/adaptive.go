package recorder

import (
	"math"
	"time"

	"github.com/librescoot/trip-service/internal/geo"
)

const (
	MinRecordDistanceM = 5.0
	HeadingTriggerDeg  = 15.0
	SpeedTriggerKmh    = 10.0
)

type RecordedPoint struct {
	Point  geo.Point
	Speed  float64
	Course float64
}

func IntervalForSpeed(speedKmh float64) time.Duration {
	switch {
	case speedKmh < 5:
		return 0
	case speedKmh < 20:
		return 5 * time.Second
	case speedKmh < 45:
		return 8 * time.Second
	default:
		return 15 * time.Second
	}
}

func ShouldRecord(last, current RecordedPoint) bool {
	dist := geo.Haversine(last.Point, current.Point)
	if dist < MinRecordDistanceM {
		return false
	}
	return true
}

func ShouldRecordImmediate(last, current RecordedPoint) bool {
	dist := geo.Haversine(last.Point, current.Point)
	if dist < MinRecordDistanceM {
		return false
	}
	if geo.HeadingDelta(last.Course, current.Course) > HeadingTriggerDeg {
		return true
	}
	if math.Abs(last.Speed-current.Speed) > SpeedTriggerKmh {
		return true
	}
	return false
}
