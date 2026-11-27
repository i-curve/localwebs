package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

func (c *client) handleMessage(e event) (message, signal, error) {
	var m message
	var s signal
	if e.kind != websocket.TextMessage { return m, s, errors.New("unexpected binary frame") }
	if err := json.Unmarshal(e.data, &m); err != nil { return m, s, err }
	switch m.Type {
	case "peers":
		if m.Code == c.room { return m, s, c.setPeers(m.Peers) }
	case "signal":
		if !idPattern.MatchString(m.From) { return m, s, errors.New("invalid sender") }
		if err := json.Unmarshal(m.Data, &s); err != nil || !idPattern.MatchString(s.ID) {
			return m, s, errors.New("invalid transfer signal")
		}
	case "error", "join-error":
		return m, s, errors.New(m.Error)
	}
	return m, s, nil
}

func (c *client) waitSignal(ctx context.Context, peer, id, kind string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 { return errors.New("transfer timed out") }
		e, err := c.next(ctx, left)
		if err != nil { return err }
		m, s, err := c.handleMessage(e)
		if err != nil { return err }
		if m.Type == "peers" {
			found := false
			for _, p := range c.peers { if p == peer { found = true } }
			if !found { return errors.New("peer disconnected") }
		}
		if m.Type != "signal" { continue }
		if s.Kind == "offer" {
			if err := c.sendSignal(m.From, signal{Kind: "cancel", ID: s.ID}); err != nil { return err }
			continue
		}
		if m.From == peer && s.ID == id {
			if s.Kind == "cancel" { return errors.New("peer cancelled the transfer") }
			if s.Kind == kind { return nil }
		}
	}
}

