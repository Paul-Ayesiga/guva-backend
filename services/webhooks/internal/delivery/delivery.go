// Package delivery is the AMQP consumer that performs the outbound
// HTTP POST for each webhook delivery job. Signs the body with
// HMAC-SHA256 using the subscription's secret, retries on failure
// with exponential backoff, and routes to the DLQ after the configured
// max attempts.
//
// Retry mechanism: when an attempt fails non-terminally, we re-publish
// the same job with `attempt++` into the delivery exchange after a
// computed delay (using a per-message TTL + dead-letter routing —
// the RabbitMQ "delayed retry via DLX" idiom). Simpler than running
// a separate scheduler.
//
// DLQ: after MaxAttempts, the job is nack-rejected (no requeue) which
// the broker dead-letters to guva.webhooks.dlx → webhooks.delivery.dead
// for an operator to inspect / replay.
package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/guva-ug/guva-backend/services/webhooks/internal/matcher"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/store"

	amqp "github.com/rabbitmq/amqp091-go"
)

type Config struct {
	AMQPURL           string
	Exchange          string
	RoutingKey        string
	Queue             string
	MaxAttempts       int
	BackoffBase       time.Duration
	BackoffMultiplier float64
	DeliveryTimeout   time.Duration
}

type Worker struct {
	cfg    Config
	logger *slog.Logger
	store  *store.Store
	client *http.Client
}

func New(cfg Config, logger *slog.Logger, st *store.Store) *Worker {
	return &Worker{
		cfg:    cfg,
		logger: logger,
		store:  st,
		client: &http.Client{Timeout: cfg.DeliveryTimeout},
	}
}

func (w *Worker) Run(ctx context.Context) error {
	conn, err := amqp.Dial(w.cfg.AMQPURL)
	if err != nil {
		return fmt.Errorf("amqp dial: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("amqp channel: %w", err)
	}
	defer ch.Close()

	// Prefetch keeps a small backlog per worker so a slow target
	// doesn't head-of-line the rest.
	if err := ch.Qos(8, 0, false); err != nil {
		return fmt.Errorf("amqp qos: %w", err)
	}

	deliveries, err := ch.ConsumeWithContext(ctx, w.cfg.Queue, "webhooks-delivery", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("amqp consume: %w", err)
	}

	w.logger.Info("webhooks delivery worker starting",
		"queue", w.cfg.Queue, "max_attempts", w.cfg.MaxAttempts,
		"backoff_base", w.cfg.BackoffBase)

	// Concurrent dispatch — without this, the first stuck delivery
	// (sleeping on backoff) blocks everything behind it in the queue.
	// QoS prefetch above caps how many we hold in flight; the
	// semaphore mirrors it so we never start more goroutines than
	// the broker is willing to deliver.
	sem := make(chan struct{}, 8)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("webhooks delivery worker stopping")
			return nil
		case msg, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("amqp delivery channel closed")
			}
			sem <- struct{}{}
			go func(m amqp.Delivery) {
				defer func() { <-sem }()
				w.handle(ctx, ch, m)
			}(msg)
		}
	}
}

