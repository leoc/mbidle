package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// passMu serializes PassCmd invocations so a locked vault (rbw, pass, …)
// prompts only once instead of once per account.
var passMu sync.Mutex

type passCache struct {
	mu    sync.Mutex
	cache map[string]string
}

var passwords = &passCache{cache: map[string]string{}}

// Get returns the account password, running PassCmd if necessary.
func (p *passCache) Get(ctx context.Context, a *IMAPAccount) (string, error) {
	if a.Pass != "" {
		return a.Pass, nil
	}
	if a.PassCmd == "" {
		return "", fmt.Errorf("neither Pass nor PassCmd configured")
	}

	p.mu.Lock()
	pw, ok := p.cache[a.Name]
	p.mu.Unlock()
	if ok {
		return pw, nil
	}

	passMu.Lock()
	defer passMu.Unlock()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", a.PassCmd)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("PassCmd failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	pw, _, _ = strings.Cut(string(out), "\n")
	if pw == "" {
		return "", fmt.Errorf("PassCmd returned an empty password")
	}

	p.mu.Lock()
	p.cache[a.Name] = pw
	p.mu.Unlock()
	return pw, nil
}

// Forget drops a cached password, e.g. after an authentication failure.
func (p *passCache) Forget(a *IMAPAccount) {
	p.mu.Lock()
	delete(p.cache, a.Name)
	p.mu.Unlock()
}
