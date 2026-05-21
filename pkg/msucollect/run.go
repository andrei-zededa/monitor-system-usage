package msucollect

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

// envSecretRe matches environment variable names that commonly hold secrets.
// When IncludeEnv == msuformat.EnvModeFiltered (the default) these are dropped
// before the env map is recorded in the header.
var envSecretRe = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASS|AUTH|CRED|COOK)`)

// CollectEnv returns the environment to record in the header for the requested
// mode. Mode must be one of the msuformat.EnvMode* constants; an unknown value
// is treated as EnvModeFiltered.
func CollectEnv(mode string) map[string]string {
	if mode == msuformat.EnvModeNone {
		return nil
	}
	raw := os.Environ()
	m := make(map[string]string, len(raw))
	for _, kv := range raw {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		if mode != msuformat.EnvModeAll && envSecretRe.MatchString(k) {
			continue
		}
		m[k] = v
	}
	return m
}

// Config groups every knob the collector needs. Zero values for Interval,
// FlushEveryN, IncludeEnv, Version, and CmdLine receive sensible defaults
// when Run is called.
type Config struct {
	Writer      *msuformat.Writer
	Interval    time.Duration
	FlushEveryN int
	Namespaces  []string
	IncludeEnv  string
	CmdLine     []string
	Env         map[string]string
	Version     string
}

// Run executes the collection loop until ctx is cancelled. It does not close
// cfg.Writer — the caller owns its lifecycle.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Writer == nil {
		return fmt.Errorf("msucollect: Config.Writer is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.FlushEveryN < 1 {
		cfg.FlushEveryN = 6
	}
	switch cfg.IncludeEnv {
	case msuformat.EnvModeFiltered, msuformat.EnvModeAll, msuformat.EnvModeNone:
	case "":
		cfg.IncludeEnv = msuformat.EnvModeFiltered
	default:
		return fmt.Errorf("msucollect: invalid IncludeEnv %q (allowed: filtered|all|none)", cfg.IncludeEnv)
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.CmdLine == nil {
		cfg.CmdLine = append([]string(nil), os.Args...)
	}
	if cfg.Env == nil {
		cfg.Env = CollectEnv(cfg.IncludeEnv)
	}

	c := NewCollector(cfg.Writer, cfg.Interval, cfg.FlushEveryN, cfg.Namespaces,
		cfg.CmdLine, cfg.Env, cfg.IncludeEnv, cfg.Version)

	stop := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stop)
	}()

	return c.Run(stop)
}