func (w *Worker) handle(ctx context.Context, ch *amqp.Channel, msg amqp.Delivery) {
	var job matcher.DeliveryJob
	if err := json.Unmarshal(msg.Body, &job); err != nil {
		w.logger.Error("delivery job unmarshal failed; dropping to DLQ",
			"error", err, "message_id", msg.MessageId)
		_ = msg.Nack(false, false) // false, false: don't requeue → dead-letter
		return
	}
	job.Attempt++

	body, sig, ts := signRequest(job)
	postCtx, cancel := context.WithTimeout(ctx, w.cfg.DeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, job.TargetURL, bytes.NewReader(body))
	if err != nil {
		w.terminalFailure(ctx, job, nil, err.Error(), msg)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Guva-Event-Type", job.EventType)
	req.Header.Set("X-Guva-Delivery-Id", job.DeliveryUUID)
	req.Header.Set("X-Guva-Subscription-Id", job.SubscriptionID)
	req.Header.Set("X-Guva-Attempt", strconv.Itoa(job.Attempt))
	req.Header.Set("X-Guva-Signature", fmt.Sprintf("t=%d,v1=%s", ts, sig))
	req.Header.Set("User-Agent", "guva-webhooks/1.0")

	resp, err := w.client.Do(req)
	if err != nil {
		// Network error — retryable.
		w.retryOrDLQ(ctx, ch, job, nil, err.Error(), msg)
		return
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Success — write back, ack.
		if err := w.store.MarkDelivered(ctx, job.DeliveryUUID, job.Attempt, resp.StatusCode, string(excerpt)); err != nil {
			w.logger.Error("MarkDelivered failed", "error", err, "delivery_uuid", job.DeliveryUUID)
		}
		_ = msg.Ack(false)
		w.logger.Info("webhook delivered",
			"delivery_uuid", job.DeliveryUUID, "target_url", job.TargetURL,
			"http_status", resp.StatusCode, "attempt", job.Attempt)
		return
	}

	// Non-2xx — retryable up to MaxAttempts. 4xx other than 408/429
	// usually means the consumer's endpoint is broken on their end;
	// for now we treat all non-2xx the same. A future refinement
	// could short-circuit on 410 Gone / 404 to disable the sub.
	w.retryOrDLQ(ctx, ch, job, &resp.StatusCode, string(excerpt), msg)
}

// retryOrDLQ either republishes the job with the next attempt + delay,
// or sends it to DLQ if MaxAttempts is reached. Always acks the
// current message (we've taken responsibility for it).
func (w *Worker) retryOrDLQ(ctx context.Context, ch *amqp.Channel, job matcher.DeliveryJob, httpStatus *int, errMsg string, msg amqp.Delivery) {
	if job.Attempt >= w.cfg.MaxAttempts {
		w.terminalFailure(ctx, job, httpStatus, errMsg, msg)
		return
	}

	delay := backoff(w.cfg.BackoffBase, w.cfg.BackoffMultiplier, job.Attempt)
	nextRetry := time.Now().UTC().Add(delay)
	if err := w.store.MarkAttempt(ctx, job.DeliveryUUID, job.Attempt, httpStatus, errMsg, &nextRetry, false); err != nil {
		w.logger.Error("MarkAttempt failed", "error", err, "delivery_uuid", job.DeliveryUUID)
	}

	body, _ := json.Marshal(job)
	// Per-message TTL + dead-letter routing back to the same exchange
	// would be the polished RabbitMQ delay pattern; that requires a
	// distinct retry queue topology. For now we sleep in the worker
	// (lightweight, but blocks this consumer slot). Prefetch=8 means
	// other deliveries keep flowing.
	w.logger.Warn("webhook delivery failed; sleeping then republishing",
		"delivery_uuid", job.DeliveryUUID, "attempt", job.Attempt,
		"delay", delay, "error", errMsg)
	select {
	case <-ctx.Done():
		_ = msg.Nack(false, true) // requeue on shutdown
		return
	case <-time.After(delay):
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ch.PublishWithContext(pubCtx, w.cfg.Exchange, w.cfg.RoutingKey, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    job.DeliveryUUID,
			Type:         job.EventType,
			Body:         body,
		}); err != nil {
		w.logger.Error("republish failed; nack with requeue", "error", err)
		_ = msg.Nack(false, true)
		return
	}
	_ = msg.Ack(false)
}

func (w *Worker) terminalFailure(ctx context.Context, job matcher.DeliveryJob, httpStatus *int, errMsg string, msg amqp.Delivery) {
	if err := w.store.MarkAttempt(ctx, job.DeliveryUUID, job.Attempt, httpStatus, errMsg, nil, true); err != nil {
		w.logger.Error("MarkAttempt (terminal) failed", "error", err, "delivery_uuid", job.DeliveryUUID)
	}
	_ = msg.Nack(false, false) // dead-letter to DLX
	w.logger.Warn("webhook delivery terminal failure → DLQ",
		"delivery_uuid", job.DeliveryUUID, "attempt", job.Attempt, "error", errMsg)
}

// signRequest computes the body bytes, the timestamp, and the
// HMAC-SHA256 signature header value.
//
// Signature input: "<unix_ts_seconds>.<json_body>" — same shape as
// Stripe-style signing. The consumer verifies with the same recipe:
//
//	expected = hmac_sha256(secret, ts + "." + raw_body)
//	header   = "t=<ts>,v1=<hex(expected)>"
//
// Consumer must reject when |now - ts| > some tolerance to prevent
// replay.
func signRequest(job matcher.DeliveryJob) (body []byte, sig string, ts int64) {
	// Build the JSON envelope the consumer receives. We wrap the
	// original event so the consumer also knows the delivery context.
	body, _ = json.Marshal(map[string]any{
		"delivery_uuid":   job.DeliveryUUID,
		"subscription_id": job.SubscriptionID,
		"event_type":      job.EventType,
		"event":           json.RawMessage(job.Event),
		"attempt":         job.Attempt,
		"delivered_at":    time.Now().UTC().Format(time.RFC3339Nano),
	})
	ts = time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(job.SecretHex))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(body)
	sig = hex.EncodeToString(mac.Sum(nil))
	return body, sig, ts
}

// backoff computes the delay for attempt n. attempt 1 → base; attempt
// 2 → base*mult; attempt 3 → base*mult^2; etc. Capped at 24h.
func backoff(base time.Duration, mult float64, attempt int) time.Duration {
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= mult
	}
	if d > float64(24*time.Hour) {
		d = float64(24 * time.Hour)
	}
	return time.Duration(d)
}
