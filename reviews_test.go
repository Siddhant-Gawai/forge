package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type reviewFixture struct {
	head, ci, status string
	merged, unknown  bool
	mergeCalls       int
	identity         int64
}

func remoteFixture(t *testing.T, name string) (*App, *reviewFixture) {
	t.Helper()
	a := testApp(t)
	f := &reviewFixture{head: strings.Repeat("a", 40), ci: "success", status: "clean", identity: 22}
	a.store.state.Projects[0].Repository = Repository{Provider: name, ExternalID: "123", FullName: "acme/orbit", DefaultBranch: "main"}
	for _, u := range []string{"alex", "sam", "jordan"} {
		a.connections[u+":"+name] = "token"
	}
	a.client = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		var body any
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing provider credential")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/repositories/"):
			body = map[string]any{"id": 456, "full_name": "acme/another", "html_url": "https://github.com/acme/another", "default_branch": "main"}
		case r.Method == "PUT":
			f.mergeCalls++
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if p["sha"] != f.head {
				t.Error("merge not bound to current head")
			}
			if f.unknown {
				return nil, errors.New("response lost")
			}
			f.merged = true
			body = map[string]any{"merged": true}
		case strings.HasSuffix(r.URL.Path, "/user"):
			body = map[string]any{"id": f.identity}
		case strings.HasSuffix(r.URL.Path, "/status"):
			body = map[string]any{"state": f.ci, "total_count": 1}
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			body = map[string]any{"total_count": 0, "check_runs": []any{}}
		case strings.HasSuffix(r.URL.Path, "/files"):
			body = []any{map[string]any{"filename": "main.go", "status": "modified", "patch": "@@ -1,2 +1,2 @@\n-old\n+new\n context"}}
		case strings.HasSuffix(r.URL.Path, "/diffs"):
			body = []any{map[string]any{"new_path": "main.go", "old_path": "main.go", "diff": "@@ -1,2 +1,2 @@\n-old\n+new\n context"}}
		default:
			if name == "github" {
				state := "open"
				if f.merged {
					state = "closed"
				}
				body = map[string]any{"title": "Real request", "body": "Description", "state": state, "merged": f.merged, "mergeable": true, "mergeable_state": f.status, "user": map[string]any{"id": 11, "login": "author"}, "head": map[string]any{"sha": f.head, "ref": "feature"}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": map[string]any{"id": 123}}}
			} else {
				state := "opened"
				if f.merged {
					state = "merged"
				}
				status := "mergeable"
				if f.status != "clean" {
					status = f.status
				}
				body = map[string]any{"title": "Real request", "description": "Description", "state": state, "sha": f.head, "source_branch": "feature", "target_branch": "main", "target_project_id": 123, "author": map[string]any{"id": 11, "username": "author"}, "detailed_merge_status": status, "diff_refs": map[string]any{"base_sha": strings.Repeat("b", 40)}, "head_pipeline": map[string]any{"sha": f.head, "status": f.ci}}
			}
		}
		b, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}, nil
	})}
	return a, f
}
func reviewDo(t *testing.T, a *App, u string, c reviewCommand, status int) MergeRequest {
	t.Helper()
	c.ProjectID = "orbit"
	w := request(a, "POST", "/api/reviews", u, c)
	if w.Code != status {
		t.Fatalf("%s: want %d got %d: %s", c.Action, status, w.Code, w.Body.String())
	}
	var out struct {
		MergeRequest MergeRequest `json:"merge_request"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.MergeRequest
}
func TestProviderReviewMergeWorkflow(t *testing.T) {
	for _, name := range []string{"github", "gitlab"} {
		t.Run(name, func(t *testing.T) {
			a, f := remoteFixture(t, name)
			m := reviewDo(t, a, "alex", reviewCommand{Action: "link", Number: 7}, 200)
			c := reviewCommand{ID: m.ID, HeadSHA: f.head}
			c.Action = "merge"
			reviewDo(t, a, "alex", c, 409)
			c.Action = "diff"
			reviewDo(t, a, "sam", c, 200)
			c.Action = "review"
			c.Decision = "approve"
			f.identity = 11
			reviewDo(t, a, "sam", c, 403)
			f.identity = 22
			reviewDo(t, a, "sam", c, 200)
			act(t, a, "alex", Command{Kind: "mr.merge", ID: m.ID}, 409)
			c.Action = "comment"
			c.Body = "Fix this"
			c.Path = "main.go"
			c.Line = 100
			c.Side = "new"
			reviewDo(t, a, "sam", c, 400)
			c.Line = 1
			m = reviewDo(t, a, "sam", c, 200)
			c.Action = "merge"
			reviewDo(t, a, "alex", c, 409)
			act(t, a, "sam", Command{Kind: "mr.resolve", ID: m.ID, CommentID: m.Comments[0].ID}, 200)
			f.ci = "failed"
			reviewDo(t, a, "alex", c, 409)
			f.ci = "success"
			f.status = "blocked"
			reviewDo(t, a, "alex", c, 409)
			f.status = "clean"
			f.head = strings.Repeat("c", 40)
			reviewDo(t, a, "alex", c, 409)
			c.HeadSHA = f.head
			reviewDo(t, a, "alex", c, 409)
			c.Action = "review"
			c.Decision = "request_changes"
			reviewDo(t, a, "sam", c, 200)
			c.Action = "merge"
			reviewDo(t, a, "alex", c, 409)
			c.Action = "review"
			c.Decision = "approve"
			reviewDo(t, a, "sam", c, 200)
			c.Action = "merge"
			reviewDo(t, a, "jordan", c, 403)
			m = reviewDo(t, a, "alex", c, 200)
			if f.mergeCalls != 1 || m.State != "merged" || m.Remote.MergeAttempt != "" {
				t.Fatal("provider result not reconciled")
			}
			if err := verifyChain(a.store.state.Audit); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestUnknownMergeOutcomeCannotRetry(t *testing.T) {
	a, f := remoteFixture(t, "github")
	a.store.state.Projects[0].RequiredApprovals = 0
	m := reviewDo(t, a, "alex", reviewCommand{Action: "link", Number: 1}, 200)
	f.unknown = true
	c := reviewCommand{Action: "merge", ID: m.ID, HeadSHA: f.head}
	reviewDo(t, a, "alex", c, 409)
	reviewDo(t, a, "alex", c, 409)
	if f.mergeCalls != 1 {
		t.Fatal("retried an uncertain merge")
	}
	f.merged = true
	m = reviewDo(t, a, "alex", reviewCommand{Action: "sync", ID: m.ID}, 200)
	if m.State != "merged" || m.Remote.MergeAttempt != "" {
		t.Fatal("sync did not reconcile external merge")
	}
}
func TestReviewPersistenceAndDistinctIdentity(t *testing.T) {
	a, f := remoteFixture(t, "github")
	a.store.state.Projects[0].RequiredApprovals = 2
	m := reviewDo(t, a, "alex", reviewCommand{Action: "link", Number: 3}, 200)
	c := reviewCommand{Action: "review", ID: m.ID, HeadSHA: f.head, Decision: "approve"}
	reviewDo(t, a, "sam", c, 200)
	reviewDo(t, a, "jordan", c, 200)
	c.Action = "merge"
	reviewDo(t, a, "alex", c, 409)
	s, err := inflateState(flattenState(a.store.state))
	if err != nil {
		t.Fatal(err)
	}
	stored := findMR(&s, "orbit", m.ID)
	if stored == nil || len(stored.Remote.Reviews) != 2 {
		t.Fatal("reviews lost in database round trip")
	}
	if f.mergeCalls != 0 {
		t.Fatal("same provider identity counted twice")
	}
	w := request(a, "POST", "/api/import", "alex", map[string]string{"project_id": "orbit", "provider": "github", "external_id": "456"})
	if w.Code != 409 {
		t.Fatal("repository replacement should be blocked", w.Code, w.Body)
	}
}

func TestMergePersistenceFailurePreventsProviderWrite(t *testing.T) {
	a, f := remoteFixture(t, "github")
	a.store.state.Projects[0].RequiredApprovals = 0
	m := reviewDo(t, a, "alex", reviewCommand{Action: "link", Number: 1}, 200)
	block := filepath.Join(t.TempDir(), "file-not-directory")
	if err := os.WriteFile(block, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	a.store.path = filepath.Join(block, "state.json")
	reviewDo(t, a, "alex", reviewCommand{Action: "merge", ID: m.ID, HeadSHA: f.head}, 500)
	if f.mergeCalls != 0 {
		t.Fatal("provider write occurred despite persistence failure")
	}
}

func TestMergeBlockersUseRealCIAndCurrentPermissions(t *testing.T) {
	a, f := remoteFixture(t, "github")
	m := reviewDo(t, a, "alex", reviewCommand{Action: "link", Number: 1}, 200)
	reviewDo(t, a, "sam", reviewCommand{Action: "review", ID: m.ID, HeadSHA: f.head, Decision: "approve"}, 200)
	a.store.state.Organizations[0].Members["sam"] = "guest"
	c := reviewCommand{Action: "merge", ID: m.ID, HeadSHA: f.head}
	reviewDo(t, a, "alex", c, 409)
	a.store.state.Organizations[0].Members["sam"] = "maintainer"
	f.ci = "pending"
	a.store.state.Pipelines = append(a.store.state.Pipelines, Pipeline{ID: "fake-ci", ProjectID: "orbit", MRID: m.ID, State: "passed"})
	reviewDo(t, a, "alex", c, 409)
	a.store.state.Projects[0].Archived = true
	reviewDo(t, a, "alex", c, 403)
	if f.mergeCalls != 0 {
		t.Fatal("blocked merge executed")
	}
}
