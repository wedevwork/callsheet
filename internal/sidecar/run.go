package sidecar

import (
	"context"
	"log/slog"
	"time"

	"github.com/wedevwork/callsheet/internal/client"
	"github.com/wedevwork/callsheet/internal/contract"
)

// backoffSchedule is the delay after consecutive failures: 1, 2, 4, 8,
// 16 s, then 30 s capped. Each delay is multiplied by an independent
// uniform jitter in [0.8, 1.0).
var backoffSchedule = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

// stableAcks resets the backoff: only three consecutive acknowledgements on
// one connection prove it healthy, so short flaps keep backing off.
const stableAcks = 3

// backoff returns the jittered delay for the attempt-th consecutive
// failure (0-based) and sample u in [0, 1).
func backoff(attempt int, u float64) time.Duration {
	base := backoffSchedule[min(attempt, len(backoffSchedule)-1)]
	return time.Duration(float64(base) * (0.8 + 0.2*u))
}

// terminal reports whether err is an actionable configuration error that
// ends Run rather than the plane going away: a protocol version mismatch
// (exit 7), an invalid protocol exchange (2), an unknown enrolled node (3)
// or failed trust (6). A duplicate active stream (conflict), unavailable
// and internal errors are retried.
func terminal(err error) bool {
	switch contract.CodeOf(err) {
	case contract.CodeProtocolMismatch, contract.CodeInvalidArgument, contract.CodeNotFound, contract.CodeTrustFailed:
		return true
	}
	return false
}

// run is Run: it validates the enrollment under the state lock, which it
// holds for its whole lifetime, and keeps one outbound connection.
func (d *deps) run(ctx context.Context, o RunOptions) error {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !contract.ValidSoftwareVersion(o.SoftwareVersion) {
		return errf(contract.CodeInvalidArgument, "invalid software version %q", contract.SafeText(o.SoftwareVersion, 64))
	}
	l := layout{root: o.StateDir}
	pre, err := l.scan()
	if err != nil {
		return err
	}
	if !pre.rootExists {
		return errf(contract.CodeNotFound, "no sidecar enrollment in %s; run callsheet sidecar enroll", l.root)
	}
	lk, err := l.acquire()
	if err != nil {
		return err
	}
	defer lk.release()
	s, err := l.scan()
	if err != nil {
		return err
	}
	if !s.hasIdentity {
		return errf(contract.CodeNotFound, "no sidecar enrollment in %s; run callsheet sidecar enroll", l.root)
	}
	id, err := l.loadIdentity()
	if err != nil {
		return err
	}
	if !s.hasEnrollment {
		return errf(contract.CodeNotFound, "node %s in %s has an identity but no completed enrollment; run callsheet sidecar enroll to complete it", id, l.root)
	}
	e, err := l.loadEnrollment()
	if err != nil {
		return err
	}
	c, err := d.newClient(e.PlaneURL, client.Trust{CAPEM: []byte(e.CAPEM), Fingerprint: e.CAFingerprint})
	if err != nil {
		return err
	}
	defer c.Close()
	logger = logger.With("node_id", id)
	logger.Info("sidecar starting", "plane_url", e.PlaneURL, "state_dir", l.root)
	d.emit(event{kind: evStarted})
	return d.loop(ctx, c, id, o.SoftwareVersion, logger)
}

// loop dials once immediately, then after every ended session retries with
// the capped, jittered backoff until ctx ends or a terminal error occurs.
// It never exits just because the plane went away.
func (d *deps) loop(ctx context.Context, c planeClient, id, sw string, logger *slog.Logger) error {
	attempt := 0
	for n := 1; ; n++ {
		err := d.session(ctx, c, n, id, sw, logger, func() { attempt = 0 })
		// Cancellation wins whenever it happened.
		if cerr := ctx.Err(); cerr != nil {
			logger.Info("sidecar stopped")
			return cerr
		}
		d.emit(event{kind: evEnded, session: n, err: err})
		if terminal(err) {
			attrs := []any{"error", err}
			var ce *contract.Error
			if contract.IsCode(err, contract.CodeProtocolMismatch) && asContract(err, &ce) {
				l, _ := ce.DetailInt("local_version")
				r, _ := ce.DetailInt("remote_version")
				attrs = append(attrs, "local_version", l, "remote_version", r, "advice", "run the same callsheet version on the plane and this node")
			}
			logger.Error("connection failed permanently", attrs...)
			return err
		}
		delay := backoff(attempt, d.jitter())
		attempt++
		logger.Warn("disconnected; retrying", "error", err, "retry_in_ms", delay.Milliseconds())
		d.emit(event{kind: evBackoff, session: n, delay: delay})
		timer, stop := d.clock.NewTimer(delay)
		select {
		case <-timer:
		case <-ctx.Done():
		}
		stop()
		if cerr := ctx.Err(); cerr != nil {
			logger.Info("sidecar stopped")
			return cerr
		}
	}
}
