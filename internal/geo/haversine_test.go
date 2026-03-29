package geo

import (
	"math"
	"testing"
)

func TestHaversine_BerlinKnownDistance(t *testing.T) {
	alexanderplatz := Point{Lat: 52.5219, Lon: 13.4132}
	brandenburgerTor := Point{Lat: 52.5163, Lon: 13.3777}

	dist := Haversine(alexanderplatz, brandenburgerTor)

	if dist < 2000 || dist > 2500 {
		t.Errorf("Berlin Alexanderplatz to Brandenburger Tor = %.1fm, want ~2200m", dist)
	}
}

func TestHaversine_SamePoint(t *testing.T) {
	p := Point{Lat: 52.520, Lon: 13.405}
	dist := Haversine(p, p)
	if dist != 0 {
		t.Errorf("same point distance = %f, want 0", dist)
	}
}

func TestHaversine_Antipodal(t *testing.T) {
	a := Point{Lat: 0, Lon: 0}
	b := Point{Lat: 0, Lon: 180}

	dist := Haversine(a, b)
	halfCircumference := math.Pi * earthRadiusM

	tolerance := halfCircumference * 0.001
	if math.Abs(dist-halfCircumference) > tolerance {
		t.Errorf("antipodal distance = %.1fm, want ~%.1fm", dist, halfCircumference)
	}
}

func TestHeadingDelta_Basic(t *testing.T) {
	tests := []struct {
		a, b float64
		want float64
	}{
		{0, 90, 90},
		{90, 0, 90},
		{350, 10, 20},
		{10, 350, 20},
		{0, 180, 180},
		{180, 0, 180},
		{0, 0, 0},
		{359, 1, 2},
		{1, 359, 2},
	}

	for _, tt := range tests {
		got := HeadingDelta(tt.a, tt.b)
		if math.Abs(got-tt.want) > 0.001 {
			t.Errorf("HeadingDelta(%f, %f) = %f, want %f", tt.a, tt.b, got, tt.want)
		}
	}
}
