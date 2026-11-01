# Librescoot Trip Service

Records rides from unlock to lock, with adaptive GPS traces and per-profile attribution. Unlock/lock sequences that do not move the scooter are discarded.

**This is a prototype.**

Part of the [Librescoot](https://librescoot.org/) open-source platform.

```bash
GOTOOLCHAIN=go1.25.7 make build-host
./bin/trip-service --redis localhost:6379 --db /tmp/trips.db
```

## Capabilities

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

## Plausibility filter

An unlock/lock sequence only becomes history when the ECU odometer advances by at least 100 m from the ride's baseline. Shorter sequences — unlocking to fetch something from the seatbox, pushing the scooter a few metres — are deleted instead of completed: no history row, no GPS points and no `trip:completed` event, so they never reach trip history or profile statistics. A backward odometer delta is treated the same way. The floor is `recorder.MinTripDistanceM`.

The counter is not affected: it tracks the ECU odometer independently of trip history. It counts positive odometer deltas while ready to drive. A lower reading establishes a new baseline without subtracting accumulated distance, so subsequent increases count even after a controller replacement or odometer reset.

## Operation and interfaces

### Data sources

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

## Reading trip history

The read-only JSON RPC queue `trip:history:rpc` provides `history.list`,
`history.get` and `history.points`. The `trip:history:ready` lease is `1`
while the interface is running. Only completed trips are returned; the active
trip and the independent trip counter retain their existing interfaces.

`history.list` accepts `limit` (1-50, default 20), an optional `profile` and
an opaque `cursor`. It returns newest completions first, with `next_cursor`
when another page exists. The cursor fixes the first page's upper bound: newly
completed rides appear on refresh, while retention can still remove old rides.
`history.get` accepts `id`; `history.points` accepts `id`, `after_id` and
`limit` (1-256, default 100) and returns points in acquisition order with
`next_after_id` when more are available. A removed trip returns `trip not found`.

Trip times are Unix seconds, GPS timestamps Unix milliseconds, distance metres,
ready duration seconds and speed km/h. The maximum is the recorded GPS speed,
not an ECU peak. The point count is queried from the actual stored points. GPS
tracks can contain gaps; missing routes do not prevent reading ride summaries.

## Battery energy accounting

The `energy` Redis hash reports a signed **net energy estimate** from the two
active traction packs. Each interval uses the change in BMS remaining capacity
(mAh) multiplied by the mean of its endpoint voltages. It does not extrapolate
an instantaneous current over the battery polling interval. Positive values
mean net withdrawal; negative values mean net capacity gain. Capacity changes
can also include BMS gauge corrections, so these are estimates, not separate
gross discharge or recuperation measurements.

`net:mwh` is the sum of `driving:mwh`, `parked:mwh`, `standby:mwh` and
`unassigned:mwh`. Driving means `ready-to-drive`, including stops within that
state; parked includes hop-on and waiting-seatbox. Each pack interval is assigned
in full to the phase with the largest accumulated time share, including repeated
visits to that phase. Valid vehicle transition timestamps locate changes between
observations; otherwise the observation time is used. Attribution uses acquisition
endpoints, not the later Redis read time. Exact ties or a dominant unknown period
remain unassigned. This is a split by operating phase, not by electrical load:
charging and auxiliary loads can
occur in any phase. It does not separately measure AUX/CBB charging or losses.

Accounting consumes `sample:timestamp` and `sample:valid` from battery-service,
plus presence, active state, serial, voltage and remaining/full capacity. These
metadata describe NFC acquisition, not the BMS's internal measurement time.
Repeated cached reads do not create energy. Read errors, invalid/stale samples,
pack replacement, full-capacity recalibration, implausible jumps and long gaps
break the baseline. Unobserved energy is not invented, including across service
restarts, sleep gaps and removal/off-vehicle charging. Both slots carrying the
same serial are rejected to prevent double counting.

`source=bms-capacity-delta`, `since` (UTC), `known`, `status`, `updated-at` (Unix
milliseconds), `intervals` and `gaps` describe the estimate's coverage. Status is
`waiting`, `collecting`, `partial`, `unavailable` or `storage-error`. No interval
means unknown, not a measured zero. Acquisitions older than 90 seconds and
intervals longer than 120 seconds are rejected. Polling Redis once a second
does not change battery activation or NFC polling.

Totals are persisted with file and directory fsync plus atomic rename in
`energy.json` beside the configured trip database. The file is authoritative;
Redis is a presentation snapshot. A corrupt file is reported as unavailable,
never silently replaced with zero totals. Baselines are deliberately not
restored across process restarts. `energy` publishes `updated-at` after each
atomic hash update. Existing ECU energy counters and instantaneous power are
unchanged and have a separate measurement period.

## Storage

SQLite WAL at `/data/trips.db`. Two tables:

- `trips` - one row per ride (profile, start/end location, odometer, distance, duration, speeds)
- `trip_points` - GPS trace points (timestamp, lat/lon, altitude, speed, course, odometer)

Distance comes from odometer delta rather than summing GPS hops — the wheel measurement is more accurate than chasing noisy GPS positions.

## Crash recovery

On startup, checks for trips stuck in `recording` state. If the vehicle is still in `ready-to-drive`, resumes recording. Otherwise marks the trip as `abandoned` with whatever data was captured.

## Build

```bash
GOTOOLCHAIN=go1.25.7 make build        # ARM cross-compile
GOTOOLCHAIN=go1.25.7 make build-host   # native
GOTOOLCHAIN=go1.25.7 make test
```

## Packaging

The Yocto layer in [meta-librescoot](https://github.com/librescoot/meta-librescoot) installs `/usr/bin/trip-service` and the systemd unit `librescoot-trip.service`.

## License

This project is dual-licensed. The source code is available under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

Made with ❤️ by the Librescoot community
