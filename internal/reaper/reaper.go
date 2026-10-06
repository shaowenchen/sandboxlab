// Package reaper deletes sandboxes whose time is up.
//
// It is a loop rather than a scheduled job on purpose. The environment this
// control plane is built for lives for hours on a throwaway runner, so a
// CronJob would be a chart object, a scheduler dependency and an RBAC grant for
// something that can be a goroutine — and a goroutine cannot be forgotten to be
// enabled.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/shaowenchen/sandboxlab/internal/k8s"
)

// Reaper deletes expired sandboxes on an interval.
type Reaper struct {
	client   *k8s.Client
	interval time.Duration
	log      *slog.Logger
}

// New builds a reaper. An interval of zero or less disables it, which is what a
// deployment that would rather expire sandboxes by hand sets.
func New(client *k8s.Client, interval time.Duration, log *slog.Logger) *Reaper {
	return &Reaper{client: client, interval: interval, log: log}
}

// Run sweeps until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	if r.interval <= 0 {
		r.log.Info("the reaper is disabled; sandboxes will not expire on their own")
		return
	}
	r.log.Info("the reaper is running", "interval", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep deletes every sandbox that has expired, once.
//
// A failure on one sandbox does not stop the sweep: the next tick is thirty
// seconds away and the ones already collected are collected. A failure to list
// at all ends the sweep, because there is nothing to act on.
func (r *Reaper) Sweep(ctx context.Context) {
	expired, err := r.client.Expired(ctx)
	if err != nil {
		r.log.Warn("could not list sandboxes to expire", "error", err)
		return
	}
	for _, sb := range expired {
		if err := r.client.Delete(ctx, sb.ID); err != nil {
			// k8s.ErrNotFound is the common case and not worth a warning: the
			// sandbox was deleted by someone else between the list and the
			// delete, which is the outcome this loop wanted anyway.
			if err == k8s.ErrNotFound {
				continue
			}
			r.log.Warn("could not delete an expired sandbox", "sandbox", sb.ID, "error", err)
			continue
		}
		r.log.Info("deleted an expired sandbox",
			"sandbox", sb.ID,
			"template", sb.Template,
			"expired", time.Since(*sb.ExpiresAt).Truncate(time.Second).String()+" ago",
		)
	}
}
