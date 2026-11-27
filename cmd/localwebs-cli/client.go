package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const chunkSize = 256 * 1024
const transferLimit int64 = 100_000_000_000
const textLimit int64 = 1024 * 1024

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var roomPattern = regexp.MustCompile(`^[0-9]{6}$`)

type message struct {
	Type string `json:"type"`
	Code string `json:"code,omitempty"`
	ID string `json:"id,omitempty"`
	To string `json:"to,omitempty"`
	From string `json:"from,omitempty"`
	Peers []string `json:"peers,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
	RoomTTLMinutes int `json:"roomTtlMinutes,omitempty"`
}

type signal struct {
	Kind string `json:"kind"`
	ID string `json:"id"`
	Name string `json:"name,omitempty"`
	Size *int64 `json:"size"`
	Content string `json:"content,omitempty"`
}

type event struct {
	kind int
	data []byte
	err error
}

type client struct {
	conn *websocket.Conn
	events chan event
	id, room string
	peers []string
	expires time.Time
	failed error
}

func connect(ctx context.Context, address, room string) (*client, error) {
	if !roomPattern.MatchString(room) {
		return nil, errors.New("room code must contain exactly six digits")
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("server must be an http or https URL without credentials")
	}
	if u.Scheme == "https" { u.Scheme = "wss" } else { u.Scheme = "ws" }
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/ws", "", "", ""
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil && resp.Body != nil { _ = resp.Body.Close() }
		return nil, fmt.Errorf("connect: %w", err)
	}
	c := &client{conn: conn, events: make(chan event, 32), room: room}
	conn.SetReadLimit(chunkSize + 4096)
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			select {
			case c.events <- event{kind: kind, data: data, err: err}:
			case <-ctx.Done(): return
			}
			if err != nil { return }
		}
	}()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	joined := false
	for {
		select {
		case <-ctx.Done(): conn.Close(); return nil, ctx.Err()
		case <-deadline.C: conn.Close(); return nil, errors.New("joining room timed out")
		case e := <-c.events:
			if e.err != nil { conn.Close(); return nil, e.err }
			if e.kind != websocket.TextMessage { conn.Close(); return nil, errors.New("unexpected binary message while joining") }
			var m message
			if err := json.Unmarshal(e.data, &m); err != nil { conn.Close(); return nil, err }
			switch m.Type {
			case "hello":
				if !idPattern.MatchString(m.ID) { conn.Close(); return nil, errors.New("invalid device ID") }
				c.id = m.ID
				if err := conn.WriteJSON(message{Type: "join", Code: room}); err != nil { conn.Close(); return nil, err }
			case "joined":
				if c.id == "" || m.Code != room || m.RoomTTLMinutes < 1 || m.RoomTTLMinutes > 1440 {
					conn.Close(); return nil, errors.New("invalid room session")
				}
				c.expires = time.Now().Add(time.Duration(m.RoomTTLMinutes) * time.Minute)
				joined = true
			case "peers":
				if joined && m.Code == room {
					if err := c.setPeers(m.Peers); err != nil { conn.Close(); return nil, err }
					return c, nil
				}
			case "join-error", "error":
				conn.Close(); return nil, errors.New(m.Error)
			}
		}
	}
}

func (c *client) setPeers(peers []string) error {
	for _, p := range peers {
		if !idPattern.MatchString(p) || p == c.id { return errors.New("invalid peer list") }
	}
	c.peers = append([]string(nil), peers...)
	sort.Strings(c.peers)
	return nil
}

func (c *client) peer(prefix string) (string, error) {
	prefix = strings.ToLower(prefix)
	var matches []string
	for _, peer := range c.peers {
		if strings.HasPrefix(peer, prefix) { matches = append(matches, peer) }
	}
	if prefix == "" || len(matches) != 1 {
		return "", fmt.Errorf("device %q matches %d peers; specify an unambiguous ID", prefix, len(matches))
	}
	return matches[0], nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil { return "", err }
	return hex.EncodeToString(b[:]), nil
}

func (c *client) sendSignal(peer string, s signal) error {
	data, err := json.Marshal(s)
	if err != nil { return err }
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(message{Type: "signal", To: peer, Data: data})
}

func (c *client) next(ctx context.Context, timeout time.Duration) (event, error) {
	wait := time.Until(c.expires)
	if wait <= 0 { return event{}, errors.New("room session expired") }
	if timeout > 0 && timeout < wait { wait = timeout }
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done(): return event{}, ctx.Err()
	case <-timer.C:
		if time.Now().After(c.expires) { return event{}, errors.New("room session expired") }
		return event{}, errors.New("transfer timed out")
	case e := <-c.events:
		if e.err != nil {
			c.failed = e.err
			return event{}, fmt.Errorf("connection lost: %w", e.err)
		}
		return e, nil
	}
}

func (c *client) close() error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.conn.WriteJSON(message{Type: "leave"})
	return c.conn.Close()
}
