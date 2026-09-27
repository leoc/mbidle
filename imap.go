package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	cmdTimeout     = 90 * time.Second
	minBackoff     = 10 * time.Second
	maxBackoff     = 5 * time.Minute
	stableDuration = 2 * time.Minute
)

// boxState is the part of a mailbox STATUS used for change detection.
type boxState struct {
	UIDValidity uint32
	UIDNext     imap.UID
	Messages    uint32
	Unseen      uint32
	ModSeq      uint64
}

// diff classifies the change between two states.
func (old boxState) diff(cur boxState) Op {
	switch {
	case old == cur:
		return 0
	case old.UIDValidity == cur.UIDValidity && cur.UIDNext > old.UIDNext &&
		cur.Messages-old.Messages == uint32(cur.UIDNext-old.UIDNext):
		// Only appends: no expunges in between.
		return OpPullNew
	default:
		return OpPull
	}
}

// Watcher observes the IMAP side of one channel.
type Watcher struct {
	ch    *Channel
	cfg   AccountConfig
	sched *Scheduler
	log   *slog.Logger
}

// Run keeps the main connection (IDLE on INBOX + folder scan) and the
// optional extra IDLE connections alive until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	mode := w.cfg.Mode
	if mode == "off" {
		w.log.Info("watching disabled")
		return
	}
	for _, box := range w.cfg.IdleExtra {
		go w.keepAlive(ctx, func(ctx context.Context) error { return w.session(ctx, box, false) })
	}
	w.keepAlive(ctx, func(ctx context.Context) error { return w.session(ctx, "INBOX", mode != "scan") })
}

func (w *Watcher) keepAlive(ctx context.Context, session func(context.Context) error) {
	backoff := minBackoff
	for {
		start := time.Now()
		err := session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > stableDuration {
			backoff = minBackoff
		}
		w.log.Warn("connection lost, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

type event struct {
	op Op
}

// Connect dials and authenticates. events receives unilateral server data.
func (w *Watcher) Connect(ctx context.Context, events chan<- event) (*imapclient.Client, error) {
	a := w.ch.Account
	tlsConfig := &tls.Config{ServerName: a.Host}
	if a.CertFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if pem, err := os.ReadFile(a.CertFile); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
		tlsConfig.RootCAs = pool
	}

	send := func(op Op) {
		if events == nil {
			return
		}
		select {
		case events <- event{op}:
		default: // a sync is already pending
		}
	}
	opts := &imapclient.Options{
		TLSConfig: tlsConfig,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					send(OpPullNew)
				}
			},
			Expunge: func(uint32) { send(OpPull) },
			Fetch: func(msg *imapclient.FetchMessageData) {
				msg.Collect()
				send(OpPull)
			},
		},
	}

	var (
		c   *imapclient.Client
		err error
	)
	switch strings.ToUpper(a.TLSType) {
	case "IMAPS":
		c, err = imapclient.DialTLS(a.Addr(), opts)
	case "NONE":
		c, err = imapclient.DialInsecure(a.Addr(), opts)
	default:
		c, err = imapclient.DialStartTLS(a.Addr(), opts)
	}
	if err != nil {
		return nil, err
	}

	pass, err := passwords.Get(ctx, a)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := wait(c, c.Login(a.User, pass)); err != nil {
		c.Close()
		var ie *imap.Error
		if errors.As(err, &ie) {
			passwords.Forget(a)
		}
		return nil, fmt.Errorf("login: %w", err)
	}
	return c, nil
}

// session runs one connection. With scan, it additionally re-checks all
// folders of the channel every scan interval via LIST-STATUS.
func (w *Watcher) session(ctx context.Context, idleBox string, scan bool) error {
	events := make(chan event, 1)
	c, err := w.Connect(ctx, events)
	if err != nil {
		return err
	}
	defer c.Close()
	log := w.log.With("folder", idleBox)

	caps := c.Caps()
	canIdle := caps.Has(imap.CapIdle) || caps.Has(imap.CapIMAP4rev2)
	if !scan && !canIdle {
		return fmt.Errorf("server does not support IDLE")
	}

	idleBoxName := w.farName(idleBox)
	var state map[string]boxState
	if scan {
		// We may have missed changes while disconnected.
		w.sched.Add(w.ch.Name, "", OpFull)
		if state, err = w.scan(c, nil); err != nil {
			return err
		}
		mode := "scan"
		if canIdle {
			mode = "idle+scan"
		}
		log.Info("connected", "mode", mode, "folders", len(state), "scan_interval", w.cfg.ScanInterval.Duration)
	} else {
		log.Info("connected", "mode", "idle")
	}

	if canIdle {
		if _, err := waitSelect(c, idleBoxName); err != nil {
			return fmt.Errorf("examine %s: %w", idleBox, err)
		}
	}

	interval := w.cfg.ScanInterval.Duration
	if !scan {
		// Extra IDLE connections still wake up regularly to detect dead
		// connections (e.g. after suspend).
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		var idle *imapclient.IdleCommand
		if canIdle {
			if idle, err = c.Idle(); err != nil {
				return fmt.Errorf("idle: %w", err)
			}
		}

		stop := false
		for !stop {
			select {
			case <-ctx.Done():
				if idle != nil {
					idle.Close()
				}
				c.Logout().Wait()
				return nil
			case <-c.Closed():
				return errors.New("connection closed by server")
			case ev := <-events:
				log.Debug("server event", "op", ev.op)
				w.sched.Add(w.ch.Name, idleBox, ev.op)
			case <-ticker.C:
				stop = true
			}
		}

		if idle != nil {
			if err := withTimeout(c, func() error {
				if err := idle.Close(); err != nil {
					return err
				}
				return idle.Wait()
			}); err != nil {
				return fmt.Errorf("stop idle: %w", err)
			}
		}
		if scan {
			if state, err = w.scan(c, state); err != nil {
				return err
			}
		} else if err := wait(c, c.Noop()); err != nil {
			return err
		}
	}
}

