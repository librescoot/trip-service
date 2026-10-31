package energy

import "time"

type phaseChange struct {
	at    time.Time
	phase string
}

type phaseTimeline struct {
	changes          []phaseChange
	observed         time.Time
	state, timestamp string
}

func (p *phaseTimeline) append(at time.Time, phase string) {
	if n := len(p.changes); n > 0 {
		last := &p.changes[n-1]
		if last.at.Equal(at) {
			last.phase = phase
			return
		}
		if last.phase == phase {
			return
		}
	}
	p.changes = append(p.changes, phaseChange{at: at, phase: phase})
}

func (p *phaseTimeline) observe(now, started time.Time, vehicle map[string]string) {
	state, timestamp := vehicle["state"], vehicle["state:timestamp"]
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	validTime := err == nil && !at.After(now)
	if p.observed.IsZero() {
		if started.After(now) {
			started = now
		}
		p.append(started, "unassigned")
		if !validTime {
			at = now
		} else if at.Before(started) {
			at = started
		}
		p.append(at, phase(state))
	} else if state != p.state || timestamp != p.timestamp {
		if !validTime || !at.After(p.observed) {
			at = now
		}
		// A changed timestamp on the same state reveals a missed excursion.
		if state == p.state {
			p.append(p.observed, "unassigned")
		}
		p.append(at, phase(state))
	}
	p.observed, p.state, p.timestamp = now, state, timestamp
	// A delayed endpoint may need an entire valid interval before its sample age.
	cutoff := now.Add(-maxSampleAge - maxInterval)
	first := 0
	for first+1 < len(p.changes) && !p.changes[first+1].at.After(cutoff) {
		first++
	}
	if first > 0 {
		copy(p.changes, p.changes[first:])
		p.changes = p.changes[:len(p.changes)-first]
	}
}

func (p *phaseTimeline) largestShare(start, end time.Time) string {
	if !end.After(start) {
		return "unassigned"
	}
	durations := make(map[string]time.Duration, 4)
	cursor, current := start, "unassigned"
	for _, change := range p.changes {
		if !change.at.After(start) {
			current = change.phase
			continue
		}
		if !change.at.Before(end) {
			break
		}
		durations[current] += change.at.Sub(cursor)
		cursor, current = change.at, change.phase
	}
	durations[current] += end.Sub(cursor)
	bucket, longest, tied := "unassigned", time.Duration(0), false
	for phase, duration := range durations {
		if duration > longest {
			bucket, longest, tied = phase, duration, false
		} else if duration == longest {
			tied = true
		}
	}
	if tied {
		return "unassigned"
	}
	return bucket
}
