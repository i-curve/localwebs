package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type attempt struct {
	limit *rate.Limiter
	seen  time.Time
}

type attempts struct {
	mu               sync.Mutex
	ips              map[string]attempt
	nextSweep        time.Time
	perMinute, burst int
}

func (a *attempts) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.After(a.nextSweep) {
		for key, entry := range a.ips {
			if now.Sub(entry.seen) > 10*time.Minute {
				delete(a.ips, key)
			}
		}
		a.nextSweep = now.Add(time.Minute)
	}
	entry, ok := a.ips[ip]
	if !ok {
		if len(a.ips) >= 10000 {
			return false
		}
		entry.limit = rate.NewLimiter(rate.Limit(float64(a.perMinute)/60), a.burst)
	}
	entry.seen = now
	a.ips[ip] = entry
	return entry.limit.Allow()
}

const sessionCookie = "localwebs_admin"
const sessionDuration = 8 * time.Hour

type admin struct {
	store   *configStore
	cluster *cluster
	key     [32]byte
	logins  attempts
}

func (a *admin) sign(value string, cfg config) string {
	mac := hmac.New(sha256.New, a.key[:])
	_ = json.NewEncoder(mac).Encode([]string{value, cfg.Username, cfg.Password})
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *admin) setSession(w http.ResponseWriter, r *http.Request, cfg config, age int) {
	value := ""
	if age > 0 {
		payload := strconv.FormatInt(time.Now().Add(sessionDuration).Unix(), 10) + "." + newID()
		value = payload + "." + a.sign(payload, cfg)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/api/admin", MaxAge: age,
		HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *admin) authorized(r *http.Request, cfg config) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || cfg.Password == "" {
		return false
	}
	fields := strings.Split(c.Value, ".")
	if len(fields) != 3 || !idPattern.MatchString(fields[1]) {
		return false
	}
	exp, err := strconv.ParseInt(fields[0], 10, 64)
	return err == nil && exp > time.Now().Unix() && equalSecret(fields[2], a.sign(fields[0]+"."+fields[1], cfg))
}

func fail(c *gin.Context, status int, text string) { c.JSON(status, gin.H{"error": text}) }

func jsonBody(c *gin.Context) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		fail(c, http.StatusUnsupportedMediaType, "Expected application/json.")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 80*1024)
	return true
}

func (a *admin) login(c *gin.Context) {
	cfg := a.store.get()
	if cfg.Password == "" {
		fail(c, http.StatusServiceUnavailable, "Set an administrator password on the server first.")
		return
	}
	if !a.logins.allow(c.RemoteIP()) {
		fail(c, http.StatusTooManyRequests, "Too many attempts. Try again shortly.")
		return
	}
	if !jsonBody(c) {
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(c.Request.Body, &input); err != nil {
		fail(c, http.StatusBadRequest, "Invalid login request.")
		return
	}
	userOK, passOK := equalSecret(input.Username, cfg.Username), equalSecret(input.Password, cfg.Password)
	if !userOK || !passOK {
		fail(c, http.StatusUnauthorized, "Incorrect username or password.")
		return
	}
	a.setSession(c.Writer, c.Request, cfg, int(sessionDuration.Seconds()))
	c.Status(http.StatusNoContent)
}

func (a *admin) settings(c *gin.Context) {
	cfg := a.store.get()
	if !a.authorized(c.Request, cfg) {
		fail(c, http.StatusUnauthorized, "Sign in to manage settings.")
		return
	}
	if c.Request.Method == http.MethodPut {
		if !jsonBody(c) {
			return
		}
		p, err := decodePatch(c.Request.Body)
		if err != nil {
			fail(c, http.StatusBadRequest, "Invalid settings request.")
			return
		}
		next, err := a.store.update(cfg, p)
		if err != nil {
			if strings.HasPrefix(err.Error(), "save config:") {
				log.Print(err)
				fail(c, http.StatusInternalServerError, "Could not save settings. Check the server logs.")
			} else {
				fail(c, http.StatusBadRequest, err.Error())
			}
			return
		}
		if next.Username != cfg.Username || next.Password != cfg.Password {
			a.setSession(c.Writer, c.Request, next, int(sessionDuration.Seconds()))
		}
		cfg = next
	}
	peers := 0
	if cfg.ClusterEnabled {
		peers = a.cluster.connected()
	}
	c.JSON(http.StatusOK, gin.H{
		"username": cfg.Username, "clusterEnabled": cfg.ClusterEnabled,
		"clusterPeerUrls": cfg.ClusterPeerURLs, "hasClusterKey": cfg.ClusterKey != "",
		"clusterConnectedPeers": peers,
	})
}

func router(h *hub, cl *cluster, webDir string) http.Handler {
	r := gin.New()
	r.Use(gin.Recovery(), func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Frame-Options", "DENY")
		c.Next()
	})
	a := &admin{store: cl.store, cluster: cl, logins: attempts{ips: make(map[string]attempt), perMinute: 8, burst: 8}}
	_, _ = rand.Read(a.key[:])
	joins := attempts{ips: make(map[string]attempt), perMinute: 30, burst: 20}
	r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/ws", func(c *gin.Context) {
		ip := c.RemoteIP()
		h.serve(c.Writer, c.Request, func() bool { return joins.allow(ip) })
	})
	r.GET("/ws/cluster", func(c *gin.Context) { cl.accept(c.Writer, c.Request) })
	api := r.Group("/api/admin", func(c *gin.Context) { c.Header("Cache-Control", "no-store"); c.Next() })
	api.POST("/login", a.login)
	api.POST("/logout", func(c *gin.Context) {
		if !jsonBody(c) {
			return
		}
		a.setSession(c.Writer, c.Request, config{}, -1)
		c.Status(http.StatusNoContent)
	})
	api.GET("/settings", a.settings)
	api.PUT("/settings", a.settings)
	r.Static("/assets", filepath.Join(webDir, "assets"))
	page := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.File(filepath.Join(webDir, "index.html"))
	}
	r.GET("/", page)
	r.GET("/admin", page)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/{token}", h.downloadHTTP)
	mux.Handle("/", r)
	return mux
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	store, err := loadConfig(envOr("LOCALWEBS_CONFIG_FILE", "config.json"), os.LookupEnv)
	if err != nil {
		log.Fatal(err)
	}
	if store.get().Password == "" {
		log.Print("Administrator login is disabled until a password is configured.")
	}
	webDir := envOr("LOCALWEBS_WEB_DIR", "dist")
	if _, err := os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		log.Fatal("Build the frontend with npm run build: ", err)
	}
	gin.SetMode(gin.ReleaseMode)
	h := newHub()
	cl := &cluster{h: h, store: store}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cl.supervise(ctx)
	s := &http.Server{
		Addr: envOr("LOCALWEBS_ADDR", ":8080"), Handler: router(h, cl, webDir),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdown); err != nil {
			log.Print(err)
		}
	}()
	log.Printf("Listening on %s", s.Addr)
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
