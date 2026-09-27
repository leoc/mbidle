package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// LocalWatcher watches the Maildir stores of all channels with inotify and
// schedules push syncs for local changes.
type LocalWatcher struct {
	channels []*Channel
	sched    *Scheduler
	log      *slog.Logger
	fsw      *fsnotify.Watcher
}

func NewLocalWatcher(channels []*Channel, sched *Scheduler, log *slog.Logger) (*LocalWatcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	lw := &LocalWatcher{channels: channels, sched: sched, log: log, fsw: fsw}
	for _, ch := range channels {
		for _, root := range []string{ch.Maildir.Path, ch.Maildir.Inbox} {
			if root != "" {
				lw.addTree(root)
			}
		}
	}
	return lw, nil
}

// addTree watches every directory below root except maildir tmp/ dirs.
// Watching the folder directories themselves catches newly created folders,
// watching cur/ and new/ catches message changes.
func (lw *LocalWatcher) addTree(root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == "tmp" && isMaildir(filepath.Dir(p)) {
			return fs.SkipDir
		}
		if err := lw.fsw.Add(p); err != nil {
			lw.log.Warn("cannot watch", "dir", p, "err", err)
		}
		return nil
	})
}

func isMaildir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "cur"))
	return err == nil && st.IsDir()
}

func (lw *LocalWatcher) Run(ctx context.Context) {
	defer lw.fsw.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-lw.fsw.Errors:
			lw.log.Warn("inotify error", "err", err)
		case ev := <-lw.fsw.Events:
			lw.handle(ev)
		}
	}
}

func (lw *LocalWatcher) handle(ev fsnotify.Event) {
	if ev.Has(fsnotify.Chmod) {
		return
	}
	name := filepath.Base(ev.Name)
	if strings.HasPrefix(name, ".mbsyncstate") || name == ".uidvalidity" || name == ".isyncuidmap.db" {
		return
	}

	dir := filepath.Dir(ev.Name)
	switch filepath.Base(dir) {
	case "cur", "new":
		dir = filepath.Dir(dir)
		if !isMaildir(dir) {
			return
		}
	default:
		// A directory was created somewhere in the tree: watch it (new
		// folder). Its box is synced once cur/ appears in it.
		if !ev.Has(fsnotify.Create) {
			return
		}
		if st, err := os.Stat(ev.Name); err != nil || !st.IsDir() {
			return
		}
		lw.addTree(ev.Name)
		switch {
		case isMaildir(ev.Name):
			dir = ev.Name
		case name == "cur":
			// dir is the new folder whose cur/ was just created.
		default:
			return
		}
	}

	for _, ch := range lw.channels {
		box, ok := ch.Maildir.DirToBox(dir)
		if !ok || !MatchPatterns(ch.Patterns, box) {
			continue
		}
		if lw.sched.Quiet(ch.Name, box) {
			return
		}
		lw.log.Debug("local change", "channel", ch.Name, "folder", box, "file", name)
		lw.sched.Add(ch.Name, box, OpPush)
		return
	}
}
