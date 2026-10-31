package door

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

const eventRetryLifetime = time.Hour

// Wake is the mailbox hook. It never waits for the event loop or a callback.
func (s *Server) Wake() {
	select {
	case s.eventWake <- struct{}{}:
	default:
	}
}

// RunEvents queues new mail and delivers due events until shutdown. Run once,
// alongside the mailbox, and wait for it to return before closing either store.
func (s *Server) RunEvents(ctx context.Context) {
	if s.mailbox == nil {
		return
	}
	s.mailbox.SetDoorEvents(s)
	defer s.mailbox.SetDoorEvents(nil)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := s.queueEvents(); err != nil {
			slog.Error("door events queue", "err", err)
		}
		if err := s.deliverEvents(ctx); err != nil && ctx.Err() == nil {
			slog.Error("door events store", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.eventWake:
		case <-tick.C:
		}
	}
}

// Mail and this queue live in separate databases. Save the mailbox position
// in the same transaction as the events: after a crash we either see both or
// reread the same mail. No callback can change a message's read or run state.
func (s *Server) queueEvents() error {
	var position int64
	if err := s.db.QueryRow(`SELECT seq FROM event_position WHERE id=1`).Scan(&position); err != nil {
		return err
	}
	messages, err := s.mailbox.DoorMessages(position)
	if err != nil || len(messages) == 0 {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,agent,after_seq FROM subscriptions WHERE expires_at>?`, s.now().UnixMilli())
	if err != nil {
		return err
	}
	type subscription struct {
		id, agent string
		after     int64
	}
	var subs []subscription
	for rows.Next() {
		var sub subscription
		if err := rows.Scan(&sub.id, &sub.agent, &sub.after); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, sub)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range messages {
		expires := m.ReceivedAt.Add(eventRetryLifetime)
		if !expires.After(s.now()) {
			continue
		}
		for _, sub := range subs {
			if sub.agent != m.Message.To || sub.after >= m.Sequence {
				continue
			}
			env := m.Message
			h := sha256.Sum256([]byte(m.From + "\n" + env.ID + "\n" + sub.id))
			id := "evt_" + hex.EncodeToString(h[:16])
			body, err := json.Marshal(event{
				ID: id, Name: eventName, Timestamp: m.ReceivedAt.UTC(),
				Data: eventMessage{ID: env.ID, From: m.From, To: env.To, Kind: env.Kind,
					Text: env.Body.Text, FromThread: env.FromThread, ToThread: env.ToThread,
					ReplyTo: env.ReplyTo, State: env.Body.State, Files: env.Body.Files},
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO event_deliveries
 (id,subscription,body,next_try_at,expires_at) VALUES (?,?,?,?,?)`,
				id, sub.id, body, s.now().UnixMilli(), expires.UnixMilli()); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE event_position SET seq=? WHERE id=1`, messages[len(messages)-1].Sequence); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(messages) == 100 {
		s.Wake()
	}
	return nil
}

func (s *Server) deliverEvents(ctx context.Context) error {
	now := s.now().UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM subscriptions WHERE expires_at<=?`, now); err != nil {
		return err
	}
	// Giving up is quiet. The mail still waits in inbox.
	if _, err := s.db.Exec(`DELETE FROM event_deliveries WHERE expires_at<=?`, now); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT id FROM event_deliveries WHERE next_try_at<=?
 ORDER BY next_try_at,id LIMIT 20`, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.deliverEvent(ctx, id); err != nil {
			return err
		}
	}
	if len(ids) == 20 {
		s.Wake()
	}
	return nil
}

func (s *Server) deliverEvent(ctx context.Context, id string) error {
	var sub, principal, url, secret, generation string
	var body []byte
	var attempts int
	// Recheck immediately before each attempt, including queued work selected
	// before a revoke, unsubscribe or expiry. An HTTP request already in flight
	// cannot be recalled, but nothing further is scheduled after removal.
	err := s.db.QueryRow(`SELECT s.id,s.agent,s.url,s.secret,s.generation,d.body,d.attempts
 FROM event_deliveries d JOIN subscriptions s ON s.id=d.subscription
 JOIN connectors c ON c.agent=s.agent
 WHERE d.id=? AND d.expires_at>? AND s.expires_at>?`,
		id, s.now().UnixMilli(), s.now().UnixMilli()).Scan(
		&sub, &principal, &url, &secret, &generation, &body, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	status := 0
	err = s.allowed(principal)
	if err == nil {
		status, _, err = s.postEvent(ctx, url, id, sub, secret, body)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && (status/100 == 2 || status == 410 || status == 413) {
		if status == 410 {
			// A late response from an old callback must not remove a renewal
			// or a replacement subscription with the same public id.
			_, err = s.db.Exec(`DELETE FROM subscriptions WHERE id=? AND generation=?`, sub, generation)
		} else {
			_, err = s.db.Exec(`DELETE FROM event_deliveries WHERE id=?`, id)
		}
		if err == nil && status/100 == 2 {
			slog.Info("door events delivered", "agent", principal, "event", id, "subscription", sub, "status", status)
		}
		return err
	}
	attempts++
	delay := min(2*time.Second*time.Duration(1<<min(attempts-1, 8)), 5*time.Minute)
	_, err = s.db.Exec(`UPDATE event_deliveries SET attempts=?,next_try_at=? WHERE id=?`,
		attempts, s.now().Add(delay).UnixMilli(), id)
	if err == nil {
		slog.Info("door events retry", "agent", principal, "event", id, "attempt", attempts, "status", status)
	}
	return err
}
