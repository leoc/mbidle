package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IMAPAccount holds connection settings from an IMAPAccount section (or an
// IMAPStore with inline connection settings).
type IMAPAccount struct {
	Name     string
	Host     string
	Port     int
	User     string
	Pass     string
	PassCmd  string
	TLSType  string // IMAPS, STARTTLS or None
	CertFile string
}

// MaildirStore holds the local side of a channel.
type MaildirStore struct {
	Name       string
	Path       string
	Inbox      string
	SubFolders string // Verbatim, Maildir++, Legacy or ""
	Flatten    string
}

// Channel ties a far IMAP store to a near Maildir store.
type Channel struct {
	Name     string
	Account  *IMAPAccount
	FarPath  string // IMAPStore Path + Far mailbox prefix
	Maildir  *MaildirStore
	Patterns []string
}

type imapStore struct {
	account *IMAPAccount
	path    string
}

// ParseMbsyncrc reads an mbsync configuration and returns all channels that
// connect an IMAP store with a Maildir store.
func ParseMbsyncrc(path string) ([]*Channel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	accounts := map[string]*IMAPAccount{}
	imapStores := map[string]*imapStore{}
	maildirs := map[string]*MaildirStore{}
	type rawChannel struct {
		name, far, near string
		patterns        []string
	}
	var rawChannels []*rawChannel

	var (
		curAccount *IMAPAccount
		curStore   *imapStore
		curMaildir *MaildirStore
		curChannel *rawChannel
	)
	reset := func() { curAccount, curStore, curMaildir, curChannel = nil, nil, nil, nil }

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			// An empty line terminates the current section.
			reset()
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)

		switch key {
		case "IMAPAccount":
			reset()
			curAccount = &IMAPAccount{Name: value}
			accounts[value] = curAccount
			continue
		case "IMAPStore":
			reset()
			curStore = &imapStore{}
			imapStores[value] = curStore
			continue
		case "MaildirStore":
			reset()
			curMaildir = &MaildirStore{Name: value}
			maildirs[value] = curMaildir
			continue
		case "Channel":
			reset()
			curChannel = &rawChannel{name: value}
			rawChannels = append(rawChannels, curChannel)
			continue
		case "Group":
			reset()
			continue
		}

		// IMAPStore sections may carry inline account settings.
		if curStore != nil {
			if key == "Account" {
				curStore.account = accounts[value]
				continue
			}
			if key == "Path" {
				curStore.path = unquote(value)
				continue
			}
			if curStore.account == nil {
				curStore.account = &IMAPAccount{}
			}
			curAccount = curStore.account
		}

		switch {
		case curAccount != nil:
			if err := curAccount.set(key, value); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
		case curMaildir != nil:
			switch key {
			case "Path":
				curMaildir.Path = expandHome(unquote(value))
			case "Inbox":
				curMaildir.Inbox = expandHome(unquote(value))
			case "SubFolders":
				curMaildir.SubFolders = value
			case "Flatten":
				curMaildir.Flatten = unquote(value)
			}
		case curChannel != nil:
			switch key {
			case "Far", "Master":
				curChannel.far = value
			case "Near", "Slave":
				curChannel.near = value
			case "Pattern", "Patterns":
				curChannel.patterns = append(curChannel.patterns, splitArgs(value)...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var channels []*Channel
	for _, rc := range rawChannels {
		farStore, farBox := splitStoreRef(rc.far)
		nearStore, _ := splitStoreRef(rc.near)
		is, md := imapStores[farStore], maildirs[nearStore]
		if is == nil || md == nil {
			// Swapped direction (IMAP near, Maildir far) is not supported.
			continue
		}
		if is.account == nil || is.account.Host == "" {
			return nil, fmt.Errorf("channel %s: IMAP store %s has no account/host", rc.name, farStore)
		}
		if is.account.Name == "" {
			is.account.Name = farStore
		}
		if md.Inbox == "" {
			md.Inbox = expandHome("~/Maildir")
		}
		patterns := rc.patterns
		if len(patterns) == 0 {
			// Without Patterns mbsync syncs exactly the Far/Near mailbox.
			if farBox == "" {
				farBox = "INBOX"
			}
			patterns = []string{farBox}
			farBox = ""
		}
		channels = append(channels, &Channel{
			Name:     rc.name,
			Account:  is.account,
			FarPath:  is.path + farBox,
			Maildir:  md,
			Patterns: patterns,
		})
	}
	return channels, nil
}

func (a *IMAPAccount) set(key, value string) error {
	switch key {
	case "Host":
		a.Host = value
	case "Port":
		p, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid port %q", value)
		}
		a.Port = p
	case "User":
		a.User = unquote(value)
	case "Pass":
		a.Pass = unquote(value)
	case "PassCmd":
		a.PassCmd = strings.TrimPrefix(unquote(value), "+")
	case "TLSType", "SSLType":
		a.TLSType = value
	case "CertificateFile":
		a.CertFile = expandHome(unquote(value))
	case "UseIMAPS":
		if value == "yes" {
			a.TLSType = "IMAPS"
		}
	}
	return nil
}

// Addr returns host:port with mbsync's default port.
func (a *IMAPAccount) Addr() string {
	port := a.Port
	if port == 0 {
		port = 143
		if strings.EqualFold(a.TLSType, "IMAPS") {
			port = 993
		}
	}
	return fmt.Sprintf("%s:%d", a.Host, port)
}

// FarToBox converts a server mailbox name into mbsync's canonical box name
// (hierarchy delimiter "/", Far prefix stripped). ok is false if the mailbox
// lies outside the channel's prefix or is not selected by the Patterns.
func (c *Channel) FarToBox(name string, delim rune) (box string, ok bool) {
	if delim != 0 && delim != '/' {
		name = strings.ReplaceAll(name, string(delim), "/")
	}
	if !strings.EqualFold(name, "INBOX") || c.FarPath != "" {
		if !strings.HasPrefix(name, c.FarPath) {
			return "", false
		}
		name = strings.TrimPrefix(name, c.FarPath)
	} else {
		name = "INBOX"
	}
	if name == "" {
		return "", false
	}
	return name, MatchPatterns(c.Patterns, name)
}

// MatchPatterns applies mbsync channel patterns; later matches win.
func MatchPatterns(patterns []string, box string) bool {
	matched := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if matchIMAPPattern(p, box) {
			matched = !neg
		}
	}
	return matched
}

// matchIMAPPattern implements IMAP LIST wildcards: "*" matches anything,
// "%" matches anything but the hierarchy delimiter "/".
func matchIMAPPattern(pattern, name string) bool {
	if pattern == "" {
		return name == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(name); i++ {
			if matchIMAPPattern(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	case '%':
		for i := 0; i <= len(name); i++ {
			if matchIMAPPattern(pattern[1:], name[i:]) {
				return true
			}
			if i < len(name) && name[i] == '/' {
				return false
			}
		}
		return false
	default:
		if name == "" || pattern[0] != name[0] {
			return false
		}
		return matchIMAPPattern(pattern[1:], name[1:])
	}
}

// DirToBox maps a local maildir directory to its mbsync box name.
func (m *MaildirStore) DirToBox(dir string) (string, bool) {
	dir = filepath.Clean(dir)
	inbox := filepath.Clean(m.Inbox)
	if dir == inbox {
		return "INBOX", true
	}
	if rel, ok := relPath(inbox, dir); ok && m.Flatten == "" {
		switch m.SubFolders {
		case "Legacy":
			return "INBOX/" + legacyToBox(rel), true
		case "Maildir++":
			if strings.HasPrefix(rel, "..") {
				return "INBOX/" + strings.ReplaceAll(rel[2:], ".", "/"), true
			}
			if strings.HasPrefix(rel, ".") && !strings.Contains(rel, "/") {
				return strings.ReplaceAll(rel[1:], ".", "/"), true
			}
			return "", false
		default:
			return "INBOX/" + rel, true
		}
	}
	if m.Path == "" {
		return "", false
	}
	rel, ok := relPath(filepath.Clean(m.Path), dir)
	if !ok {
		return "", false
	}
	switch {
	case m.Flatten != "":
		return strings.ReplaceAll(rel, m.Flatten, "/"), true
	case m.SubFolders == "Legacy":
		return legacyToBox(rel), true
	default:
		return rel, true
	}
}

func legacyToBox(rel string) string {
	parts := strings.Split(rel, "/")
	for i := range parts {
		if i > 0 || strings.HasPrefix(parts[i], ".") {
			parts[i] = strings.TrimPrefix(parts[i], ".")
		}
	}
	return strings.Join(parts, "/")
}

func relPath(base, dir string) (string, bool) {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return rel, true
}

func splitStoreRef(ref string) (store, box string) {
	ref = strings.TrimPrefix(unquote(ref), ":")
	store, box, _ = strings.Cut(ref, ":")
	return store, box
}

// splitArgs splits a config value into whitespace-separated, optionally
// double-quoted arguments.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote, have = !inQuote, true
		case (r == ' ' || r == '\t') && !inQuote:
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}
