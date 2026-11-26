package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func testNode(t *testing.T, path string, env map[string]string) (*configStore, *cluster, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store, err := loadConfig(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	cl := &cluster{h: h, store: store}
	s := httptest.NewServer(router(h, cl, t.TempDir()))
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { defer close(finished); cl.supervise(ctx) }()
	t.Cleanup(func() { cancel(); <-finished })
	return store, cl, s.URL
}

func dialClient(t *testing.T, endpoint, room string) (*websocket.Conn, string) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(endpoint, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	hello := nextMessage(t, ws, func(m message) bool { return m.Type == "hello" })
	if !idPattern.MatchString(hello.ID) {
		t.Fatalf("invalid client ID %q", hello.ID)
	}
	if err := ws.WriteJSON(message{Type: "join", Code: room}); err != nil {
		t.Fatal(err)
	}
	nextMessage(t, ws, func(m message) bool { return m.Type == "joined" && m.Code == room })
	return ws, hello.ID
}

func nextMessage(t *testing.T, ws *websocket.Conn, match func(message) bool) message {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m message
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		if match(m) {
			return m
		}
	}
}

func withPeer(peer string) func(message) bool {
	return func(m message) bool {
		return m.Type == "peers" && len(m.Peers) == 1 && m.Peers[0] == peer
	}
}

func checkRelay(t *testing.T, src, dst *websocket.Conn, from, to string) {
	t.Helper()
	id := newID()
	signal, err := json.Marshal(map[string]any{"kind": "offer", "id": id, "name": "photo.bin", "size": chunkSize})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.WriteJSON(message{Type: "signal", To: to, Data: signal}); err != nil {
		t.Fatal(err)
	}
	m := nextMessage(t, dst, func(m message) bool { return m.Type == "signal" })
	if m.From != from || !bytes.Equal(m.Data, signal) {
		t.Fatalf("incorrect signal: %+v", m)
	}
	payload := bytes.Repeat([]byte{0, 0xff, 42, 0x7f}, chunkSize/4)
	wire := append([]byte(to+id), payload...)
	// A later probe over the same connection proves data was not sent ahead of consent.
	if err := src.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		t.Fatal(err)
	}
	sendSignal(t, src, to, fmt.Sprintf(`{"kind":"offer","id":%q,"name":"probe","size":0}`, newID()))
	nextMessage(t, dst, func(m message) bool { return m.Type == "signal" })
	sendSignal(t, dst, from, fmt.Sprintf(`{"kind":"accept","id":%q}`, id))
	nextMessage(t, src, func(m message) bool { return m.Type == "signal" })
	if err := src.WriteMessage(websocket.BinaryMessage, wire); err != nil {
		t.Fatal(err)
	}
	kind, data, err := dst.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(data, append([]byte(from+id), payload...)) {
		t.Fatalf("incorrect binary frame: kind=%d, size=%d, err=%v", kind, len(data), err)
	}
}

func sendSignal(t *testing.T, ws *websocket.Conn, to, data string) {
	t.Helper()
	if err := ws.WriteJSON(message{Type: "signal", To: to, Data: json.RawMessage(data)}); err != nil {
		t.Fatal(err)
	}
}

func TestRoomIsolationAndTransfer(t *testing.T) {
	_, _, server := testNode(t, filepath.Join(t.TempDir(), "config.json"), nil)
	a, aid := dialClient(t, server, "001234")
	b, bid := dialClient(t, server, "001234")
	other, oid := dialClient(t, server, "001235")
	nextMessage(t, a, withPeer(bid))
	nextMessage(t, b, withPeer(aid))
	roster := nextMessage(t, other, func(m message) bool { return m.Type == "peers" })
	if len(roster.Peers) != 0 {
		t.Fatalf("different room saw peers: %v", roster.Peers)
	}
	checkRelay(t, a, b, aid, bid)
	if err := a.WriteJSON(message{Type: "signal", To: oid, Data: json.RawMessage(fmt.Sprintf(`{"kind":"offer","id":%q,"name":"test","size":1}`, newID()))}); err != nil {
		t.Fatal(err)
	}
	_ = other.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := other.ReadMessage(); err == nil {
		t.Fatal("different room received a signal")
	}
}

