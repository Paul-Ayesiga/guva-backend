// Package anchor implements the periodic Merkle-anchor job.
//
// On each tick:
//
//  1. Read the latest anchor's range_to_id (or 0 if none).
//  2. Read the chain's max entry_id.
//  3. If new entries exist (max > last anchored), compute the Merkle
//     root over (last_anchored+1 .. max) and INSERT a new anchor row.
//  4. (Optional) POST the new root to an external witness URL.
//
// The anchor table is append-only (trigger + role); each anchor is the
// platform's commitment to a contiguous range of chain entries. Once a
// root is published externally and an external_proof is attached, that
// range is permanently provable to a third party even if GUVA itself
// vanishes.
package anchor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
	"github.com/guva-ug/guva-backend/services/audit/internal/store"
)

// Config bundles the inputs to the anchor job.
type Config struct {
	Store         *store.Store
	Logger        *slog.Logger
	Interval      time.Duration // default 5m
	MinNewEntries int           // skip the tick if fewer than this many new entries (default 1)

	// WitnessURL, if non-empty, is POSTed every freshly-computed
	// anchor as JSON. The endpoint is expected to return a 2xx; the
	// response body (if any) is currently ignored. Future work
	// records the response into external_proof.
	WitnessURL string

	// MeterTick is invoked once per tick with the outcome label
	// ("ok"|"empty"|"error"). The audit service wires this to a
	// Prometheus counter; tests can pass nil.
	MeterTick func(outcome string)
}

// Job is the periodic anchor builder.
type Job struct {
	cfg Config
}

func New(cfg Config) *Job {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.MinNewEntries <= 0 {
		cfg.MinNewEntries = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MeterTick == nil {
		cfg.MeterTick = func(string) {}
	}
	return &Job{cfg: cfg}
}

// Run blocks until ctx is cancelled. Drains immediately on start so
// a fresh service doesn't have to wait one full interval for the
// first anchor.
func (j *Job) Run(ctx context.Context) error {
	j.cfg.Logger.Info("anchor job starting",
		"interval", j.cfg.Interval, "min_new_entries", j.cfg.MinNewEntries,
		"witness_url", j.cfg.WitnessURL)
	j.tickOnce(ctx)
	t := time.NewTicker(j.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			j.cfg.Logger.Info("anchor job stopping")
			return nil
		case <-t.C:
			j.tickOnce(ctx)
		}
	}
}

// tickOnce performs one anchor computation if the chain has new rows.
// Errors are logged but never propagate — anchoring is best-effort
// and a single failure shouldn't abort the loop.
func (j *Job) tickOnce(ctx context.Context) {
	from, to, err := j.computeRange(ctx)
	if err != nil {
		j.cfg.MeterTick("error")
		j.cfg.Logger.Error("anchor range lookup failed", "error", err)
		return
	}
	if to-from+1 < int64(j.cfg.MinNewEntries) {
		j.cfg.MeterTick("empty")
		j.cfg.Logger.Debug("anchor tick skipped — not enough new entries",
			"from", from, "to", to)
		return
	}

	leaves, err := j.cfg.Store.EntryHashRange(ctx, from, to)
	if err != nil {
		j.cfg.MeterTick("error")
		j.cfg.Logger.Error("anchor leaves read failed", "error", err, "from", from, "to", to)
		return
	}
	root, err := audit.ComputeMerkleRoot(leaves)
	if err != nil {
		j.cfg.MeterTick("error")
		j.cfg.Logger.Error("anchor merkle compute failed", "error", err, "leaves", len(leaves))
		return
	}

	id, err := j.cfg.Store.InsertAnchor(ctx, from, to, int64(len(leaves)), root, audit.MerkleAlgorithm)
	if err != nil {
		j.cfg.MeterTick("error")
		j.cfg.Logger.Error("anchor insert failed", "error", err, "from", from, "to", to)
		return
	}
	j.cfg.MeterTick("ok")
	j.cfg.Logger.Info("anchor computed",
		"anchor_id", id, "from", from, "to", to,
		"leaves", len(leaves), "root", root)

	// External publish — best effort. If the witness rejects or
	// times out, the anchor is still recorded locally; an operator
	// can replay the publish later.
	if j.cfg.WitnessURL != "" {
		if err := publish(ctx, j.cfg.WitnessURL, id, from, to, root); err != nil {
			j.cfg.Logger.Warn("anchor witness publish failed; recorded locally only",
				"error", err, "witness_url", j.cfg.WitnessURL, "anchor_id", id)
		}
	}
}

func (j *Job) computeRange(ctx context.Context) (from, to int64, err error) {
	maxID, err := j.cfg.Store.MaxEntryID(ctx)
	if err != nil {
		return 0, 0, err
	}
	if maxID == 0 {
		return 1, 0, nil // empty chain → empty range
	}
	last, err := j.cfg.Store.LatestAnchor(ctx)
	switch {
	case err == nil:
		from = last.RangeToID + 1
	case errors.Is(err, store.ErrNoAnchors):
		// First anchor ever — start from the smallest entry on the chain.
		// We could pull MIN(entry_id) explicitly, but starting from 1
		// is fine since EntryHashRange tolerates an empty prefix.
		from = 1
	default:
		return 0, 0, err
	}
	if from > maxID {
		return from, from - 1, nil // nothing new
	}
	return from, maxID, nil
}

func publish(ctx context.Context, url string, anchorID, from, to int64, root string) error {
	payload, _ := json.Marshal(map[string]any{
		"anchor_id":     anchorID,
		"range_from_id": from,
		"range_to_id":   to,
		"merkle_root":   root,
		"algorithm":     audit.MerkleAlgorithm,
		"computed_at":   time.Now().UTC().Format(time.RFC3339Nano),
	})
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Source", "guva-audit-anchor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("witness HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
