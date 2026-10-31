package energy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"
)

func load(path string, now time.Time) (Totals, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Totals{Version: 1, Since: now.UTC()}, nil
	}
	if err != nil {
		return Totals{}, err
	}
	var t Totals
	if err = json.Unmarshal(raw, &t); err != nil {
		return t, err
	}
	return t, t.validate()
}

func save(path string, t Totals) error {
	if err := t.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".energy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func snapshot(ctx context.Context, client *redis.Client) (map[string]string, [2]map[string]string, error) {
	pipe := client.TxPipeline()
	vehicle := pipe.HGetAll(ctx, "vehicle")
	b0 := pipe.HGetAll(ctx, "battery:0")
	b1 := pipe.HGetAll(ctx, "battery:1")
	_, err := pipe.Exec(ctx)
	return vehicle.Val(), [2]map[string]string{b0.Val(), b1.Val()}, err
}

func publish(ctx context.Context, client *redis.Client, fields map[string]any) error {
	pipe := client.TxPipeline()
	pipe.HSet(ctx, "energy", fields)
	pipe.Publish(ctx, "energy", "updated-at")
	_, err := pipe.Exec(ctx)
	return err
}

// Run never commands batteries or changes their sampling/activation policy.
// The durable file is authoritative; Redis is only the presentation snapshot.
func Run(ctx context.Context, client *redis.Client, path string, logger *slog.Logger) error {
	now := time.Now()
	totals, err := load(path, now)
	if err != nil {
		return fmt.Errorf("load energy accounting: %w", err)
	}
	if totals.Intervals > 0 {
		totals.Gaps++ // Baselines cannot account for process downtime.
	}
	if err = save(path, totals); err != nil {
		return fmt.Errorf("save energy accounting: %w", err)
	}
	a := New(totals, now)
	saved := totals
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		vehicle, packs, readErr := snapshot(readCtx, client)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		status := "collecting"
		if readErr != nil {
			a.Break()
			status = "unavailable"
			logger.Warn("energy snapshot unavailable", "error", readErr)
		} else {
			status = a.Observe(time.Now(), vehicle, packs)
		}
		if a.Totals != saved {
			if err = save(path, a.Totals); err != nil {
				// Retain the unsaved baseline in memory to retry without counting twice,
				// but never publish totals that a restart would immediately roll back.
				logger.Error("save energy accounting", "error", err)
				status = "storage-error"
			} else {
				saved = a.Totals
			}
		}
		presentation := *a
		presentation.Totals = saved
		pubCtx, pubCancel := context.WithTimeout(ctx, 2*time.Second)
		err = publish(pubCtx, client, presentation.Fields(time.Now(), status))
		pubCancel()
		if err != nil && ctx.Err() == nil {
			logger.Warn("publish energy accounting", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
