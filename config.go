package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration for TOML strings like "2m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

type AccountConfig struct {
	Mode         string   `toml:"mode"`          // auto | idle+scan | scan | off
	ScanInterval Duration `toml:"scan_interval"` // LIST-STATUS scan of all folders
	IdleExtra    []string `toml:"idle_extra"`    // additional IDLE connections
}

type Config struct {
	MbsyncConfig string   `toml:"mbsync_config"`
	MbsyncBin    string   `toml:"mbsync_bin"`
	AfterSync    string   `toml:"after_sync"`
	Debounce     Duration `toml:"debounce"`
	FullSync     Duration `toml:"full_sync"`
	MaxParallel  int      `toml:"max_parallel"`
	WatchLocal   *bool    `toml:"watch_local"`
	// How long shutdown waits for running mbsync processes before
	// terminating them.
	ShutdownTimeout Duration `toml:"shutdown_timeout"`

	Defaults AccountConfig            `toml:"defaults"`
	Accounts map[string]AccountConfig `toml:"account"`
}

func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir + "/mbidle/config.toml"
	}
	return expandHome("~/.config/mbidle/config.toml")
}

// LoadConfig reads the optional mbidle config and fills in defaults.
func LoadConfig(path string, explicit bool) (*Config, error) {
	cfg := &Config{}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if !errors.Is(err, os.ErrNotExist) || explicit {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	}

	if cfg.MbsyncConfig == "" {
		cfg.MbsyncConfig = defaultMbsyncrc()
	}
	cfg.MbsyncConfig = expandHome(cfg.MbsyncConfig)
	if cfg.MbsyncBin == "" {
		cfg.MbsyncBin = "mbsync"
	}
	if cfg.Debounce.Duration == 0 {
		cfg.Debounce.Duration = 2 * time.Second
	}
	if cfg.FullSync.Duration == 0 {
		cfg.FullSync.Duration = 30 * time.Minute
	}
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 2
	}
	if cfg.ShutdownTimeout.Duration == 0 {
		cfg.ShutdownTimeout.Duration = 10 * time.Second
	}
	if cfg.WatchLocal == nil {
		t := true
		cfg.WatchLocal = &t
	}
	if cfg.Defaults.Mode == "" {
		cfg.Defaults.Mode = "auto"
	}
	if cfg.Defaults.ScanInterval.Duration == 0 {
		cfg.Defaults.ScanInterval.Duration = 2 * time.Minute
	}
	return cfg, nil
}

// Account returns the effective settings for an mbsync channel.
func (c *Config) Account(channel string) AccountConfig {
	ac := c.Defaults
	if o, ok := c.Accounts[channel]; ok {
		if o.Mode != "" {
			ac.Mode = o.Mode
		}
		if o.ScanInterval.Duration != 0 {
			ac.ScanInterval = o.ScanInterval
		}
		if o.IdleExtra != nil {
			ac.IdleExtra = o.IdleExtra
		}
	}
	return ac
}

func defaultMbsyncrc() string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = expandHome("~/.config")
	}
	if p := xdg + "/isyncrc"; fileExists(p) {
		return p
	}
	return expandHome("~/.mbsyncrc")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
