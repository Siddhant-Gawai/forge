package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthImportWithMockProvider(t *testing.T) {
	for _, name := range []string{"github", "gitlab"} {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			t.Setenv(strings.ToUpper(name)+"_CLIENT_ID", "client")
			t.Setenv(strings.ToUpper(name)+"_CLIENT_SECRET", "secret")
			a.client = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"access_token":"provider-token"}`
				if r.Method == "POST" {
					r.ParseForm()
					if r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "secret" {
						t.Error("missing PKCE/client credentials")
					}
				} else {
					if r.Header.Get("Authorization") != "Bearer provider-token" {
						t.Error("provider token not attached")
					}
					if name == "github" {
						body = `{"id":123,"full_name":"acme/imported","html_url":"https://github.com/acme/imported","default_branch":"main"}`
					} else {
						body = `{"id":123,"path_with_namespace":"acme/imported","web_url":"https://gitlab.com/acme/imported","default_branch":"main"}`
					}
					if r.URL.Path == "/user/repos" || r.URL.Path == "/api/v4/projects" {
						body = "[" + body + "]"
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			w := request(a, "GET", "/api/oauth/"+name+"/start?project_id=orbit", "jordan", nil)
			if w.Code != 403 {
				t.Fatal("developer connected project", w.Code)
			}
			w = request(a, "GET", "/api/oauth/"+name+"/start?project_id=orbit", "alex", nil)
			if w.Code != 302 {
				t.Fatal(w.Body)
			}
			target, _ := url.Parse(w.Header().Get("Location"))
			state := target.Query().Get("state")
			if state == "" || target.Query().Get("code_challenge") == "" {
				t.Fatal("state/PKCE missing")
			}
			callback := "/api/oauth/" + name + "/callback?state=" + state + "&code=code"
			if w = request(a, "GET", callback, "sam", nil); w.Code != 400 {
				t.Fatal("OAuth state not user-bound")
			}
			if w = request(a, "GET", callback, "alex", nil); w.Code != 303 {
				t.Fatal(w.Body)
			}
			if w = request(a, "GET", callback, "alex", nil); w.Code != 400 {
				t.Fatal("OAuth state replay accepted")
			}
			if w = request(a, "GET", "/api/oauth/"+name+"/repositories", "alex", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "acme/imported") {
				t.Fatal(w.Body)
			}
			if w = request(a, "POST", "/api/import", "alex", map[string]string{"project_id": "orbit", "provider": name, "external_id": "123"}); w.Code != 200 {
				t.Fatal(w.Body)
			}
			if a.store.state.Projects[0].Repository.FullName != "acme/imported" || len(a.store.state.Audit) != 1 {
				t.Fatal("import or audit missing")
			}
			w = request(a, "GET", "/api/state", "alex", nil)
			if strings.Contains(w.Body.String(), "provider-token") {
				t.Fatal("provider token exposed")
			}
		})
	}
}

func TestTeamsLabelsMilestonesAndNotificationOwnership(t *testing.T) {
	a := testApp(t)
	act(t, a, "alex", Command{Kind: "team.create", OrgID: "acme", Name: "Delivery", Members: []string{"sam"}}, 200)
	team := a.store.state.Teams[1]
	act(t, a, "sam", Command{Kind: "team.update", ID: team.ID, OrgID: "acme", Name: "Delivery team", Members: []string{"sam", "jordan"}}, 200)
	act(t, a, "sam", Command{Kind: "project.create", OrgID: "acme", Name: "delivery", TeamID: team.ID}, 200)
	act(t, a, "sam", Command{Kind: "label.create", Name: "urgent", Color: "invalid"}, 400)
	act(t, a, "sam", Command{Kind: "label.create", Name: "urgent", Color: "#FF0000"}, 200)
	act(t, a, "sam", Command{Kind: "label.create", Name: "urgent", Color: "#FF0000"}, 409)
	act(t, a, "sam", Command{Kind: "milestone.create", Title: "Launch", DueDate: "not-a-date"}, 400)
	act(t, a, "sam", Command{Kind: "milestone.create", Title: "Launch", DueDate: "2027-01-01"}, 200)
	m := a.store.state.Milestones[1]
	act(t, a, "sam", Command{Kind: "milestone.state", ID: m.ID, State: "closed"}, 200)
	act(t, a, "sam", Command{Kind: "issue.create", Title: "Assigned task", AssigneeID: "jordan"}, 200)
	n := a.store.state.Notifications[0]
	act(t, a, "sam", Command{Kind: "notification.read", ID: n.ID}, 200)
	if a.store.state.Notifications[0].Read {
		t.Fatal("another user's notification marked read")
	}
	act(t, a, "jordan", Command{Kind: "notification.read", ID: n.ID}, 200)
	if !a.store.state.Notifications[0].Read {
		t.Fatal("notification not marked read")
	}
	old := a.store.state.Projects[0].WebhookSecret
	act(t, a, "sam", Command{Kind: "webhook.rotate"}, 200)
	if old == a.store.state.Projects[0].WebhookSecret {
		t.Fatal("secret not rotated")
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(s, true, "http://localhost")
	for _, u := range s.state.Users {
		a.sessions[u.ID] = session{u.ID, time.Now().Add(time.Hour)}
	}
	t.Cleanup(func() { a.cancel(); a.workers.Wait() })
	return a
}
func request(a *App, method, path, u string, v any) *httptest.ResponseRecorder {
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("X-Requested-With", "forge")
	if u != "" {
		r.AddCookie(&http.Cookie{Name: "forge_session", Value: u})
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func act(t *testing.T, a *App, u string, c Command, status int) *httptest.ResponseRecorder {
	t.Helper()
	if c.ProjectID == "" {
		c.ProjectID = "orbit"
	}
	w := request(a, "POST", "/api/action", u, c)
	if w.Code != status {
		t.Fatalf("%s: got %d, want %d: %s", c.Kind, w.Code, status, w.Body.String())
	}
	return w
}
func TestAuthenticationAndCSRF(t *testing.T) {
	a := testApp(t)
	if w := request(a, "GET", "/api/state", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/api/action", strings.NewReader(`{}`))
	r.AddCookie(&http.Cookie{Name: "forge_session", Value: "alex"})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r.Header.Set("X-Requested-With", "forge")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestRolesAndIsolation(t *testing.T) {
	a := testApp(t)
	act(t, a, "jordan", Command{Kind: "project.settings", Name: "changed"}, 403)
	act(t, a, "sam", Command{Kind: "organization.member", OrgID: "acme", UserID: "jordan", Role: "owner"}, 403)
	act(t, a, "alex", Command{Kind: "organization.member", OrgID: "acme", UserID: "alex", Role: "guest"}, 409)
	act(t, a, "alex", Command{Kind: "organization.create", Name: "Private"}, 200)
	o := a.store.state.Organizations[1]
	act(t, a, "alex", Command{Kind: "project.create", OrgID: o.ID, Name: "Secret"}, 200)
	p := a.store.state.Projects[1]
	w := request(a, "GET", "/api/state", "jordan", nil)
	if strings.Contains(w.Body.String(), p.ID) {
		t.Fatal("private project leaked")
	}
	act(t, a, "jordan", Command{Kind: "issue.create", ProjectID: p.ID, Title: "intrusion"}, 404)
	act(t, a, "alex", Command{Kind: "organization.member", OrgID: "acme", UserID: "jordan", Role: "guest"}, 200)
	act(t, a, "jordan", Command{Kind: "issue.create", Title: "denied"}, 403)
	w = request(a, "GET", "/api/state", "jordan", nil)
	if strings.Contains(w.Body.String(), a.store.state.Projects[0].WebhookSecret) {
		t.Fatal("webhook secret leaked")
	}
}
func TestIssueValidationAndAssignment(t *testing.T) {
	a := testApp(t)
	act(t, a, "alex", Command{Kind: "issue.create", Title: "bad label", Labels: []string{"foreign"}}, 400)
	act(t, a, "alex", Command{Kind: "issue.create", Title: "bad assignee", AssigneeID: "missing"}, 400)
	act(t, a, "alex", Command{Kind: "issue.create", Title: "bad milestone", MilestoneID: "foreign"}, 400)
	before := len(a.store.state.Issues)
	act(t, a, "alex", Command{Kind: "issue.create", Title: "New work", AssigneeID: "jordan", Labels: []string{"feature"}, MilestoneID: "v1"}, 200)
	if len(a.store.state.Issues) != before+1 || len(a.store.state.Notifications) != 1 {
		t.Fatal("issue or notification missing")
	}
	i := a.store.state.Issues[before]
	act(t, a, "jordan", Command{Kind: "issue.update", ID: i.ID, Title: i.Title, State: "closed", AssigneeID: "jordan"}, 200)
	reopened, err := openStore(a.store.path)
	if err != nil || reopened.state.Issues[before].State != "closed" {
		t.Fatal("update did not persist", err)
	}
}
func TestMergeApprovalAndDiscussionGates(t *testing.T) {
	a := testApp(t)
	act(t, a, "jordan", Command{Kind: "mr.approve", ID: "42"}, 403)
	act(t, a, "alex", Command{Kind: "mr.merge", ID: "42"}, 409)
	act(t, a, "sam", Command{Kind: "mr.approve", ID: "42"}, 200)
	act(t, a, "sam", Command{Kind: "mr.approve", ID: "42"}, 200)
	if len(a.store.state.MergeRequests[0].Approvals) != 1 {
		t.Fatal("duplicate approval")
	}
	act(t, a, "sam", Command{Kind: "mr.comment", ID: "42", Body: "Please check access", Path: "main.go", Line: 12}, 200)
	act(t, a, "alex", Command{Kind: "mr.merge", ID: "42"}, 409)
	comment := a.store.state.MergeRequests[0].Comments[0]
	act(t, a, "jordan", Command{Kind: "mr.resolve", ID: "42", CommentID: comment.ID}, 403)
	act(t, a, "sam", Command{Kind: "mr.resolve", ID: "42", CommentID: comment.ID}, 200)
	act(t, a, "alex", Command{Kind: "mr.merge", ID: "42"}, 200)
	act(t, a, "alex", Command{Kind: "mr.comment", ID: "42", Body: "too late"}, 409)
	if a.store.state.MergeRequests[0].State != "merged" {
		t.Fatal("not merged")
	}
}
func TestLatestPipelineAndRevokedApproval(t *testing.T) {
	a := testApp(t)
	act(t, a, "sam", Command{Kind: "mr.approve", ID: "42"}, 200)
	act(t, a, "alex", Command{Kind: "organization.member", OrgID: "acme", UserID: "sam", Role: "guest"}, 200)
	act(t, a, "alex", Command{Kind: "mr.merge", ID: "42"}, 409)
	act(t, a, "alex", Command{Kind: "mr.approve", ID: "42"}, 200)
	a.store.state.Pipelines = append(a.store.state.Pipelines, Pipeline{ID: "failed", ProjectID: "orbit", MRID: "42", State: "failed"})
	act(t, a, "alex", Command{Kind: "mr.merge", ID: "42"}, 409)
}
func TestArchiveAndAudit(t *testing.T) {
	a := testApp(t)
	act(t, a, "alex", Command{Kind: "project.settings", Name: "orbit-api", Archived: true, RequiredApprovals: 2, RequirePipeline: true}, 200)
	act(t, a, "sam", Command{Kind: "issue.create", Title: "blocked"}, 409)
	if len(a.store.state.Audit) != 1 {
		t.Fatal("audit missing")
	}
	act(t, a, "alex", Command{Kind: "project.settings", Name: "orbit-api", RequiredApprovals: 2}, 200)
	act(t, a, "sam", Command{Kind: "issue.create", Title: "allowed"}, 200)
	if err := verifyChain(a.store.state.Activity); err != nil {
		t.Fatal(err)
	}
	if err := verifyChain(a.store.state.Audit); err != nil {
		t.Fatal(err)
	}
}
func TestWebhookSignaturesAndReplay(t *testing.T) {
	for _, name := range []string{"github", "gitlab"} {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			body := []byte(`{"ref":"refs/heads/main"}`)
			secret := a.store.state.Projects[0].WebhookSecret
			send := func(valid bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "/api/webhooks/"+name+"/orbit", bytes.NewReader(body))
				if name == "github" {
					r.Header.Set("X-GitHub-Delivery", "delivery-1")
					r.Header.Set("X-GitHub-Event", "push")
					m := hmac.New(sha256.New, []byte(secret))
					m.Write(body)
					sig := hex.EncodeToString(m.Sum(nil))
					if !valid {
						sig = "wrong"
					}
					r.Header.Set("X-Hub-Signature-256", "sha256="+sig)
				} else {
					r.Header.Set("X-Gitlab-Event-UUID", "delivery-1")
					r.Header.Set("X-Gitlab-Event", "Push Hook")
					token := secret
					if !valid {
						token = "wrong"
					}
					r.Header.Set("X-Gitlab-Token", token)
				}
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				return w
			}
			if w := send(false); w.Code != 401 {
				t.Fatal(w.Code)
			}
			if w := send(true); w.Code != 200 {
				t.Fatal(w.Body)
			}
			if w := send(true); w.Code != 200 || !strings.Contains(w.Body.String(), `"duplicate":true`) {
				t.Fatal(w.Body)
			}
			if len(a.store.state.Activity) != 1 {
				t.Fatal("replay appended activity")
			}
			s, err := openStore(a.store.path)
			if err != nil || !s.state.Deliveries[name+":orbit:delivery-1"] {
				t.Fatal("dedup not persisted", err)
			}
		})
	}
}
func TestPipelineLifecycle(t *testing.T) {
	for _, outcome := range []string{"passed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			a := testApp(t)
			act(t, a, "alex", Command{Kind: "pipeline.create", Ref: "wrong", MRID: "42", Outcome: outcome}, 400)
			w := act(t, a, "alex", Command{Kind: "pipeline.create", Ref: "feature/project-access", MRID: "42", Outcome: outcome}, 200)
			var p Pipeline
			json.Unmarshal(w.Body.Bytes(), &p)
			if p.State != "queued" {
				t.Fatal(p.State)
			}
			deadline := time.Now().Add(9 * time.Second)
			sawRunning := false
			for time.Now().Before(deadline) {
				a.store.mu.Lock()
				cur := a.store.state.Pipelines[len(a.store.state.Pipelines)-1]
				a.store.mu.Unlock()
				if cur.State == "running" {
					sawRunning = true
				}
				if cur.State == outcome {
					if !sawRunning || len(cur.Logs) < 6 {
						t.Fatal("missing running state or logs")
					}
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatal("pipeline timed out")
		})
	}
}
func TestTransactionRollbackAndTamperDetection(t *testing.T) {
	a := testApp(t)
	before := len(a.store.state.Issues)
	original := a.store.path
	a.store.path = t.TempDir()
	act(t, a, "alex", Command{Kind: "issue.create", Title: "rollback"}, 500)
	if len(a.store.state.Issues) != before {
		t.Fatal("failed write changed state")
	}
	a.store.path = original
	act(t, a, "alex", Command{Kind: "issue.create", Title: "persist"}, 200)
	a.store.state.Activity[0].Detail = "tampered"
	b, _ := json.Marshal(a.store.state)
	os.WriteFile(original, b, 0600)
	if _, err := openStore(original); err == nil {
		t.Fatal("tampered event accepted")
	}
}
func TestSSEAndOAuthValidation(t *testing.T) {
	a := testApp(t)
	server := httptest.NewServer(a)
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/api/events", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: "alex"})
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	line, _ := reader.ReadString('\n')
	if line != "event: ready\n" {
		t.Fatal(line)
	}
	reader.ReadString('\n')
	reader.ReadString('\n')
	act(t, a, "alex", Command{Kind: "issue.create", Title: "stream test"}, 200)
	line, _ = reader.ReadString('\n')
	if line != "event: update\n" {
		t.Fatal(line)
	}
	res.Body.Close()
	w := request(a, "GET", "/api/oauth/github/callback?state=bogus&code=x", "alex", nil)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = request(a, "GET", "/api/oauth/github/repositories", "alex", nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
