# trip-service

Records every ride from unlock to lock, with adaptive GPS traces and per-profile attribution.

```bash
GOTOOLCHAIN=go1.25.7 make build-host
./bin/trip-service --redis localhost:6379 --db /tmp/trips.db
```

## What it does

- Starts a trip when the vehicle enters `ready-to-drive`, ends it on lock
- Records GPS points at adaptive intervals (more points in turns, fewer on straight roads)
- Attributes each trip to the active rider profile
- Persists everything to SQLite, crash-safe
- Publishes completion events so the profile service can update per-user stats

## Adaptive GPS recording

Recording frequency adjusts to how you're riding:

| Speed | Interval |
|-------|----------|
| < 5 km/h | Paused (filters parking noise) |
| 5-20 km/h | ~5s |
| 20-45 km/h | ~8s |
| > 45 km/h | ~15s |

Heading changes > 15 degrees or speed changes > 10 km/h trigger an immediate capture regardless of interval. Points closer than 5m are filtered out (GPS jitter).

A typical 30-minute city ride produces 200-600 points, about 10-25 KB.

## Data sources

Reads directly from Redis, same hashes as other services. No dependency on radio-gaga.

| Redis Hash | Fields | Purpose |
|------------|--------|---------|
| `vehicle` | `state` | Trip start/end detection |
| `engine-ecu` | `speed`, `odometer` | Speed tracking, distance calculation |
| `gps` | `latitude`, `longitude`, `altitude`, `course`, `speed` | GPS trace |
| `profile` | `active.id` | Who's riding |

## What it publishes

| Key | Type | Content |
|-----|------|---------|
| `trip` | Hash | `status` (recording/idle), `id`, `profile-id`, `distance-m`, `duration-s` |
| `trip:completed` | Pub/Sub | Fired on trip end with trip ID, profile ID, distance, duration, max speed |

## Storage

SQLite WAL at `/data/trips.db`. Two tables:

- `trips` - one row per ride (profile, start/end location, odometer, distance, duration, speeds)
- `trip_points` - GPS trace points (timestamp, lat/lon, altitude, speed, course, odometer)

Distance comes from odometer delta, not GPS path sum (more accurate on the scooter's actual wheel measurement).

## Crash recovery

On startup, checks for trips stuck in `recording` state. If the vehicle is still in `ready-to-drive`, resumes recording. Otherwise marks the trip as `abandoned` with whatever data was captured.

## Build

```bash
GOTOOLCHAIN=go1.25.7 make build        # ARM cross-compile
GOTOOLCHAIN=go1.25.7 make build-host   # native
GOTOOLCHAIN=go1.25.7 make test
```

## Deploy

```bash
scp bin/trip-service deep-blue:/data/trip-service-test
ssh deep-blue "systemctl stop librescoot-trip && cp /data/trip-service-test /usr/bin/trip-service && systemctl start librescoot-trip"
```

## License

AGPL-3.0
