package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Op describes which direction(s) of a mailbox need to be synchronized.
type Op uint8

const (
	OpPullNew Op = 1 << iota // new messages on the server
	OpPull                   // any other server-side change
	OpPush                   // local change
	OpFull    = OpPull | OpPush
)

// mbsyncFlags returns the mbsync operation flags for an op set.
func (o Op) mbsyncFlags() []string {
	switch {
	case o&OpPush != 0 && o&(OpPull|OpPullNew) != 0:
		return nil
	case o&OpPush != 0:
		return []string{"--push"}
	case o&OpPull != 0:
		return []string{"--pull"}
	case o&OpPullNew != 0:
		return []string{"--pull-new"}
	}
	return nil
}

type batch struct {
	all   Op            // op for the whole channel
	boxes map[string]Op // per-box ops
	due   bool          // debounce elapsed
	timer *time.Timer
}

// Scheduler debounces and batches sync requests per channel and runs mbsync
// with at most one process per channel and MaxParallel processes overall.
type Scheduler struct {
	cfg *Config
	log *slog.Logger

	mu       sync.Mutex
	cond     *sync.Cond
	pending  map[string]*batch
	running  map[string][]string // channel -> boxes being synced (nil = all)
	finished map[string]time.Time
	queue    []string
	queued   map[string]bool
	closed   bool

	hookMu      sync.Mutex
	hookPending bool
	hookBoxes   map[string][]string

	wg sync.WaitGroup
}

func NewScheduler(cfg *Config, log *slog.Logger) *Scheduler {
	s := &Scheduler{
		cfg:      cfg,
		log:      log,
		pending:  map[string]*batch{},
		running:  map[string][]string{},
		finished: map[string]time.Time{},
		queued:   map[string]bool{},
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Add requests a sync of box (or the whole channel if box is "").
func (s *Scheduler) Add(channel, box string, op Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	b := s.pending[channel]
	if b == nil {
		b = &batch{boxes: map[string]Op{}}
		s.pending[channel] = b
		b.timer = time.AfterFunc(s.cfg.Debounce.Duration, func() {
			s.mu.Lock()
			b.due = true
			s.kick(channel)
			s.mu.Unlock()
		})
	}
	if box == "" {
		b.all |= op
	} else {
		b.boxes[box] |= op
	}
}

// kick enqueues a channel whose batch is due and which is not running.
// Must be called with s.mu held.
func (s *Scheduler) kick(channel string) {
	b := s.pending[channel]
	if b == nil || !b.due || s.queued[channel] {
		return
	}
	if _, busy := s.running[channel]; busy {
		return
	}
	s.queued[channel] = true
	s.queue = append(s.queue, channel)
	s.cond.Signal()
}

// Quiet reports whether local changes in box of channel are most likely
// caused by mbsync itself (running or just finished).
func (s *Scheduler) Quiet(channel, box string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if boxes, busy := s.running[channel]; busy {
		if boxes == nil {
			return true
		}
		for _, b := range boxes {
			if b == box {
				return true
			}
		}
		return false
	}
	return time.Since(s.finished[channel]) < 2*time.Second
}

// Run starts the worker pool and blocks until ctx is cancelled and all
// running mbsync processes have exited.
func (s *Scheduler) Run(ctx context.Context) {
	for i := 0; i < s.cfg.MaxParallel; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	<-ctx.Done()
	s.mu.Lock()
	s.closed = true
	for _, b := range s.pending {
		b.timer.Stop()
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		channel := s.queue[0]
		s.queue = s.queue[1:]
		delete(s.queued, channel)
		b := s.pending[channel]
		delete(s.pending, channel)

		op, boxes := b.all, []string(nil)
		if b.all&OpFull != OpFull {
			for box, o := range b.boxes {
				op |= o
				boxes = append(boxes, box)
			}
			sort.Strings(boxes)
			if b.all != 0 {
				// A channel-wide request covers all boxes.
				boxes = nil
			}
		}
		s.running[channel] = boxes
		s.mu.Unlock()

		ok := s.runMbsync(channel, boxes, op)

		s.mu.Lock()
		delete(s.running, channel)
		s.finished[channel] = time.Now()
		s.kick(channel)
		s.mu.Unlock()

		if ok {
			s.afterSync(channel, boxes)
		}
	}
}

func (s *Scheduler) runMbsync(channel string, boxes []string, op Op) bool {
	target := channel
	if len(boxes) > 0 {
		target += ":" + strings.Join(boxes, ",")
	}
	args := append(op.mbsyncFlags(), target)

	start := time.Now()
	s.log.Info("sync", "cmd", s.cfg.MbsyncBin+" "+strings.Join(args, " "))
	var stderr bytes.Buffer
	cmd := exec.Command(s.cfg.MbsyncBin, args...)
	cmd.Stderr = &stderr
	// Own process group: a Ctrl-C in the terminal must not kill mbsync
	// mid-sync; shutdown waits for it instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := cmd.Run()
	if err != nil {
		s.log.Warn("sync failed", "channel", channel, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		return false
	}
	s.log.Debug("sync done", "channel", channel, "took", time.Since(start).Round(time.Millisecond))
	return true
}

// afterSync runs the after_sync hook, coalescing syncs that finish while the
// hook is already running or scheduled.
func (s *Scheduler) afterSync(channel string, boxes []string) {
	if s.cfg.AfterSync == "" {
		return
	}
	s.hookMu.Lock()
	if s.hookBoxes == nil {
		s.hookBoxes = map[string][]string{}
	}
	s.hookBoxes[channel] = append(s.hookBoxes[channel], boxes...)
	if s.hookPending {
		s.hookMu.Unlock()
		return
	}
	s.hookPending = true
	s.hookMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		time.Sleep(s.cfg.Debounce.Duration)
		s.hookMu.Lock()
		synced := s.hookBoxes
		s.hookBoxes = nil
		s.hookPending = false
		s.hookMu.Unlock()

		var channels, folders []string
		for ch, bs := range synced {
			channels = append(channels, ch)
			for _, b := range bs {
				folders = append(folders, ch+":"+b)
			}
		}
		sort.Strings(channels)
		sort.Strings(folders)

		cmd := exec.Command("sh", "-c", s.cfg.AfterSync)
		cmd.Env = append(os.Environ(),
			"MBIDLE_CHANNELS="+strings.Join(channels, " "),
			"MBIDLE_FOLDERS="+strings.Join(folders, "\n"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			s.log.Warn("after_sync failed", "err", err, "output", strings.TrimSpace(string(out)))
		}
	}()
}
