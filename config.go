package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

//go:embed config.system.json
var systemDefaults []byte

type config struct {
	Username        string   `json:"username"`
	Password        string   `json:"password"`
	ClusterEnabled  bool     `json:"clusterEnabled"`
	ClusterPeerURLs []string `json:"clusterPeerUrls"`
	ClusterKey      string   `json:"clusterKey"`
}

type configPatch struct {
	Username        *string   `json:"username,omitempty"`
	Password        *string   `json:"password,omitempty"`
	ClusterEnabled  *bool     `json:"clusterEnabled,omitempty"`
	ClusterPeerURLs *[]string `json:"clusterPeerUrls,omitempty"`
	ClusterKey      *string   `json:"clusterKey,omitempty"`
	LegacyUsername  *string   `json:"adminUsername,omitempty"`
	LegacyPassword  *string   `json:"adminPassword,omitempty"`
	LegacyPeerURL   *string   `json:"clusterPeerUrl,omitempty"`
}

func (c config) equal(other config) bool {
	return c.Username == other.Username && c.Password == other.Password &&
		c.ClusterEnabled == other.ClusterEnabled && c.ClusterKey == other.ClusterKey &&
		slices.Equal(c.ClusterPeerURLs, other.ClusterPeerURLs)
}

func (p configPatch) apply(c config) config {
	if p.Username != nil {
		c.Username = *p.Username
	}
	if p.Password != nil {
		c.Password = *p.Password
	}
	if p.ClusterEnabled != nil {
		c.ClusterEnabled = *p.ClusterEnabled
	}
	if p.ClusterPeerURLs != nil {
		c.ClusterPeerURLs = *p.ClusterPeerURLs
	}
	if p.ClusterKey != nil {
		c.ClusterKey = *p.ClusterKey
	}
	return c
}

func (p configPatch) merge(next configPatch) configPatch {
	if next.Username != nil {
		p.Username = next.Username
	}
	if next.Password != nil {
		p.Password = next.Password
	}
	if next.ClusterEnabled != nil {
		p.ClusterEnabled = next.ClusterEnabled
	}
	if next.ClusterPeerURLs != nil {
		p.ClusterPeerURLs = next.ClusterPeerURLs
	}
	if next.ClusterKey != nil {
		p.ClusterKey = next.ClusterKey
	}
	return p
}

func decode(r io.Reader, dst any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}

func decodePatch(r io.Reader) (configPatch, error) {
	var p *configPatch
	if err := decode(r, &p); err != nil {
		return configPatch{}, err
	}
	if p == nil {
		return configPatch{}, errors.New("expected a configuration object")
	}
	if p.Username == nil {
		p.Username = p.LegacyUsername
	}
	if p.Password == nil {
		p.Password = p.LegacyPassword
	}
	if p.ClusterPeerURLs == nil && p.LegacyPeerURL != nil {
		urls := splitURLs(*p.LegacyPeerURL)
		p.ClusterPeerURLs = &urls
	}
	p.LegacyUsername, p.LegacyPassword, p.LegacyPeerURL = nil, nil, nil
	return *p, nil
}

const maxPeerURLs = 32

func splitURLs(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
}

func normalizeURLs(urls []string) ([]string, error) {
	if len(urls) > maxPeerURLs {
		return nil, errors.New("enter no more than 32 peer URLs")
	}
	out := make([]string, 0, len(urls))
	seen := make(map[string]bool)
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 2048 || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Hostname() == "" || u.User != nil || (u.EscapedPath() != "/" && u.EscapedPath() != "") ||
			strings.ContainsAny(raw, "?#") {
			return nil, errors.New("peer URLs must be HTTP(S) origins without credentials or paths")
		}
		origin := u.Scheme + "://" + u.Host
		if !seen[origin] {
			out = append(out, origin)
			seen[origin] = true
		}
	}
	return out, nil
}

func validate(c config) error {
	if strings.TrimSpace(c.Username) == "" || len(c.Username) > 128 || len(c.Password) > 1024 {
		return errors.New("invalid administrator credentials")
	}
	if len(c.ClusterKey) > 256 || strings.IndexFunc(c.ClusterKey, func(r rune) bool { return r < '!' || r > '~' }) >= 0 ||
		(c.ClusterEnabled && len(c.ClusterKey) < 24) {
		return errors.New("an enabled cluster needs a shared key of 24–256 printable ASCII characters without spaces")
	}
	return nil
}

type configStore struct {
	mu            sync.RWMutex
	path          string
	base, current config
	overrides     configPatch
}

func loadConfig(path string, env func(string) (string, bool)) (*configStore, error) {
	var base config
	if err := decode(bytes.NewReader(systemDefaults), &base); err != nil {
		return nil, fmt.Errorf("system config: %w", err)
	}
	for _, setting := range []struct {
		name, legacy string
		target       *string
	}{
		{"LOCALWEBS_USERNAME", "LOCALWEBS_ADMIN_USERNAME", &base.Username},
		{"LOCALWEBS_PASSWORD", "LOCALWEBS_ADMIN_PASSWORD", &base.Password},
		{"LOCALWEBS_CLUSTER_KEY", "", &base.ClusterKey},
	} {
		if value, ok := env(setting.name); ok {
			*setting.target = value
		} else if setting.legacy != "" {
			if value, ok := env(setting.legacy); ok {
				*setting.target = value
			}
		}
	}
	if value, ok := env("LOCALWEBS_CLUSTER_PEER_URLS"); ok {
		base.ClusterPeerURLs = splitURLs(value)
	} else if value, ok := env("LOCALWEBS_CLUSTER_PEER_URL"); ok {
		base.ClusterPeerURLs = splitURLs(value)
	}
	if value, ok := env("LOCALWEBS_CLUSTER_ENABLED"); ok {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("LOCALWEBS_CLUSTER_ENABLED: %w", err)
		}
		base.ClusterEnabled = enabled
	}
	s := &configStore{path: path, base: base}
	content, err := os.ReadFile(path)
	if err == nil {
		s.overrides, err = decodePatch(bytes.NewReader(content))
		if err != nil {
			return nil, fmt.Errorf("config file: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.current = s.overrides.apply(base)
	s.current.ClusterPeerURLs, err = normalizeURLs(s.current.ClusterPeerURLs)
	if err != nil {
		return nil, err
	}
	return s, validate(s.current)
}

func (s *configStore) get() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *configStore) update(expected config, edits configPatch) (config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.current.equal(expected) {
		return config{}, errors.New("settings changed; reload and try again")
	}
	if edits.Password != nil && len(*edits.Password) < 12 {
		return config{}, errors.New("new administrator password must be at least 12 bytes")
	}
	if edits.ClusterPeerURLs != nil {
		normalized, err := normalizeURLs(*edits.ClusterPeerURLs)
		if err != nil {
			return config{}, err
		}
		edits.ClusterPeerURLs = &normalized
	}
	p := s.overrides.merge(edits)
	next := p.apply(s.base)
	var err error
	next.ClusterPeerURLs, err = normalizeURLs(next.ClusterPeerURLs)
	if err != nil {
		return config{}, err
	}
	if err := validate(next); err != nil {
		return config{}, err
	}
	if err := s.write(p); err != nil {
		return config{}, fmt.Errorf("save config: %w", err)
	}
	s.overrides, s.current = p, next
	return next, nil
}

func (s *configStore) write(p configPatch) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".localwebs-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	e := json.NewEncoder(f)
	e.SetIndent("", "  ")
	err = e.Encode(p)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
