package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const chunkSize = 256 * 1024
const maxFrame = chunkSize + 103
const maxRoom = 32
const maxClients = 1024

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var codePattern = regexp.MustCompile(`^[0-9]{6}$`)
var upgrader = websocket.Upgrader{HandshakeTimeout: 5 * time.Second, WriteBufferSize: 4096}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // Go's crypto/rand terminates the process on entropy failure.
	return hex.EncodeToString(b[:])
}

type frame struct {
	kind int
	data []byte
}

type socket struct {
	conn    *websocket.Conn
	out     chan frame
	initial []meshState
	done    chan struct{}
	once    sync.Once
}

func newSocket(c *websocket.Conn) *socket {
	return &socket{conn: c, out: make(chan frame, 24), done: make(chan struct{})}
}

func (s *socket) close() { s.once.Do(func() { close(s.done); _ = s.conn.Close() }) }

func (s *socket) send(f frame) {
	select {
	case <-s.done:
	case s.out <- f:
	default:
		s.close()
	}
}

func (s *socket) json(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	s.send(frame{websocket.TextMessage, b})
}

func (s *socket) run(maxText int, handle func(frame)) {
	defer s.close()
	go func() {
		defer s.close()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for _, m := range s.initial {
			_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := s.conn.WriteJSON(m); err != nil {
				return
			}
		}
		for {
			select {
			case <-s.done:
				return
			case f := <-s.out:
				_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := s.conn.WriteMessage(f.kind, f.data); err != nil {
					return
				}
			case <-ping.C:
				if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			}
		}
	}()
	s.conn.SetReadLimit(maxFrame)
	_ = s.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	s.conn.SetPongHandler(func(string) error { return s.conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	for {
		kind, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.TextMessage && len(data) > maxText {
			return
		}
		handle(frame{kind, data})
	}
}

type client struct {
	*socket
	id, room  string
	transfers map[string]consent
	download  *download
}

type message struct {
	Type  string          `json:"type"`
	Code  string          `json:"code,omitempty"`
	ID    string          `json:"id,omitempty"`
	To    string          `json:"to,omitempty"`
	From  string          `json:"from,omitempty"`
	Peers []string        `json:"peers,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	Hops  uint8           `json:"hops,omitempty"`
	Token string          `json:"token,omitempty"`
}

type hub struct {
	mu        sync.Mutex
	id        string
	clients   map[string]*client
	rooms     map[string]map[string]*client
	remote    map[string]map[string]string
	links     map[string]meshLink
	states    map[string]meshState
	routes    map[string]string
	serial    uint64
	downloads map[string]*download
}

func newHub() *hub {
	return &hub{
		id: newID(), clients: make(map[string]*client), rooms: make(map[string]map[string]*client),
		remote: make(map[string]map[string]string), links: make(map[string]meshLink),
		states: make(map[string]meshState), routes: make(map[string]string),
		downloads: make(map[string]*download),
	}
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request, allowJoin func() bool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &client{socket: newSocket(conn), id: newID()}
	h.mu.Lock()
	if len(h.clients) >= maxClients {
		h.mu.Unlock()
		_ = conn.WriteJSON(message{Type: "error", Error: "Server is full."})
		c.close()
		return
	}
	h.clients[c.id] = c
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.leave(c)
		delete(h.clients, c.id)
	}()
	c.json(message{Type: "hello", ID: c.id})
	c.run(4096, func(f frame) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if f.kind == websocket.BinaryMessage {
			if len(f.data) > 64 && len(f.data) <= 64+chunkSize && idPattern.Match(f.data[:32]) && idPattern.Match(f.data[32:64]) {
				h.relay(c, string(f.data[:32]), nil, f.data[32:])
			}
			return
		}
		var m message
		if json.Unmarshal(f.data, &m) != nil {
			c.json(message{Type: "error", Error: "Invalid message."})
			return
		}
		switch m.Type {
		case "join":
			switch {
			case !allowJoin():
				c.json(message{Type: "join-error", Error: "Too many attempts. Try again shortly."})
			case !codePattern.MatchString(m.Code):
				c.json(message{Type: "join-error", Error: "Enter exactly six digits."})
			case c.room == m.Code:
				c.json(message{Type: "joined", Code: c.room})
				h.presence(c.room)
			case len(h.rooms[m.Code]) >= maxRoom:
				c.json(message{Type: "join-error", Error: "This room is full."})
			default:
				h.leave(c)
				c.room = m.Code
				if h.rooms[c.room] == nil {
					h.rooms[c.room] = make(map[string]*client)
				}
				h.rooms[c.room][c.id] = c
				c.json(message{Type: "joined", Code: c.room})
				h.refresh(c.room)
			}
		case "leave":
			h.leave(c)
			c.json(message{Type: "left"})
		case "signal":
			if idPattern.MatchString(m.To) && len(m.Data) <= 2048 && len(m.Data) != 0 {
				if d := c.download; d != nil && m.To == d.peer {
					if matchingSignal(m.Data, "cancel", d.id) {
						h.dropDownload(d)
					} else if matchingSignal(m.Data, "accept", d.id) || matchingSignal(m.Data, "ack", d.id) || matchingSignal(m.Data, "done", d.id) {
						return
					}
				}
				h.relay(c, m.To, m.Data, nil)
			}
		case "prepare-download":
			h.prepareDownload(c, m.To, m.ID)
		}
	})
}

func (h *hub) leave(c *client) {
	room := c.room
	if room == "" {
		return
	}
	c.room = ""
	if c.download != nil {
		h.dropDownload(c.download)
	}
	clear(c.transfers)
	delete(h.rooms[room], c.id)
	if len(h.rooms[room]) == 0 {
		delete(h.rooms, room)
	}
	h.refresh(room)
}

func (h *hub) refresh(room string) {
	if len(h.links) != 0 {
		h.publish("")
	}
	h.presence(room)
}

func (h *hub) presence(room string) {
	for id, c := range h.rooms[room] {
		for transfer, state := range c.transfers {
			if h.rooms[room][state.peer] == nil && h.remote[room][state.peer] == "" {
				delete(c.transfers, transfer)
				if c.download != nil && c.download.id == transfer {
					h.dropDownload(c.download)
				}
			}
		}
		peers := make([]string, 0, len(h.rooms[room])+len(h.remote[room]))
		for other := range h.rooms[room] {
			if other != id {
				peers = append(peers, other)
			}
		}
		for other := range h.remote[room] {
			peers = append(peers, other)
		}
		// An explicit empty list distinguishes an empty room from a malformed update.
		c.json(struct {
			Type  string   `json:"type"`
			Code  string   `json:"code"`
			Peers []string `json:"peers"`
		}{"peers", room, peers})
	}
}

func (h *hub) deliver(c *client, from string, data json.RawMessage, payload []byte) {
	if !c.allowed(from, data, payload, false) {
		return
	}
	if d := c.download; d != nil && d.peer == from {
		if payload != nil && string(payload[:32]) == d.id {
			d.queue(payload[32:])
			return
		}
		if matchingSignal(data, "end", d.id) {
			d.queue(nil)
			return
		}
		if matchingSignal(data, "cancel", d.id) {
			h.dropDownload(d)
		}
	}
	if payload != nil {
		c.send(frame{websocket.BinaryMessage, append([]byte(from), payload...)})
	} else {
		c.json(message{Type: "signal", From: from, Data: data})
	}
}

func (h *hub) relay(c *client, to string, data json.RawMessage, payload []byte) {
	if c.room == "" || to == c.id || (h.rooms[c.room][to] == nil && h.remote[c.room][to] == "") ||
		!c.allowed(to, data, payload, true) {
		return
	}
	if dest := h.rooms[c.room][to]; dest != nil {
		h.deliver(dest, c.id, data, payload)
	} else {
		h.forward("", c.room, c.id, to, data, payload, meshTTL)
	}
}
