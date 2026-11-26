package main

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"
)

type download struct {
	c                     *client
	id, peer, token, name string
	size                  *int64
	chunks                chan []byte
	closed                chan struct{}
	expiry                *time.Timer
}

func (d *download) queue(chunk []byte) {
	select {
	case d.chunks <- chunk:
	default:
		d.c.close()
	}
}

func matchingSignal(data json.RawMessage, kind, id string) bool {
	var s struct{ Kind, ID string }
	return json.Unmarshal(data, &s) == nil && s.Kind == kind && s.ID == id
}

func (h *hub) prepareDownload(c *client, peer, id string) {
	t := c.transfers[id]
	if t.peer != peer || !idPattern.MatchString(peer) || t.sends || t.accepted || t.content != "" || c.download != nil {
		c.json(message{Type: "error", Error: "The download could not be started."})
		return
	}
	d := &download{
		c: c, id: id, peer: peer, token: newID(), name: t.name, size: t.size,
		chunks: make(chan []byte, 1), closed: make(chan struct{}),
	}
	c.download = d
	h.downloads[d.token] = d
	d.expiry = time.AfterFunc(time.Minute, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.downloads[d.token] == d {
			h.downloadSignal(d, "cancel")
			d.c.json(message{Type: "download-failed", ID: d.id})
			h.dropDownload(d)
		}
	})
	c.json(message{Type: "download-ready", ID: id, Token: d.token})
}

func (h *hub) dropDownload(d *download) {
	if d.c.download != d {
		return
	}
	d.c.download = nil
	delete(h.downloads, d.token)
	d.expiry.Stop()
	close(d.closed)
}

func (h *hub) downloadSignal(d *download, kind string) {
	h.relay(d.c, d.peer, json.RawMessage(`{"kind":"`+kind+`","id":"`+d.id+`"}`), nil)
}

func (h *hub) downloadHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.mu.Lock()
	d := h.downloads[r.PathValue("token")]
	if d == nil || d.c.transfers[d.id].peer != d.peer {
		h.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	delete(h.downloads, d.token)
	d.expiry.Stop()
	h.downloadSignal(d, "accept")
	d.c.json(message{Type: "download-started", ID: d.id})
	h.mu.Unlock()

	completed := false
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if d.c.download != d {
			return
		}
		if completed {
			h.downloadSignal(d, "done")
			d.c.json(message{Type: "download-finished", ID: d.id})
		} else {
			h.downloadSignal(d, "cancel")
			d.c.json(message{Type: "download-failed", ID: d.id})
		}
		h.dropDownload(d)
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Accel-Buffering", "no")
	if d.size != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*d.size, 10))
	}
	rc := http.NewResponseController(w)
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case bytes := <-d.chunks:
			if bytes == nil {
				completed = true
				return
			}
			if err := rc.SetWriteDeadline(time.Now().Add(25 * time.Second)); err != nil {
				panic(http.ErrAbortHandler)
			}
			if _, err := w.Write(bytes); err != nil || rc.Flush() != nil {
				panic(http.ErrAbortHandler)
			}
			h.mu.Lock()
			if d.c.download == d {
				h.downloadSignal(d, "ack")
			}
			h.mu.Unlock()
			timer.Reset(30 * time.Second)
		case <-timer.C:
			panic(http.ErrAbortHandler)
		case <-d.closed:
			panic(http.ErrAbortHandler)
		case <-r.Context().Done():
			// A browser may finish a known-length download before the sender's end
			// signal arrives. Wait for that signal after the last byte.
			h.mu.Lock()
			t := d.c.transfers[d.id]
			h.mu.Unlock()
			if d.size == nil || t.received != *d.size || t.awaitingAck {
				panic(http.ErrAbortHandler)
			}
			select {
			case tail := <-d.chunks:
				if tail == nil {
					completed = true
					return
				}
				panic(http.ErrAbortHandler)
			case <-d.closed:
				panic(http.ErrAbortHandler)
			case <-timer.C:
				panic(http.ErrAbortHandler)
			}
		}
	}
}
