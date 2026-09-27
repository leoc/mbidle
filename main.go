// Command mbidle watches IMAP accounts configured for mbsync and runs
// targeted mbsync syncs whenever a folder changes on the server or locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
)

func main() {
	configPath := flag.String("config", "", "mbidle config file (default ~/.config/mbidle/config.toml)")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mbidle [-config file] [-v] [run|check] [channel ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	path, explicit := *configPath, *configPath != ""
	if !explicit {
		path = defaultConfigPath()
	}
	cfg, err := LoadConfig(path, explicit)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	channels, err := ParseMbsyncrc(cfg.MbsyncConfig)
	if err != nil {
		log.Error("mbsync config", "err", err)
		os.Exit(1)
	}

	cmd, args := "run", flag.Args()
	if len(args) > 0 && (args[0] == "run" || args[0] == "check") {
		cmd, args = args[0], args[1:]
	}
	if len(args) > 0 {
		channels = slices.DeleteFunc(channels, func(c *Channel) bool { return !slices.Contains(args, c.Name) })
	}
	if len(channels) == 0 {
		log.Error("no matching IMAP/Maildir channels", "mbsync_config", cfg.MbsyncConfig)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "check":
		if !check(ctx, cfg, channels, log) {
			os.Exit(1)
		}
	default:
		run(ctx, cfg, channels, log)
	}
}

func run(ctx context.Context, cfg *Config, channels []*Channel, log *slog.Logger) {
	sched := NewScheduler(cfg, log)
	schedDone := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(schedDone)
	}()

	var wg sync.WaitGroup
	for _, ch := range channels {
		w := &Watcher{ch: ch, cfg: cfg.Account(ch.Name), sched: sched, log: log.With("channel", ch.Name)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(ctx)
		}()
	}

	if *cfg.WatchLocal {
		lw, err := NewLocalWatcher(channels, sched, log)
		if err != nil {
			log.Warn("local watching disabled", "err", err)
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lw.Run(ctx)
			}()
		}
	}

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	full := time.NewTicker(cfg.FullSync.Duration)
	defer full.Stop()
	syncAll := func(reason string) {
		log.Info("full sync", "reason", reason)
		for _, ch := range channels {
			sched.Add(ch.Name, "", OpFull)
		}
	}

	log.Info("mbidle started", "channels", len(channels), "mbsync_config", cfg.MbsyncConfig)
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down, waiting for running syncs")
			wg.Wait()
			<-schedDone
			return
		case <-usr1:
			syncAll("SIGUSR1")
		case <-full.C:
			syncAll("interval")
		}
	}
}

// check logs into every account and reports capabilities, the chosen mode
// and the folders mbidle would watch, without syncing anything.
func check(ctx context.Context, cfg *Config, channels []*Channel, log *slog.Logger) bool {
	ok := true
	for _, ch := range channels {
		ac := cfg.Account(ch.Name)
		w := &Watcher{ch: ch, cfg: ac, log: log.With("channel", ch.Name)}
		fmt.Printf("%s (%s@%s)\n", ch.Name, ch.Account.User, ch.Account.Addr())

		c, err := w.Connect(ctx, nil)
		if err != nil {
			fmt.Printf("  error: %v\n\n", err)
			ok = false
			continue
		}
		caps := c.Caps()
		var have []string
		for _, capName := range []imap.Cap{imap.CapIdle, imap.CapNotify, imap.CapListStatus, imap.CapCondStore, imap.CapQResync} {
			if caps.Has(capName) {
				have = append(have, string(capName))
			}
		}
		mode := ac.Mode
		if mode == "auto" {
			mode = "scan"
			if caps.Has(imap.CapIdle) {
				mode = "idle+scan"
			}
		}
		fmt.Printf("  capabilities: %s\n", strings.Join(have, " "))
		fmt.Printf("  mode:         %s (scan every %s)\n", mode, ac.ScanInterval.Duration)
		if len(ac.IdleExtra) > 0 {
			fmt.Printf("  idle_extra:   %s\n", strings.Join(ac.IdleExtra, ", "))
		}

		state, err := (&Watcher{ch: ch, cfg: ac, log: w.log}).scan(c, nil)
		if err != nil {
			fmt.Printf("  error: %v\n\n", err)
			ok = false
			c.Close()
			continue
		}
		boxes := make([]string, 0, len(state))
		for b := range state {
			boxes = append(boxes, b)
		}
		slices.Sort(boxes)
		fmt.Printf("  folders (%d): %s\n\n", len(boxes), strings.Join(boxes, ", "))
		c.Logout().Wait()
		c.Close()
	}
	return ok
}