func TestMeshIndirectRoutingAndTopologyChanges(t *testing.T) {
	const key = "a-private-shared-key-for-tests"
	const wrongKey = "another-different-key-for-tests"
	astore, ca, serverA := testNode(t, filepath.Join(t.TempDir(), "config.json"), map[string]string{
		"LOCALWEBS_CLUSTER_ENABLED": "true", "LOCALWEBS_CLUSTER_KEY": key,
	})
	_, cc, serverC := testNode(t, filepath.Join(t.TempDir(), "config.json"), map[string]string{
		"LOCALWEBS_CLUSTER_ENABLED": "true", "LOCALWEBS_CLUSTER_KEY": key,
	})
	bstore, cb, serverB := testNode(t, filepath.Join(t.TempDir(), "config.json"), map[string]string{
		"LOCALWEBS_CLUSTER_ENABLED": "true", "LOCALWEBS_CLUSTER_KEY": wrongKey,
		"LOCALWEBS_CLUSTER_PEER_URLS": serverA + "," + serverC,
	})
	a, aid := dialClient(t, serverA, "123456")
	c, cid := dialClient(t, serverC, "123456")
	nonce := newID()
	headers := http.Header{
		"Authorization":    {"Bearer " + clusterProof(wrongKey, nonce, cb.h.id, "")},
		"X-Localwebs-Node": {cb.h.id}, "X-Localwebs-Link": {nonce},
	}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverA, "http")+"/ws/cluster", headers)
	if conn != nil {
		conn.Close()
	}
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized || ca.connected() != 0 || cb.connected() != 0 {
		t.Fatal("mismatched key was not rejected")
	}
	matching := key
	if _, err := bstore.update(bstore.get(), configPatch{ClusterKey: &matching}); err != nil {
		t.Fatal(err)
	}
	nextMessage(t, a, withPeer(cid))
	nextMessage(t, c, withPeer(aid))
	if ca.connected() != 1 || cb.connected() != 2 || cc.connected() != 1 {
		t.Fatal("expected an A–B–C chain")
	}
	checkRelay(t, c, a, cid, aid)
	checkRelay(t, a, c, aid, cid)
	checkDownload(t, serverC, a, c, aid, cid, 3, true)
	disabled := false
	if _, err := bstore.update(bstore.get(), configPatch{ClusterEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	nextMessage(t, a, func(m message) bool { return m.Type == "peers" && len(m.Peers) == 0 })
	enabled := true
	if _, err := bstore.update(bstore.get(), configPatch{ClusterEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	nextMessage(t, a, withPeer(cid))
	nextMessage(t, c, withPeer(aid))
	d, did := dialClient(t, serverB, "654321")
	e, eid := dialClient(t, serverC, "654321")
	nextMessage(t, d, withPeer(eid))
	nextMessage(t, e, withPeer(did))
	checkRelay(t, e, d, eid, did)
	// Complete the cycle, then remove the bridge. A and C should still be able
	// to route traffic, and a flooded announcement must not cause duplicates.
	both := []string{serverB, serverC}
	if _, err := astore.update(astore.get(), configPatch{ClusterPeerURLs: &both}); err != nil {
		t.Fatal(err)
	}
	awaitRoute(t, ca, cc, 2)
	awaitRoute(t, cc, ca, 2)
	checkDownload(t, serverA, c, a, cid, aid, 1, false)
	if _, err := bstore.update(bstore.get(), configPatch{ClusterEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	awaitRoute(t, ca, cc, 1)
	awaitRoute(t, cc, ca, 1)
	checkDownload(t, serverC, a, c, aid, cid, 1, false)
}

func awaitRoute(t *testing.T, node, peer *cluster, count int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		node.h.mu.Lock()
		ready := len(node.h.links) == count && node.h.routes[peer.h.id] == peer.h.id
		node.h.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("route did not converge with %d direct links", count)
}

func checkDownload(t *testing.T, server string, sender, receiver *websocket.Conn, from, to string, count int, unknown bool) {
	t.Helper()
	id := newID()
	size := fmt.Sprint(count * chunkSize)
	if unknown {
		size = "null"
	}
	sendSignal(t, sender, to, fmt.Sprintf(`{"kind":"offer","id":%q,"name":"data.bin","size":%s}`, id, size))
	nextMessage(t, receiver, func(m message) bool { return signalMatches(m, "offer", id) })
	if err := receiver.WriteJSON(message{Type: "prepare-download", To: from, ID: id}); err != nil {
		t.Fatal(err)
	}
	ready := nextMessage(t, receiver, func(m message) bool { return m.Type == "download-ready" && m.ID == id })
	if res, err := http.Head(server + "/download/" + ready.Token); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("HEAD must not consume interactive download URL: %s", res.Status)
		}
	}
	result := make(chan error, 1)
	chunk := bytes.Repeat([]byte{0x2a, 0xf0}, chunkSize/2)
	go func() {
		res, err := http.Get(server + "/download/" + ready.Token)
		if err != nil {
			result <- err
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Disposition") != "attachment; filename=data.bin" ||
			(!unknown && res.ContentLength != int64(count*chunkSize)) {
			result <- fmt.Errorf("invalid download response: %s %v", res.Status, res.Header)
			return
		}
		got := make([]byte, chunkSize)
		for i := 0; i < count; i++ {
			if _, err := io.ReadFull(res.Body, got); err != nil || !bytes.Equal(got, chunk) {
				result <- fmt.Errorf("download mismatch at chunk %d: %v", i, err)
				return
			}
		}
		if _, err := res.Body.Read(got); err != io.EOF {
			result <- fmt.Errorf("expected EOF: %v", err)
			return
		}
		result <- nil
	}()
	nextMessage(t, sender, func(m message) bool { return signalMatches(m, "accept", id) })
	packet := append([]byte(to+id), chunk...)
	for i := 0; i < count; i++ {
		if err := sender.WriteMessage(websocket.BinaryMessage, packet); err != nil {
			t.Fatal(err)
		}
		nextMessage(t, sender, func(m message) bool { return signalMatches(m, "ack", id) })
	}
	sendSignal(t, sender, to, fmt.Sprintf(`{"kind":"end","id":%q}`, id))
	nextMessage(t, sender, func(m message) bool { return signalMatches(m, "done", id) })
	nextMessage(t, receiver, func(m message) bool { return m.Type == "download-finished" && m.ID == id })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func signalMatches(m message, kind, id string) bool {
	return m.Type == "signal" && matchingSignal(m.Data, kind, id)
}

func TestNativeDownloadBeyondPreviousLimit(t *testing.T) {
	_, _, server := testNode(t, filepath.Join(t.TempDir(), "config.json"), nil)
	a, aid := dialClient(t, server, "123456")
	b, bid := dialClient(t, server, "123456")
	nextMessage(t, a, withPeer(bid))
	nextMessage(t, b, withPeer(aid))
	checkDownload(t, server, a, b, aid, bid, (512*1024*1024)/chunkSize+1, false)
}

func TestTransferOfferSizeBoundaries(t *testing.T) {
	peer := newID()
	for _, tt := range []struct {
		content string
		size    int64
		want    bool
	}{
		{"", 100_000_000_000, true},
		{"", 100_000_000_001, false},
		{"text", 1024 * 1024, true},
		{"text", 1024*1024 + 1, false},
	} {
		c := new(client)
		data := fmt.Sprintf(`{"kind":"offer","id":%q,"name":"content","content":%q,"size":%d}`, newID(), tt.content, tt.size)
		if got := c.allowed(peer, json.RawMessage(data), nil, true); got != tt.want {
			t.Errorf("content=%q size=%d: got %t, want %t", tt.content, tt.size, got, tt.want)
		}
	}
}

func TestAdminPersistsOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "config.json")
	env := map[string]string{"LOCALWEBS_USERNAME": "bootstrap", "LOCALWEBS_PASSWORD": "initial-private-password"}
	_, _, server := testNode(t, path, env)
	adminURL := server + "/api/admin"
	request := func(method, route, body string, cookie *http.Cookie, want int) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, adminURL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d, want %d", method, route, resp.StatusCode, want)
		}
		return resp
	}
	request("GET", "/settings", "", nil, 401)
	request("POST", "/login", `{"username":"bootstrap","password":"wrong"}`, nil, 401)
	login := request("POST", "/login", `{"username":"bootstrap","password":"initial-private-password"}`, nil, 204)
	cookies := login.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("missing protected session cookie")
	}
	request("PUT", "/settings", `{"clusterEnabled":true}`, cookies[0], 400)
	updated := request("PUT", "/settings", `{"username":"owner","clusterPeerUrls":["https://one.example/","http://two.example","https://one.example"]}`, cookies[0], 200)
	request("GET", "/settings", "", cookies[0], 401)
	var settings map[string]any
	if err := json.NewDecoder(updated.Body).Decode(&settings); err != nil {
		t.Fatal(err)
	}
	if settings["username"] != "owner" || settings["clusterEnabled"] != false || settings["password"] != nil {
		t.Fatalf("unexpected settings response: %v", settings)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"owner"`)) || bytes.Contains(data, []byte("initial-private-password")) {
		t.Fatalf("config did not persist only the override: %v", err)
	}
	reloaded, err := loadConfig(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil || reloaded.get().Username != "owner" || reloaded.get().Password != env["LOCALWEBS_PASSWORD"] ||
		strings.Join(reloaded.get().ClusterPeerURLs, ",") != "https://one.example,http://two.example" {
		t.Fatalf("config precedence not preserved on reload: %v", err)
	}
}
