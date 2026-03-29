# Trip Service

Records trips (unlock-to-lock) with adaptive GPS traces, attributes them to rider profiles.

## Build

```bash
GOTOOLCHAIN=go1.25.7 make build      # ARM binary
GOTOOLCHAIN=go1.25.7 make build-host # Host binary
GOTOOLCHAIN=go1.25.7 make test       # Run tests
```

## Architecture

- SQLite WAL database at `/data/trips.db`
- Watches: vehicle (state), engine-ecu (speed, odometer), gps (position), profile (active rider)
- Publishes: trip hash (current trip status), trip:completed (pub/sub on completion)
- Adaptive GPS recording: speed-dependent intervals, heading/speed triggers, 5m minimum distance

## Deploy to deep-blue

```bash
GOTOOLCHAIN=go1.25.7 make build
scp bin/trip-service deep-blue:/data/trip-service-test
ssh deep-blue "systemctl stop librescoot-trip && cp /data/trip-service-test /usr/bin/trip-service && systemctl start librescoot-trip"
```
