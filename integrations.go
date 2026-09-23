package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type providerConfig struct{ AuthURL, TokenURL, APIURL, ClientID, Secret, Scope string }

func provider(name string) (providerConfig, bool) {
	switch name {
	case "github":
		return providerConfig{"https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token", "https://api.github.com", os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"), "repo"}, true
	case "gitlab":
		return providerConfig{"https://gitlab.com/oauth/authorize", "https://gitlab.com/oauth/token", "https://gitlab.com/api/v4", os.Getenv("GITLAB_CLIENT_ID"), os.Getenv("GITLAB_CLIENT_SECRET"), "api"}, true
	}
	return providerConfig{}, false
}
func (a *App) oauthStart(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r)
	if u == "" {
		return
	}
	name := r.PathValue("provider")
	p, ok := provider(name)
	if !ok {
		fail(w, 404, "unknown provider")
		return
	}
	if p.ClientID == "" || p.Secret == "" {
		fail(w, 503, "configure "+strings.ToUpper(name)+"_CLIENT_ID and _CLIENT_SECRET first")
		return
	}
	pid := r.URL.Query().Get("project_id")
	a.store.mu.Lock()
	proj := project(&a.store.state, pid)
	minimum := 3
	if r.URL.Query().Get("review") == "1" {
		minimum = 2
	}
	allowed := proj != nil && role(&a.store.state, proj, u) >= minimum && !proj.Archived
	a.store.mu.Unlock()
	if !allowed {
		fail(w, 403, "active project maintainer required")
		return
	}
	state := id()
	verifier := id() + id()
	a.mu.Lock()
	for k, f := range a.flows {
		if time.Now().After(f.Expires) {
			delete(a.flows, k)
		}
	}
	a.flows[state] = oauthFlow{UserID: u, Provider: name, ProjectID: pid, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
	flow := a.flows[state]
	flow.Review = r.URL.Query().Get("review") == "1"
	a.flows[state] = flow
	a.mu.Unlock()
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {p.ClientID}, "redirect_uri": {a.baseURL + "/api/oauth/" + name + "/callback"}, "scope": {p.Scope}, "state": {state}, "response_type": {"code"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, p.AuthURL+"?"+q.Encode(), http.StatusFound)
}
func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r)
	if u == "" {
		return
	}
	name := r.PathValue("provider")
	p, ok := provider(name)
	if !ok {
		fail(w, 404, "unknown provider")
		return
	}
	state := r.URL.Query().Get("state")
	a.mu.Lock()
	f, found := a.flows[state]
	if found && f.UserID == u && f.Provider == name {
		delete(a.flows, state)
	}
	a.mu.Unlock()
	if !found || f.UserID != u || f.Provider != name || time.Now().After(f.Expires) {
		fail(w, 400, "invalid or expired OAuth state")
		return
	}
	if r.URL.Query().Get("code") == "" {
		fail(w, 400, "provider authorization was not completed")
		return
	}
	q := url.Values{"client_id": {p.ClientID}, "client_secret": {p.Secret}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {a.baseURL + "/api/oauth/" + name + "/callback"}, "grant_type": {"authorization_code"}, "code_verifier": {f.Verifier}}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", p.TokenURL, strings.NewReader(q.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		fail(w, 502, "OAuth token exchange failed")
		return
	}
	defer res.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" {
		fail(w, 502, "provider rejected token exchange")
		return
	}
	a.mu.Lock()
	a.connections[u+":"+name] = token.AccessToken
	a.mu.Unlock()
	if f.Review {
		http.Redirect(w, r, "/?project="+url.QueryEscape(f.ProjectID)+"&review=1", http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/?project="+url.QueryEscape(f.ProjectID)+"&import="+name, http.StatusSeeOther)
	}
}
func (a *App) providerGet(r *http.Request, name, token, path string, out any) error {
	p, ok := provider(name)
	if !ok {
		return reject(400, "unknown provider")
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", p.APIURL+path, nil)
	if err != nil {
		return reject(400, "invalid repository")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Forge-MVP")
	res, err := a.client.Do(req)
	if err != nil {
		return reject(502, "provider is unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return reject(502, fmt.Sprintf("provider returned HTTP %d; reconnect if authorization expired", res.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
		return reject(502, "invalid provider response")
	}
	return nil
}

type githubRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}
type gitlabRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"path_with_namespace"`
	HTMLURL       string `json:"web_url"`
	DefaultBranch string `json:"default_branch"`
}

func (a *App) connection(u, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connections[u+":"+name]
}
func (a *App) repositories(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r)
	if u == "" {
		return
	}
	name := r.PathValue("provider")
	if _, ok := provider(name); !ok {
		fail(w, 404, "unknown provider")
		return
	}
	token := a.connection(u, name)
	if token == "" {
		fail(w, 401, "connect provider first")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 10000 {
		fail(w, 400, "invalid page")
		return
	}
	repos := []Repository{}
	var err error
	if name == "github" {
		var data []githubRepo
		err = a.providerGet(r, name, token, fmt.Sprintf("/user/repos?per_page=50&sort=updated&page=%d", page), &data)
		for _, v := range data {
			repos = append(repos, Repository{name, strconv.FormatInt(v.ID, 10), v.FullName, v.HTMLURL, v.DefaultBranch})
		}
	} else {
		var data []gitlabRepo
		err = a.providerGet(r, name, token, fmt.Sprintf("/projects?membership=true&per_page=50&order_by=last_activity_at&page=%d", page), &data)
		for _, v := range data {
			repos = append(repos, Repository{name, strconv.FormatInt(v.ID, 10), v.FullName, v.HTMLURL, v.DefaultBranch})
		}
	}
	if err != nil {
		actionError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"repositories": repos, "page": page, "has_more": len(repos) == 50})
}
func (a *App) importRepository(w http.ResponseWriter, r *http.Request) {
	a.reviewMu.Lock()
	defer a.reviewMu.Unlock()
	u := a.require(w, r)
	if u == "" {
		return
	}
	var v struct {
		ProjectID  string `json:"project_id"`
		Provider   string `json:"provider"`
		ExternalID string `json:"external_id"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, 400, err.Error())
		return
	}
	external, err := strconv.ParseInt(v.ExternalID, 10, 64)
	if err != nil || external <= 0 {
		fail(w, 400, "numeric repository ID required")
		return
	}
	token := a.connection(u, v.Provider)
	if token == "" {
		fail(w, 401, "connect provider first")
		return
	}
	a.store.mu.Lock()
	p := project(&a.store.state, v.ProjectID)
	allowed := p != nil && role(&a.store.state, p, u) >= 3 && !p.Archived
	a.store.mu.Unlock()
	if !allowed {
		fail(w, 403, "active project maintainer required")
		return
	}
	var repo Repository
	if v.Provider == "github" {
		var data githubRepo
		err = a.providerGet(r, v.Provider, token, "/repositories/"+strconv.FormatInt(external, 10), &data)
		repo = Repository{v.Provider, strconv.FormatInt(data.ID, 10), data.FullName, data.HTMLURL, data.DefaultBranch}
	} else if v.Provider == "gitlab" {
		var data gitlabRepo
		err = a.providerGet(r, v.Provider, token, "/projects/"+strconv.FormatInt(external, 10), &data)
		repo = Repository{v.Provider, strconv.FormatInt(data.ID, 10), data.FullName, data.HTMLURL, data.DefaultBranch}
	} else {
		fail(w, 400, "unknown provider")
		return
	}
	if err != nil {
		actionError(w, err)
		return
	}
	err = a.transaction(func(s *State) error {
		p := project(s, v.ProjectID)
		if p == nil || role(s, p, u) < 3 || p.Archived {
			return reject(403, "active project maintainer required")
		}
		for _, m := range s.MergeRequests {
			if m.ProjectID == p.ID && m.Remote != nil && (repo.Provider != p.Repository.Provider || repo.ExternalID != p.Repository.ExternalID) {
				return reject(409, "create a separate project to connect a different repository; this project has linked reviews")
			}
		}
		p.Repository = repo
		record(s, p.ID, p.OrgID, u, "repository.import", repo.FullName, true)
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	jsonResponse(w, 200, repo)
}
func (a *App) webhook(w http.ResponseWriter, r *http.Request) {
	a.reviewMu.Lock()
	defer a.reviewMu.Unlock()
	name := r.PathValue("provider")
	if name != "github" && name != "gitlab" {
		fail(w, 404, "unknown provider")
		return
	}
	pid := r.PathValue("project")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 413, "payload too large")
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	if name == "gitlab" {
		delivery = r.Header.Get("X-Gitlab-Event-UUID")
		event = r.Header.Get("X-Gitlab-Event")
	}
	if delivery == "" || event == "" || len(delivery) > 200 || len(event) > 100 {
		fail(w, 400, "event and delivery headers required")
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		fail(w, 400, "JSON object required")
		return
	}
	duplicate := false
	err = a.transaction(func(s *State) error {
		p := project(s, pid)
		if p == nil {
			return reject(404, "project not found")
		}
		if p.Archived {
			return reject(409, "project is archived")
		}
		if name == "github" {
			sig, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
			mac := hmac.New(sha256.New, []byte(p.WebhookSecret))
			_, _ = mac.Write(body)
			if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
				return reject(401, "invalid webhook signature")
			}
		} else if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(p.WebhookSecret)) != 1 {
			return reject(401, "invalid webhook token")
		}
		key := name + ":" + pid + ":" + delivery
		if s.Deliveries == nil {
			s.Deliveries = map[string]bool{}
		}
		if s.Deliveries[key] {
			duplicate = true
			return nil
		}
		s.Deliveries[key] = true
		if p.Repository.Provider == name {
			// Treat deliveries as invalidation signals, not authoritative merge results.
			// Out-of-order events cannot roll a request back to an older commit.
			for i := range s.MergeRequests {
				m := &s.MergeRequests[i]
				if m.ProjectID == p.ID && m.Remote != nil && m.State == "open" {
					m.Remote.NeedsSync = true
				}
			}
		}
		record(s, p.ID, p.OrgID, name, "webhook."+event, "Delivery "+delivery, false)
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]bool{"accepted": true, "duplicate": duplicate})
}
