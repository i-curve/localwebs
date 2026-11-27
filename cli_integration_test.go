package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCLIInteroperability(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "localwebs-cli")
	if runtime.GOOS == "windows" { binary += ".exe" }
	build := exec.Command("go", "build", "-o", binary, "./cmd/localwebs-cli")
	if out, err := build.CombinedOutput(); err != nil { t.Fatalf("build CLI: %v\n%s", err, out) }
	_, _, server := testNode(t, filepath.Join(t.TempDir(), "config.json"), nil)
	t.Run("CLI sends file", func(t *testing.T) {
		ws, receiver := dialClient(t, server, "123456")
		file := filepath.Join(t.TempDir(), "example.bin")
		content := bytes.Repeat([]byte("abcd"), 80000)
		if err := os.WriteFile(file, content, 0600); err != nil { t.Fatal(err) }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "send", "-server", server, "-code", "123456", "-to", receiver, file)
		type result struct { out []byte; err error }
		finished := make(chan result, 1)
		go func() { out, err := cmd.CombinedOutput(); finished <- result{out, err} }()
		offer := nextMessage(t, ws, func(m message) bool {
			if m.Type != "signal" { return false }
			var data struct { Kind string }
			return json.Unmarshal(m.Data, &data) == nil && data.Kind == "offer"
		})
		var details struct { ID, Name string; Size *int64 }
		if err := json.Unmarshal(offer.Data, &details); err != nil { t.Fatal(err) }
		if details.Name != "example.bin" || details.Size == nil || *details.Size != int64(len(content)) { t.Fatalf("invalid offer: %+v", details) }
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"accept","id":%q}`, details.ID))
		var got []byte
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil { t.Fatal(err) }
			if kind == websocket.BinaryMessage {
				if len(data) <= 64 || string(data[:64]) != offer.From+details.ID { t.Fatal("invalid binary frame") }
				got = append(got, data[64:]...)
				sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"ack","id":%q}`, details.ID))
			} else {
				var m message
				if err := json.Unmarshal(data, &m); err != nil { t.Fatal(err) }
				if isCLISignal(m, "end", details.ID) { break }
			}
		}
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"done","id":%q}`, details.ID))
		if !bytes.Equal(got, content) { t.Fatal("sent file differs") }
		select {
		case result := <-finished:
			if result.err != nil { t.Fatalf("CLI failed: %v\n%s", result.err, result.out) }
		case <-ctx.Done(): t.Fatal("CLI did not exit")
		}
	})
	t.Run("CLI sends streamed archive", func(t *testing.T) {
		ws, receiver := dialClient(t, server, "345678")
		dir := filepath.Join(t.TempDir(), "folder")
		if err := os.Mkdir(dir, 0700); err != nil { t.Fatal(err) }
		if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("archive content"), 0600); err != nil { t.Fatal(err) }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "send", "-server", server, "-code", "345678", "-to", receiver, dir)
		type result struct { out []byte; err error }
		finished := make(chan result, 1)
		go func() { out, err := cmd.CombinedOutput(); finished <- result{out, err} }()
		offer := nextMessage(t, ws, func(m message) bool {
			if m.Type != "signal" { return false }
			var data struct { Kind string }
			return json.Unmarshal(m.Data, &data) == nil && data.Kind == "offer"
		})
		var details map[string]any
		if err := json.Unmarshal(offer.Data, &details); err != nil { t.Fatal(err) }
		if size, ok := details["size"]; !ok || size != nil || details["name"] != "folder.zip" {
			t.Fatalf("invalid archive offer: %v", details)
		}
		id, ok := details["id"].(string)
		if !ok { t.Fatal("missing transfer ID") }
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"accept","id":%q}`, id))
		var archive []byte
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil { t.Fatal(err) }
			if kind == websocket.BinaryMessage {
				if len(data) <= 64 || string(data[:64]) != offer.From+id { t.Fatal("invalid binary frame") }
				archive = append(archive, data[64:]...)
				sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"ack","id":%q}`, id))
			} else {
				var m message
				if err := json.Unmarshal(data, &m); err != nil { t.Fatal(err) }
				if isCLISignal(m, "end", id) { break }
			}
		}
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"done","id":%q}`, id))
		select {
		case result := <-finished:
			if result.err != nil { t.Fatalf("CLI failed: %v\n%s", result.err, result.out) }
		case <-ctx.Done(): t.Fatal("CLI did not exit")
		}
		z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil || len(z.File) != 1 || z.File[0].Name != "folder/test.txt" { t.Fatalf("invalid ZIP: %v", err) }
		r, err := z.File[0].Open()
		if err != nil { t.Fatal(err) }
		defer r.Close()
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "archive content" { t.Fatalf("invalid ZIP contents: %q, %v", got, err) }
	})
	t.Run("CLI receives file", func(t *testing.T) {
		ws, _ := dialClient(t, server, "234567")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "example.bin"), []byte("existing"), 0600); err != nil { t.Fatal(err) }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "receive", "-server", server, "-code", "234567", "-dir", dir)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil { t.Fatal(err) }
		waited := false
		defer func() { cancel(); if !waited { _ = cmd.Wait() } }()
		peers := nextMessage(t, ws, func(m message) bool { return m.Type == "peers" && len(m.Peers) == 1 })
		recipient := peers.Peers[0]
		id := newID()
		data := bytes.Repeat([]byte("abcd"), 80000)
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"offer","id":%q,"name":"example.bin","size":%d}`, id, len(data)))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "accept", id) })
		for offset := 0; offset < len(data); offset += chunkSize {
			end := min(offset+chunkSize, len(data))
			if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte(recipient+id), data[offset:end]...)); err != nil { t.Fatal(err) }
			nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "ack", id) })
		}
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"end","id":%q}`, id))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "done", id) })
		got, err := os.ReadFile(filepath.Join(dir, "example (1).bin"))
		if err != nil || !bytes.Equal(got, data) { t.Fatalf("received file: %v", err) }
		existing, err := os.ReadFile(filepath.Join(dir, "example.bin"))
		if err != nil || string(existing) != "existing" { t.Fatalf("existing file overwritten: %v", err) }
		textID := newID()
		text := []byte("clipboard text\n")
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"offer","id":%q,"name":"clipboard.txt","content":"text","size":%d}`, textID, len(text)))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "accept", textID) })
		if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte(recipient+textID), text...)); err != nil { t.Fatal(err) }
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "ack", textID) })
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"end","id":%q}`, textID))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "done", textID) })
		cancelID := newID()
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"offer","id":%q,"name":"partial.bin","size":10}`, cancelID))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "accept", cancelID) })
		if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte(recipient+cancelID), []byte("part")...)); err != nil { t.Fatal(err) }
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "ack", cancelID) })
		sendSignal(t, ws, recipient, fmt.Sprintf(`{"kind":"cancel","id":%q}`, cancelID))
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := os.Stat(filepath.Join(dir, "partial.bin"))
			if os.IsNotExist(err) { break }
			if time.Now().After(deadline) { t.Fatalf("partial file not removed: %v", err) }
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		_ = cmd.Wait()
		waited = true
		if !bytes.Contains(output.Bytes(), text) { t.Fatalf("received text not displayed: %s", output.String()) }
	})
	t.Run("CLI sends across cluster", func(t *testing.T) {
		const key = "a-private-shared-key-for-tests"
		_, _, nodeB := testNode(t, filepath.Join(t.TempDir(), "config.json"), map[string]string{
			"LOCALWEBS_CLUSTER_ENABLED": "true", "LOCALWEBS_CLUSTER_KEY": key,
		})
		_, _, nodeA := testNode(t, filepath.Join(t.TempDir(), "config.json"), map[string]string{
			"LOCALWEBS_CLUSTER_ENABLED": "true", "LOCALWEBS_CLUSTER_KEY": key, "LOCALWEBS_CLUSTER_PEER_URLS": nodeB,
		})
		ws, receiver := dialClient(t, nodeB, "456789")
		probe, _ := dialClient(t, nodeA, "456789")
		nextMessage(t, probe, func(m message) bool {
			if m.Type != "peers" { return false }
			for _, peer := range m.Peers { if peer == receiver { return true } }
			return false
		})
		if err := probe.Close(); err != nil { t.Fatal(err) }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "send", "-server", nodeA, "-code", "456789", "-to", receiver, "-text", "cluster text")
		type result struct { out []byte; err error }
		finished := make(chan result, 1)
		go func() { out, err := cmd.CombinedOutput(); finished <- result{out, err} }()
		offer := nextMessage(t, ws, func(m message) bool {
			if m.Type != "signal" { return false }
			var data struct { Kind string }
			return json.Unmarshal(m.Data, &data) == nil && data.Kind == "offer"
		})
		var details struct { ID, Content string; Size *int64 }
		if err := json.Unmarshal(offer.Data, &details); err != nil { t.Fatal(err) }
		if details.Content != "text" || details.Size == nil || *details.Size != int64(len("cluster text")) { t.Fatalf("invalid text offer: %+v", details) }
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"accept","id":%q}`, details.ID))
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil { t.Fatal(err) }
			if kind != websocket.BinaryMessage { continue }
			if string(data) != offer.From+details.ID+"cluster text" { t.Fatal("invalid text frame") }
			break
		}
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"ack","id":%q}`, details.ID))
		nextMessage(t, ws, func(m message) bool { return isCLISignal(m, "end", details.ID) })
		sendSignal(t, ws, offer.From, fmt.Sprintf(`{"kind":"done","id":%q}`, details.ID))
		select {
		case result := <-finished:
			if result.err != nil { t.Fatalf("CLI failed: %v\n%s", result.err, result.out) }
		case <-ctx.Done(): t.Fatal("CLI did not exit")
		}
	})
}

func isCLISignal(m message, kind, id string) bool {
	if m.Type != "signal" { return false }
	var data struct { Kind, ID string }
	return json.Unmarshal(m.Data, &data) == nil && data.Kind == kind && data.ID == id
}
