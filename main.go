package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type session struct {
	UserID  string
	Expires time.Time
}
type oauthFlow struct {
	Review                                bool
	UserID, Provider, ProjectID, Verifier string
	Expires                               time.Time
}
type App struct {
	reviewMu     sync.Mutex // Serializes review gates with permission/settings mutations.
	store        *Store
	mux          *http.ServeMux
	demo         bool
	baseURL      string
	client       *http.Client
	mu           sync.Mutex
	sessions     map[string]session
	flows        map[string]oauthFlow
	connections  map[string]string
	subscribers  map[chan struct{}]bool
	ctx          context.Context
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	authAttempts map[string]authAttempt
}

func newApp(s *Store, demo bool, base string) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{store: s, demo: demo, baseURL: base, client: &http.Client{Timeout: 20 * time.Second}, sessions: map[string]session{}, flows: map[string]oauthFlow{}, connections: map[string]string{}, subscribers: map[chan struct{}]bool{}, ctx: ctx, cancel: cancel}
	a.mux = http.NewServeMux()
	a.mux.HandleFunc("GET /api/session", a.sessionInfo)
	a.mux.HandleFunc("POST /api/login", a.login)
	a.mux.HandleFunc("POST /api/signup", a.signup)
	a.mux.HandleFunc("POST /api/logout", a.logout)
	a.mux.HandleFunc("GET /api/state", a.getState)
	a.mux.HandleFunc("GET /api/events", a.events)
	a.mux.HandleFunc("POST /api/action", a.action)
	a.mux.HandleFunc("POST /api/reviews", a.reviews)
	a.mux.HandleFunc("GET /api/oauth/{provider}/start", a.oauthStart)
	a.mux.HandleFunc("GET /api/oauth/{provider}/callback", a.oauthCallback)
	a.mux.HandleFunc("GET /api/oauth/{provider}/repositories", a.repositories)
	a.mux.HandleFunc("POST /api/import", a.importRepository)
	a.mux.HandleFunc("POST /api/webhooks/{provider}/{project}", a.webhook)
	a.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		backend := "json"
		if a.store.pg != nil {
			backend = "postgres"
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := a.store.pg.db.PingContext(ctx); err != nil {
				fail(w, 503, "database unavailable")
				return
			}
		}
		jsonResponse(w, 200, map[string]string{"status": "ok", "storage": backend})
	})
	sub, _ := fs.Sub(assets, "web")
	for _, route := range []string{"/login", "/signup", "/dashboard"} {
		a.mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			b, _ := assets.ReadFile("web/index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(b)
		})
	}
	a.mux.Handle("/", http.FileServer(http.FS(sub)))
	return a
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if r.Method != "GET" && r.Method != "HEAD" && !strings.HasPrefix(r.URL.Path, "/api/webhooks/") {
		origin := r.Header.Get("Origin")
		if origin != "" && origin != a.baseURL {
			fail(w, 403, "untrusted origin")
			return
		}
		if r.Header.Get("X-Requested-With") != "forge" {
			fail(w, 403, "X-Requested-With: forge is required")
			return
		}
	}
	a.mux.ServeHTTP(w, r)
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid request body")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (a *App) user(r *http.Request) string {
	if token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); token != "" && token == os.Getenv("FORGE_ADMIN_TOKEN") {
		return "alex"
	}
	c, err := r.Cookie("forge_session")
	if err != nil {
		return ""
	}
	if a.store.pg != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var uid string
		if err := a.store.pg.db.QueryRowContext(ctx, "SELECT user_id FROM forge.sessions WHERE token_hash=$1 AND expires_at>now()", tokenHash(c.Value)).Scan(&uid); err != nil {
			return ""
		}
		return uid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		delete(a.sessions, c.Value)
		return ""
	}
	return s.UserID
}
func (a *App) require(w http.ResponseWriter, r *http.Request) string {
	u := a.user(r)
	if u == "" {
		fail(w, 401, "sign in to continue")
	}
	return u
}
func (a *App) sessionInfo(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, map[string]any{"user_id": a.user(r), "demo": a.demo})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.allowAuth(r) {
		fail(w, 429, "too many attempts; try again in 15 minutes")
		return
	}
	var v struct {
		UserID   string `json:"user_id"`
		Token    string `json:"token"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if v.Email != "" || v.Password != "" {
		v.UserID = a.passwordLogin(v.Email, v.Password)
		if v.UserID == "" {
			fail(w, 401, "invalid email or password")
			return
		}
	} else if a.demo && v.UserID != "" {
		if v.UserID != "alex" && v.UserID != "sam" && v.UserID != "jordan" {
			fail(w, 400, "unknown demo user")
			return
		}
	} else {
		if v.Token == "" || v.Token != os.Getenv("FORGE_ADMIN_TOKEN") {
			fail(w, 401, "invalid access token")
			return
		}
		v.UserID = "alex"
	}
	if err := a.startSession(w, v.UserID); err != nil {
		fail(w, 503, "could not create session")
		return
	}
	jsonResponse(w, 200, map[string]string{"user_id": v.UserID})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("forge_session"); err == nil {
		if a.store.pg != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if _, err := a.store.pg.db.ExecContext(ctx, "DELETE FROM forge.sessions WHERE token_hash=$1", tokenHash(c.Value)); err != nil {
				fail(w, 503, "could not end session; try again")
				return
			}
		}
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "forge_session", Path: "/", Value: "", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	jsonResponse(w, 200, map[string]bool{"ok": true})
}

var ranks = map[string]int{"guest": 1, "developer": 2, "maintainer": 3, "owner": 4}

func role(s *State, p *Project, u string) int {
	n := ranks[p.Members[u]]
	for _, o := range s.Organizations {
		if o.ID == p.OrgID && ranks[o.Members[u]] > n {
			n = ranks[o.Members[u]]
		}
	}
	return n
}
func orgRole(s *State, org, u string) int {
	for _, o := range s.Organizations {
		if o.ID == org {
			return ranks[o.Members[u]]
		}
	}
	return 0
}
func project(s *State, pid string) *Project {
	for i := range s.Projects {
		if s.Projects[i].ID == pid {
			return &s.Projects[i]
		}
	}
	return nil
}
func hasUser(s *State, u string) bool {
	for _, v := range s.Users {
		if v.ID == u {
			return true
		}
	}
	return false
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func appendEvent(list *[]Event, e Event) {
	if len(*list) > 0 {
		e.PreviousHash = (*list)[len(*list)-1].Hash
	}
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	e.Hash = hex.EncodeToString(h[:])
	*list = append(*list, e)
}
func verifyChain(list []Event) error {
	prev := ""
	for _, e := range list {
		hash := e.Hash
		e.Hash = ""
		if e.PreviousHash != prev {
			return errors.New("event chain is broken")
		}
		b, _ := json.Marshal(e)
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != hash {
			return errors.New("event integrity check failed")
		}
		prev = hash
	}
	return nil
}
func record(s *State, pid, org, u, action, detail string, sensitive bool) {
	e := Event{ID: id(), ProjectID: pid, OrgID: org, ActorID: u, Action: action, Detail: detail, CreatedAt: now()}
	appendEvent(&s.Activity, e)
	if sensitive {
		appendEvent(&s.Audit, e)
	}
}
func notify(s *State, pid, uid, msg string) {
	if uid != "" {
		s.Notifications = append(s.Notifications, Notification{ID: id(), UserID: uid, ProjectID: pid, Message: msg, CreatedAt: now()})
	}
}
func (a *App) broadcast() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (a *App) transaction(fn func(*State) error) error {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	b, _ := json.Marshal(a.store.state)
	var next State
	if err := json.Unmarshal(b, &next); err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	old := a.store.state
	a.store.state = next
	if err := a.store.save(); err != nil {
		a.store.state = old
		return fmt.Errorf("persist transaction: %w", err)
	}
	a.broadcast()
	return nil
}
func (a *App) getState(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r)
	if u == "" {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	s := a.store.state
	out := State{Users: []User{}, Organizations: []Organization{}, Teams: []Team{}, Projects: []Project{}, Labels: []Label{}, Milestones: []Milestone{}, Issues: []Issue{}, MergeRequests: []MergeRequest{}, Pipelines: []Pipeline{}, Activity: []Event{}, Audit: []Event{}, Notifications: []Notification{}}
	accessible := map[string]bool{}
	admin := map[string]bool{}
	for _, p := range s.Projects {
		n := role(&s, &p, u)
		if n > 0 {
			accessible[p.ID] = true
			admin[p.ID] = n >= 3
			if n < 3 {
				p.WebhookSecret = ""
			}
			out.Projects = append(out.Projects, p)
		}
	}
	for _, o := range s.Organizations {
		if ranks[o.Members[u]] > 0 {
			out.Organizations = append(out.Organizations, o)
		}
	}
	visibleUsers := map[string]bool{u: true}
	for _, o := range out.Organizations {
		for uid := range o.Members {
			visibleUsers[uid] = true
		}
	}
	for _, p := range out.Projects {
		for uid := range p.Members {
			visibleUsers[uid] = true
		}
		for _, o := range s.Organizations {
			if o.ID == p.OrgID {
				for uid := range o.Members {
					visibleUsers[uid] = true
				}
			}
		}
	}
	for _, v := range s.Users {
		if visibleUsers[v.ID] {
			out.Users = append(out.Users, v)
		}
	}
	for _, t := range s.Teams {
		if orgRole(&s, t.OrgID, u) > 0 {
			out.Teams = append(out.Teams, t)
		}
	}
	for _, v := range s.Labels {
		if accessible[v.ProjectID] {
			out.Labels = append(out.Labels, v)
		}
	}
	for _, v := range s.Milestones {
		if accessible[v.ProjectID] {
			out.Milestones = append(out.Milestones, v)
		}
	}
	for _, v := range s.Issues {
		if accessible[v.ProjectID] {
			out.Issues = append(out.Issues, v)
		}
	}
	for _, v := range s.MergeRequests {
		if accessible[v.ProjectID] {
			out.MergeRequests = append(out.MergeRequests, v)
		}
	}
	for _, v := range s.Pipelines {
		if accessible[v.ProjectID] {
			out.Pipelines = append(out.Pipelines, v)
		}
	}
	for _, v := range s.Activity {
		if accessible[v.ProjectID] || (v.ProjectID == "" && orgRole(&s, v.OrgID, u) > 0) {
			out.Activity = append(out.Activity, v)
		}
	}
	for _, v := range s.Audit {
		if admin[v.ProjectID] || (v.ProjectID == "" && orgRole(&s, v.OrgID, u) >= 3) {
			out.Audit = append(out.Audit, v)
		}
	}
	for _, v := range s.Notifications {
		if v.UserID == u && accessible[v.ProjectID] {
			out.Notifications = append(out.Notifications, v)
		}
	}
	jsonResponse(w, 200, map[string]any{"user_id": u, "data": out})
}
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	if a.require(w, r) == "" {
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subscribers[ch] = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.subscribers, ch); a.mu.Unlock() }()
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	f.Flush()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if a.user(r) == "" {
				return
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			f.Flush()
		case <-ch:
			if a.user(r) == "" {
				return
			}
			fmt.Fprint(w, "event: update\ndata: {}\n\n")
			f.Flush()
		}
	}
}
func main() {
	if err := loadDotEnv(); err != nil {
		log.Fatal(err)
	}
	addr := env("FORGE_ADDR", "127.0.0.1:8080")
	base := env("FORGE_BASE_URL", "http://"+addr)
	demo := os.Getenv("FORGE_DEMO") == "1"
	if token := os.Getenv("FORGE_ADMIN_TOKEN"); token != "" && len(token) < 24 {
		log.Fatal("FORGE_ADMIN_TOKEN must contain at least 24 characters")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		log.Fatal("invalid FORGE_BASE_URL")
	}
	var s *Store
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		s, err = openPostgres(dbURL, env("FORGE_DATA", "data/state.json"))
		if err == nil {
			defer s.pg.db.Close()
			log.Print("PostgreSQL storage active")
		}
	} else {
		s, err = openStore(env("FORGE_DATA", "data/state.json"))
	}
	if err != nil {
		log.Fatal(err)
	}
	a := newApp(s, demo, strings.TrimRight(base, "/"))
	a.recoverPipelines()
	server := &http.Server{Addr: addr, Handler: a, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		log.Printf("Forge ready at %s (demo=%s)", base, strconv.FormatBool(demo))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	a.workers.Wait()
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func loadDotEnv() error {
	b, err := os.ReadFile(env("FORGE_ENV_FILE", ".env"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read environment file")
	}
	for n, line := range strings.Split(strings.TrimPrefix(string(b), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid environment entry on line %d", n+1)
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return fmt.Errorf("unclosed environment value on line %d", n+1)
			}
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); key != "" && !exists {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}