func (c *client) send(ctx context.Context, peer, name, content string, size *int64, src io.Reader) (err error) {
	if !safeName(name) { return errors.New("unsafe file name") }
	if !time.Now().Before(c.expires) { return errors.New("room session expired") }
	id, err := newID()
	if err != nil { return err }
	if err = c.sendSignal(peer, signal{Kind: "offer", ID: id, Name: name, Size: size, Content: content}); err != nil { return err }
	defer func() {
		if err != nil {
			if cancelErr := c.sendSignal(peer, signal{Kind: "cancel", ID: id}); cancelErr != nil {
				err = fmt.Errorf("%w (cancel: %v)", err, cancelErr)
			}
		}
	}()
	fmt.Fprintf(os.Stderr, "Waiting for %s to accept %s...\n", peer[:8], name)
	if err = c.waitSignal(ctx, peer, id, "accept", 60*time.Second); err != nil { return err }
	buf := make([]byte, chunkSize+64)
	copy(buf, peer+id)
	var sent int64
	for {
		n, readErr := src.Read(buf[64:])
		if n > 0 {
			if !time.Now().Before(c.expires) { return errors.New("room session expired") }
			sent += int64(n)
			if sent > transferLimit || (size != nil && sent > *size) { return errors.New("content exceeds the advertised size or transfer limit") }
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err = c.conn.WriteMessage(websocket.BinaryMessage, buf[:64+n]); err != nil { return err }
			if err = c.waitSignal(ctx, peer, id, "ack", 30*time.Second); err != nil { return err }
			fmt.Fprintf(os.Stderr, "\rSent %d bytes", sent)
		}
		if readErr == io.EOF { break }
		if readErr != nil { return readErr }
		if n == 0 { return io.ErrNoProgress }
	}
	if size != nil && sent != *size { return errors.New("content changed while sending") }
	if !time.Now().Before(c.expires) { return errors.New("room session expired") }
	if err = c.sendSignal(peer, signal{Kind: "end", ID: id}); err != nil { return err }
	if err = c.waitSignal(ctx, peer, id, "done", 30*time.Second); err != nil { return err }
	fmt.Fprintf(os.Stderr, "\nSent %s (%d bytes).\n", name, sent)
	return nil
}

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 1020 &&
		!strings.ContainsAny(name, `/\`) && strings.IndexFunc(name, unicode.IsControl) < 0
}

func createDestination(dir, name string) (*os.File, error) {
	if !safeName(name) { return nil, errors.New("unsafe file name") }
	if err := os.MkdirAll(dir, 0700); err != nil { return nil, err }
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 { candidate = fmt.Sprintf("%s (%d)%s", base, i, ext) }
		f, err := os.OpenFile(filepath.Join(dir, candidate), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) { continue }
		return f, err
	}
	return nil, errors.New("too many files with the same name")
}

func (c *client) receive(ctx context.Context, peer string, offer signal, dir string) (text string, err error) {
	if !time.Now().Before(c.expires) { return "", errors.New("room session expired") }
	if !safeName(offer.Name) || (offer.Content != "" && offer.Content != "text") ||
		(offer.Size != nil && (*offer.Size < 0 || *offer.Size > transferLimit)) ||
		(offer.Content == "text" && (offer.Size == nil || *offer.Size == 0 || *offer.Size > textLimit)) {
		if e := c.sendSignal(peer, signal{Kind: "cancel", ID: offer.ID}); e != nil { return "", e }
		return "", errors.New("invalid offer")
	}
	var f *os.File
	var buffer bytes.Buffer
	var dst io.Writer = &buffer
	if offer.Content != "text" {
		f, err = createDestination(dir, offer.Name)
		if err != nil {
			if e := c.sendSignal(peer, signal{Kind: "cancel", ID: offer.ID}); e != nil { return "", fmt.Errorf("%w (cancel: %v)", err, e) }
			return "", err
		}
		dst = f
		defer func() {
			if f != nil {
				if e := f.Close(); err == nil && e != nil { err = e }
				if err != nil {
					if e := os.Remove(f.Name()); e != nil { err = fmt.Errorf("%w (remove partial file: %v)", err, e) }
				}
			}
		}()
	}
	defer func() {
		if err != nil {
			if e := c.sendSignal(peer, signal{Kind: "cancel", ID: offer.ID}); e != nil { err = fmt.Errorf("%w (cancel: %v)", err, e) }
		}
	}()
	if err = c.sendSignal(peer, signal{Kind: "accept", ID: offer.ID}); err != nil { return "", err }
	var received int64
	for {
		var e event
		e, err = c.next(ctx, 30*time.Second)
		if err != nil { return "", err }
		if e.kind == websocket.BinaryMessage {
			if len(e.data) <= 64 || len(e.data) > 64+chunkSize || string(e.data[:64]) != peer+offer.ID {
				return "", errors.New("unexpected binary frame")
			}
			received += int64(len(e.data)-64)
			if received > transferLimit || (offer.Size != nil && received > *offer.Size) { return "", errors.New("received content exceeds advertised size") }
			if _, err = dst.Write(e.data[64:]); err != nil { return "", err }
			if err = c.sendSignal(peer, signal{Kind: "ack", ID: offer.ID}); err != nil { return "", err }
			fmt.Fprintf(os.Stderr, "\rReceived %d bytes", received)
			continue
		}
		var m message
		var s signal
		m, s, err = c.handleMessage(e)
		if err != nil { return "", err }
		if m.Type == "peers" {
			found := false
			for _, p := range c.peers { if p == peer { found = true } }
			if !found { return "", errors.New("peer disconnected") }
		}
		if m.Type != "signal" { continue }
		if s.Kind == "offer" {
			if err = c.sendSignal(m.From, signal{Kind: "cancel", ID: s.ID}); err != nil { return "", err }
		} else if m.From == peer && s.ID == offer.ID && s.Kind == "cancel" {
			return "", errors.New("sender cancelled the transfer")
		} else if m.From == peer && s.ID == offer.ID && s.Kind == "end" {
			if offer.Size != nil && received != *offer.Size { return "", errors.New("transfer ended before all bytes arrived") }
			if f != nil {
				if err = f.Close(); err != nil { return "", err }
				path := f.Name()
				f = nil
				fmt.Fprintf(os.Stderr, "\nSaved %s (%d bytes).\n", path, received)
			} else {
				text = buffer.String()
				fmt.Fprintf(os.Stdout, "\n%s\n", text)
			}
			if err = c.sendSignal(peer, signal{Kind: "done", ID: offer.ID}); err != nil { return "", err }
			return text, nil
		}
	}
}

func source(paths []string) (name string, size *int64, reader io.ReadCloser, err error) {
	if len(paths) == 0 { return "", nil, nil, errors.New("select at least one path") }
	info, err := os.Lstat(paths[0])
	if err != nil { return "", nil, nil, err }
	if len(paths) == 1 && info.Mode().IsRegular() {
		if info.Size() > transferLimit { return "", nil, nil, errors.New("file exceeds 100 GB") }
		f, e := os.Open(paths[0])
		if e != nil { return "", nil, nil, e }
		n := info.Size()
		return info.Name(), &n, f, nil
	}
	type entry struct { path, name string }
	var entries []entry
	var total int64
	seen := map[string]bool{}
	for _, root := range paths {
		root, err = filepath.Abs(root)
		if err != nil { return "", nil, nil, err }
		base := filepath.Base(root)
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil { return walkErr }
			if d.IsDir() { return nil }
			info, e := d.Info()
			if e != nil { return e }
			if !info.Mode().IsRegular() { return fmt.Errorf("unsupported file type: %s", path) }
			total += info.Size()
			if total > transferLimit { return errors.New("selection exceeds 100 GB") }
			rel, e := filepath.Rel(filepath.Dir(root), path)
			if e != nil { return e }
			archiveName := filepath.ToSlash(rel)
			if seen[archiveName] { return fmt.Errorf("duplicate archive path: %s", archiveName) }
			seen[archiveName] = true
			entries = append(entries, entry{path, archiveName})
			return nil
		})
		if err != nil { return "", nil, nil, err }
		if len(paths) == 1 { name = base + ".zip" }
	}
	if name == "" { name = "localwebs-files.zip" }
	r, w := io.Pipe()
	go func() {
		z := zip.NewWriter(w)
		var writeErr error
		for _, entry := range entries {
			var f *os.File
			f, writeErr = os.Open(entry.path)
			if writeErr != nil { break }
			var out io.Writer
			out, writeErr = z.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Store})
			if writeErr == nil { _, writeErr = io.Copy(out, f) }
			if e := f.Close(); writeErr == nil { writeErr = e }
			if writeErr != nil { break }
		}
		if e := z.Close(); writeErr == nil { writeErr = e }
		_ = w.CloseWithError(writeErr)
	}()
	return name, nil, r, nil
}