// scan fetches the state of all channel folders in one LIST-STATUS round
// trip (or STATUS per folder without LIST-STATUS) and schedules syncs for
// folders that differ from prev.
func (w *Watcher) scan(c *imapclient.Client, prev map[string]boxState) (map[string]boxState, error) {
	caps := c.Caps()
	statusOpts := &imap.StatusOptions{
		NumMessages: true,
		UIDNext:     true,
		UIDValidity: true,
		NumUnseen:   true,
		// Gmail's HIGHESTMODSEQ is account-wide: every change anywhere would
		// mark all folders as changed. UNSEEN still catches read/unread.
		HighestModSeq: caps.Has(imap.CapCondStore) && !caps.Has("X-GM-EXT-1"),
	}
	listStatus := caps.Has(imap.CapListStatus) || caps.Has(imap.CapIMAP4rev2)

	var listOpts *imap.ListOptions
	if listStatus {
		listOpts = &imap.ListOptions{ReturnStatus: statusOpts}
	}
	var list []*imap.ListData
	err := withTimeout(c, func() (err error) {
		list, err = c.List("", "*", listOpts).Collect()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}

	selected := ""
	if mbox := c.Mailbox(); mbox != nil {
		selected = mbox.Name
	}

	state := map[string]boxState{}
	for _, l := range list {
		if hasAttr(l.Attrs, imap.MailboxAttrNoSelect) || hasAttr(l.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		box, ok := w.ch.FarToBox(l.Mailbox, l.Delim)
		if !ok {
			continue
		}
		st := l.Status
		if st == nil {
			if listStatus && l.Mailbox == selected {
				// Servers may omit STATUS for the selected mailbox; IDLE
				// covers it anyway.
				continue
			}
			err := withTimeout(c, func() (err error) {
				st, err = c.Status(l.Mailbox, statusOpts).Wait()
				return err
			})
			if err != nil {
				w.log.Debug("status failed", "folder", box, "err", err)
				continue
			}
		}
		cur := boxState{st.UIDValidity, st.UIDNext, deref(st.NumMessages), deref(st.NumUnseen), st.HighestModSeq}
		state[box] = cur

		if prev == nil {
			continue
		}
		old, known := prev[box]
		if !known {
			w.log.Debug("new folder", "folder", box)
			w.sched.Add(w.ch.Name, box, OpPull)
			continue
		}
		if op := old.diff(cur); op != 0 {
			w.log.Debug("folder changed", "folder", box, "op", op)
			w.sched.Add(w.ch.Name, box, op)
		}
	}
	return state, nil
}

// farName maps a canonical box name back to the server name (without
// delimiter translation, which is only needed for non-"/" servers).
func (w *Watcher) farName(box string) string {
	if box == "INBOX" && w.ch.FarPath == "" {
		return "INBOX"
	}
	return w.ch.FarPath + box
}

func hasAttr(attrs []imap.MailboxAttr, a imap.MailboxAttr) bool {
	for _, x := range attrs {
		if strings.EqualFold(string(x), string(a)) {
			return true
		}
	}
	return false
}

// withTimeout closes the connection if f does not return in time, which
// unblocks f and turns a hanging connection into an error.
func withTimeout(c *imapclient.Client, f func() error) error {
	t := time.AfterFunc(cmdTimeout, func() { c.Close() })
	defer t.Stop()
	return f()
}

func wait(c *imapclient.Client, cmd *imapclient.Command) error {
	return withTimeout(c, cmd.Wait)
}

func waitSelect(c *imapclient.Client, box string) (*imap.SelectData, error) {
	var data *imap.SelectData
	err := withTimeout(c, func() (err error) {
		data, err = c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	return data, err
}

func deref(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}
