package main

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
)

const transferLimit int64 = 100_000_000_000
const textLimit int64 = 1024 * 1024

type consent struct {
	peer          string
	name          string
	content       string
	sends         bool
	accepted      bool
	awaitingAck   bool
	ended         bool
	size          *int64
	received, max int64
	last          time.Time
}

func (c *client) allowed(peer string, data json.RawMessage, payload []byte, out bool) bool {
	if payload != nil {
		id := string(payload[:32])
		t := c.transfers[id]
		if t.peer != peer || !t.accepted || t.sends != out || t.awaitingAck || t.ended {
			return false
		}
		count := t.received + int64(len(payload)-32)
		if count > t.max || (t.size != nil && count > *t.size) {
			return false
		}
		t.received, t.awaitingAck, t.last = count, true, time.Now()
		c.transfers[id] = t
		return true
	}
	var m struct {
		Kind, ID, Name, Content string
		Size                    *int64
	}
	if json.Unmarshal(data, &m) != nil || !idPattern.MatchString(m.ID) {
		return false
	}
	t := c.transfers[m.ID]
	if m.Kind == "offer" {
		for id, old := range c.transfers {
			if time.Since(old.last) > 2*time.Minute {
				delete(c.transfers, id)
			}
		}
		limit := transferLimit
		if m.Content == "text" {
			limit = textLimit
			if m.Size == nil || *m.Size == 0 {
				return false
			}
		}
		if (m.Content != "" && m.Content != "text") || m.Name == "" || m.Name == "." || m.Name == ".." ||
			len(m.Name) > 1020 || strings.ContainsAny(m.Name, `/\`) || strings.IndexFunc(m.Name, unicode.IsControl) >= 0 ||
			(m.Size != nil && (*m.Size < 0 || *m.Size > limit)) || len(c.transfers) >= 8 || c.transfers[m.ID].peer != "" {
			return false
		}
		if c.transfers == nil {
			c.transfers = make(map[string]consent)
		}
		c.transfers[m.ID] = consent{peer: peer, name: m.Name, content: m.Content, sends: out, size: m.Size, max: limit, last: time.Now()}
		return true
	}
	if t.peer != peer {
		return false
	}
	sender := t.sends == out
	switch m.Kind {
	case "accept":
		if sender || t.accepted {
			return false
		}
		t.accepted = true
	case "ack":
		if sender || !t.awaitingAck {
			return false
		}
		t.awaitingAck = false
	case "end":
		if !sender || !t.accepted || t.awaitingAck || t.ended || (t.size != nil && t.received != *t.size) {
			return false
		}
		t.ended = true
	case "done":
		if sender || !t.ended {
			return false
		}
		delete(c.transfers, m.ID)
		return true
	case "cancel":
		delete(c.transfers, m.ID)
		return true
	default:
		return false
	}
	t.last = time.Now()
	c.transfers[m.ID] = t
	return true
}
