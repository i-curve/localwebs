package main

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/gorilla/websocket"
)

const maxLinks = 64
const meshTTL = 64

type meshLink struct {
	socket *socket
	nonce  string
}

type meshState struct {
	Type      string              `json:"type"`
	Node      string              `json:"node"`
	Serial    uint64              `json:"serial"`
	Neighbors []string            `json:"neighbors"`
	Rooms     map[string][]string `json:"rooms"`
}

func (s meshState) valid() bool {
	if !idPattern.MatchString(s.Node) || s.Serial == 0 || len(s.Neighbors) > maxLinks || len(s.Rooms) > maxClients {
		return false
	}
	for _, id := range s.Neighbors {
		if !idPattern.MatchString(id) || id == s.Node {
			return false
		}
	}
	clients := make(map[string]bool)
	for room, peers := range s.Rooms {
		if !codePattern.MatchString(room) || len(peers) == 0 || len(peers) > maxRoom {
			return false
		}
		for _, id := range peers {
			if !idPattern.MatchString(id) || clients[id] {
				return false
			}
			clients[id] = true
		}
	}
	return len(clients) <= maxClients
}

func (h *hub) publish(skip string) {
	h.serial++
	state := meshState{
		Type: "state", Node: h.id, Serial: h.serial, Neighbors: slices.Sorted(maps.Keys(h.links)),
		Rooms: make(map[string][]string, len(h.rooms)),
	}
	for code, clients := range h.rooms {
		state.Rooms[code] = slices.Sorted(maps.Keys(clients))
	}
	h.states[h.id] = state
	h.flood(skip, state)
	h.rebuildRoutes()
}

func (h *hub) flood(skip string, state meshState) {
	for id, link := range h.links {
		if id != skip {
			link.socket.json(state)
		}
	}
}

func (h *hub) rebuildRoutes() {
	next := map[string]string{h.id: h.id}
	queue := []string{h.id}
	for i := 0; i < len(queue); i++ {
		from := queue[i]
		for _, to := range h.states[from].Neighbors {
			if next[to] != "" || !slices.Contains(h.states[to].Neighbors, from) {
				continue
			}
			next[to] = next[from]
			if from == h.id {
				next[to] = to
			}
			queue = append(queue, to)
		}
	}
	delete(next, h.id)
	h.routes = next
	old := h.remote
	h.remote = make(map[string]map[string]string)
	for _, node := range queue[1:] {
		for room, clients := range h.states[node].Rooms {
			if h.remote[room] == nil {
				h.remote[room] = make(map[string]string)
			}
			for _, id := range clients {
				h.remote[room][id] = node
			}
		}
	}
	for room := range h.rooms {
		if !maps.Equal(old[room], h.remote[room]) {
			h.presence(room)
		}
	}
}

func (h *hub) forward(previous, room, from, to string, data json.RawMessage, payload []byte, hops uint8) {
	if hops == 0 {
		return
	}
	next := h.routes[h.remote[room][to]]
	s := h.links[next].socket
	if s == nil || next == previous {
		return
	}
	if payload != nil {
		s.send(frame{websocket.BinaryMessage, append(append([]byte{hops}, room+from+to...), payload...)})
	} else {
		s.json(message{Type: "signal", Code: room, From: from, To: to, Data: data, Hops: hops})
	}
}

func (h *hub) meshRelay(previous, room, from, to string, data json.RawMessage, payload []byte, hops uint8) {
	if hops == 0 || hops > meshTTL || h.remote[room][from] == "" {
		return
	}
	if dest := h.rooms[room][to]; dest != nil {
		h.deliver(dest, from, data, payload)
	} else {
		h.forward(previous, room, from, to, data, payload, hops-1)
	}
}

func (h *hub) meshInbound(remote string, s *socket, f frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.links[remote].socket != s {
		return
	}
	if f.kind == websocket.BinaryMessage {
		if len(f.data) > 103 && idPattern.Match(f.data[71:103]) {
			h.meshRelay(remote, string(f.data[1:7]), string(f.data[7:39]), string(f.data[39:71]), nil, f.data[71:], f.data[0])
		}
		return
	}
	var envelope struct{ Type string }
	if json.Unmarshal(f.data, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "state":
		var state meshState
		if json.Unmarshal(f.data, &state) == nil && state.Node != h.id && state.Serial > h.states[state.Node].Serial && state.valid() {
			h.states[state.Node] = state
			h.flood(remote, state)
			h.rebuildRoutes()
		}
	case "signal":
		var m message
		if json.Unmarshal(f.data, &m) == nil && len(m.Data) != 0 && len(m.Data) <= 2048 {
			h.meshRelay(remote, m.Code, m.From, m.To, m.Data, nil, m.Hops)
		}
	}
}
