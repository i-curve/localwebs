package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type clusterSettings struct {
	enabled bool
	peers   []string
	key     string
}

func clusterConfig(c config) clusterSettings {
	return clusterSettings{c.ClusterEnabled, c.ClusterPeerURLs, c.ClusterKey}
}

func (c clusterSettings) equal(other clusterSettings) bool {
	return c.enabled == other.enabled && c.key == other.key && slices.Equal(c.peers, other.peers)
}

type cluster struct {
	h     *hub
	store *configStore
}

func equalSecret(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func clusterProof(key, nonce, from, to string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("localwebs-mesh-v1:" + nonce + from + to))
	return hex.EncodeToString(mac.Sum(nil))
}

func (c *cluster) accept(w http.ResponseWriter, r *http.Request) {
	cfg := clusterConfig(c.store.get())
	remote, nonce := r.Header.Get("X-Localwebs-Node"), r.Header.Get("X-Localwebs-Link")
	if !cfg.enabled || !idPattern.MatchString(remote) || remote == c.h.id || !idPattern.MatchString(nonce) ||
		!equalSecret(r.Header.Get("Authorization"), "Bearer "+clusterProof(cfg.key, nonce, remote, "")) {
		http.Error(w, "Cluster authorization failed", http.StatusUnauthorized)
		return
	}
	headers := make(http.Header)
	headers.Set("X-Localwebs-Node", c.h.id)
	headers.Set("X-Localwebs-Proof", clusterProof(cfg.key, nonce, c.h.id, remote))
	conn, err := upgrader.Upgrade(w, r, headers)
	if err != nil {
		return
	}
	c.run(conn, cfg, remote, nonce)
}

func (c *cluster) run(conn *websocket.Conn, cfg clusterSettings, remote, nonce string) {
	s := newSocket(conn)
	defer s.close()
	h := c.h
	h.mu.Lock()
	if !clusterConfig(c.store.get()).equal(cfg) {
		h.mu.Unlock()
		return
	}
	if existing, ok := h.links[remote]; ok {
		// Both ends prefer the same link if they dial each other concurrently.
		if existing.nonce <= nonce {
			h.mu.Unlock()
			return
		}
		existing.socket.close()
	} else if len(h.links) >= maxLinks {
		h.mu.Unlock()
		return
	}
	h.links[remote] = meshLink{s, nonce}
	h.publish(remote)
	for id, state := range h.states {
		if id == h.id || h.routes[id] != "" {
			s.initial = append(s.initial, state)
		}
	}
	h.mu.Unlock()
	s.run(maxFrame, func(f frame) { h.meshInbound(remote, s, f) })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.links[remote].socket == s {
		delete(h.links, remote)
		h.publish("")
	}
}

func (c *cluster) reset(old clusterSettings, next clusterSettings) {
	h := c.h
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, link := range h.links {
		link.socket.close()
		delete(h.links, id)
	}
	if !next.enabled || old.key != next.key {
		clear(h.states)
	}
	h.publish("")
}

func (c *cluster) connected() int {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	return len(c.h.links)
}

func (c *cluster) supervise(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var previous clusterSettings
	cancel := func() {}
	defer func() {
		cancel()
		c.reset(previous, clusterSettings{})
	}()
	for {
		cfg := clusterConfig(c.store.get())
		if !cfg.equal(previous) {
			cancel()
			// A connection can arrive before the first tick. It already uses cfg.
			if previous.enabled {
				c.reset(previous, cfg)
			}
			previous = cfg
			cancel = c.startDialers(ctx, cfg)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (c *cluster) startDialers(ctx context.Context, cfg clusterSettings) context.CancelFunc {
	workers, cancel := context.WithCancel(ctx)
	if cfg.enabled {
		for _, peer := range cfg.peers {
			go c.dialLoop(workers, cfg, peer)
		}
	}
	return cancel
}

func (c *cluster) dialLoop(ctx context.Context, cfg clusterSettings, peer string) {
	u, err := url.Parse(peer)
	if err != nil {
		return // Only normalized URLs reach the dialer.
	}
	u.Scheme = "ws" + strings.TrimPrefix(u.Scheme, "http")
	u.Path = "/ws/cluster"
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	var remote string
	for ctx.Err() == nil {
		c.h.mu.Lock()
		active := c.h.links[remote].socket
		c.h.mu.Unlock()
		if active != nil {
			select {
			case <-active.done:
			case <-ctx.Done():
			}
		} else {
			nonce := newID()
			headers := make(http.Header)
			headers.Set("X-Localwebs-Node", c.h.id)
			headers.Set("X-Localwebs-Link", nonce)
			headers.Set("Authorization", "Bearer "+clusterProof(cfg.key, nonce, c.h.id, ""))
			conn, resp, err := dialer.DialContext(ctx, u.String(), headers)
			if err == nil {
				node := resp.Header.Get("X-Localwebs-Node")
				if ctx.Err() == nil && idPattern.MatchString(node) && node != c.h.id &&
					equalSecret(resp.Header.Get("X-Localwebs-Proof"), clusterProof(cfg.key, nonce, node, c.h.id)) {
					remote = node
					c.run(conn, cfg, node, nonce)
				} else {
					_ = conn.Close()
				}
			} else if resp != nil {
				_ = resp.Body.Close()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
