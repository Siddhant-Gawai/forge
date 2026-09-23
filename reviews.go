package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// RemoteRequest is persisted alongside the local review history. Provider tokens
// remain in the authenticated user's server-side OAuth connection only.
type RemoteRequest struct {
	Number       int            `json:"number"`
	HeadSHA      string         `json:"head_sha"`
	BaseSHA      string         `json:"base_sha"`
	AuthorID     int64          `json:"author_id"`
	Author       string         `json:"author"`
	URL          string         `json:"url"`
	Draft        bool           `json:"draft"`
	Mergeable    bool           `json:"mergeable"`
	MergeStatus  string         `json:"merge_status"`
	CI           string         `json:"ci"`
	SyncedAt     string         `json:"synced_at"`
	NeedsSync    bool           `json:"needs_sync"`
	MergeAttempt string         `json:"merge_attempt"`
	Reviews      []CommitReview `json:"reviews"`
}
type CommitReview struct {
	UserID         string `json:"user_id"`
	ProviderUserID int64  `json:"provider_user_id"`
	HeadSHA        string `json:"head_sha"`
	Decision       string `json:"decision"`
	Body           string `json:"body"`
	CreatedAt      string `json:"created_at"`
}
type DiffFile struct {
	Path        string `json:"path"`
	OldPath     string `json:"old_path"`
	Patch       string `json:"patch"`
	Status      string `json:"status"`
	Unavailable bool   `json:"unavailable"`
}
type reviewCommand struct {
	Action    string `json:"action"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
	Number    int    `json:"number"`
	HeadSHA   string `json:"head_sha"`
	Decision  string `json:"decision"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
}

func findMR(s *State, pid, mid string) *MergeRequest {
	for i := range s.MergeRequests {
		if s.MergeRequests[i].ID == mid && s.MergeRequests[i].ProjectID == pid {
			return &s.MergeRequests[i]
		}
	}
	return nil
}
func remotePath(repo Repository, n int) (string, error) {
	if n < 1 {
		return "", reject(400, "positive provider request number required")
	}
	if _, err := strconv.ParseInt(repo.ExternalID, 10, 64); err != nil {
		return "", reject(400, "import a repository first")
	}
	if repo.Provider == "gitlab" {
		return fmt.Sprintf("/projects/%s/merge_requests/%d", url.PathEscape(repo.ExternalID), n), nil
	}
	parts := strings.Split(repo.FullName, "/")
	if repo.Provider != "github" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", reject(400, "import a GitHub or GitLab repository first")
	}
	return fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(parts[0]), url.PathEscape(parts[1]), n), nil
}
func (a *App) fetchRemote(r *http.Request, repo Repository, token string, n int) (MergeRequest, error) {
	path, err := remotePath(repo, n)
	if err != nil {
		return MergeRequest{}, err
	}
	m := MergeRequest{Remote: &RemoteRequest{Number: n, CI: "unknown", SyncedAt: now()}, State: "open"}
	v := m.Remote
	if repo.Provider == "github" {
		var d struct {
			Title, Body, State string
			Draft, Merged      bool
			Mergeable          *bool
			MergeableState     string `json:"mergeable_state"`
			User               struct {
				ID    int64
				Login string
			}
			Head, Base struct {
				SHA, Ref string
				Repo     struct{ ID int64 }
			}
		}
		if err = a.providerGet(r, repo.Provider, token, path, &d); err != nil {
			return m, err
		}
		if strconv.FormatInt(d.Base.Repo.ID, 10) != repo.ExternalID {
			return m, reject(409, "request belongs to a different repository")
		}
		m.Title, m.Body, m.Source, m.Target = d.Title, d.Body, d.Head.Ref, d.Base.Ref
		v.HeadSHA, v.BaseSHA, v.AuthorID, v.Author = d.Head.SHA, d.Base.SHA, d.User.ID, d.User.Login
		v.Draft, v.MergeStatus = d.Draft, d.MergeableState
		v.Mergeable = d.Mergeable != nil && *d.Mergeable && d.MergeableState == "clean"
		v.URL = "https://github.com/" + repo.FullName + "/pull/" + strconv.Itoa(n)
		if d.State == "closed" {
			m.State = "closed"
		}
		if d.Merged {
			m.State = "merged"
		}
		if validSHA(v.HeadSHA) {
			prefix := strings.Split(path, "/pulls/")[0] + "/commits/" + v.HeadSHA
			var status struct {
				State string
				Total int `json:"total_count"`
			}
			var checks struct {
				Total int                                   `json:"total_count"`
				Runs  []struct{ Status, Conclusion string } `json:"check_runs"`
			}
			// Unknown or inaccessible checks never count as a passing pipeline.
			se := a.providerGet(r, repo.Provider, token, prefix+"/status", &status)
			ce := a.providerGet(r, repo.Provider, token, prefix+"/check-runs?per_page=100&filter=latest", &checks)
			if se == nil && ce == nil && checks.Total == len(checks.Runs) {
				v.CI = "passed"
				if status.Total+checks.Total == 0 {
					v.CI = "missing"
				}
				if status.Total > 0 && status.State != "success" {
					v.CI = "pending"
					if status.State == "failure" || status.State == "error" {
						v.CI = "failed"
					}
				}
				for _, c := range checks.Runs {
					if c.Status != "completed" {
						if v.CI != "failed" {
							v.CI = "pending"
						}
					} else if c.Conclusion != "success" && c.Conclusion != "neutral" && c.Conclusion != "skipped" {
						v.CI = "failed"
					}
				}
			}
		}
	} else {
		var d struct {
			Title, Description, State, SHA string
			SourceBranch                   string `json:"source_branch"`
			TargetBranch                   string `json:"target_branch"`
			TargetProjectID                int64  `json:"target_project_id"`
			Draft                          bool
			HasConflicts                   bool   `json:"has_conflicts"`
			DetailedMergeStatus            string `json:"detailed_merge_status"`
			Author                         struct {
				ID       int64
				Username string
			}
			DiffRefs struct {
				BaseSHA string `json:"base_sha"`
				HeadSHA string `json:"head_sha"`
			} `json:"diff_refs"`
			HeadPipeline *struct{ SHA, Status string } `json:"head_pipeline"`
		}
		if err = a.providerGet(r, repo.Provider, token, path, &d); err != nil {
			return m, err
		}
		if strconv.FormatInt(d.TargetProjectID, 10) != repo.ExternalID {
			return m, reject(409, "request belongs to a different repository")
		}
		m.Title, m.Body, m.Source, m.Target = d.Title, d.Description, d.SourceBranch, d.TargetBranch
		v.HeadSHA, v.BaseSHA, v.AuthorID, v.Author = d.SHA, d.DiffRefs.BaseSHA, d.Author.ID, d.Author.Username
		v.Draft, v.MergeStatus = d.Draft, d.DetailedMergeStatus
		v.Mergeable = !d.HasConflicts && d.DetailedMergeStatus == "mergeable"
		v.URL = "https://gitlab.com/" + repo.FullName + "/-/merge_requests/" + strconv.Itoa(n)
		if d.State == "closed" || d.State == "merged" {
			m.State = d.State
		}
		if d.HeadPipeline == nil {
			v.CI = "missing"
		} else if d.HeadPipeline.SHA == v.HeadSHA {
			switch d.HeadPipeline.Status {
			case "success":
				v.CI = "passed"
			case "failed", "canceled":
				v.CI = "failed"
			default:
				v.CI = "pending"
			}
		}
	}
	if !validSHA(v.HeadSHA) || v.AuthorID <= 0 || m.Title == "" {
		return m, reject(502, "provider request is incomplete; refresh when its commits are available")
	}
	return m, nil
}

var shaPattern = regexp.MustCompile(`^[a-fA-F0-9]{40,64}$`)

func validSHA(s string) bool { return shaPattern.MatchString(s) }

func (a *App) remoteFiles(r *http.Request, repo Repository, token string, n int) ([]DiffFile, error) {
	path, err := remotePath(repo, n)
	if err != nil {
		return nil, err
	}
	files := []DiffFile{}
	for page := 1; page <= 30; page++ {
		count := 0
		if repo.Provider == "github" {
			var rows []struct {
				Filename      string
				Previous      string `json:"previous_filename"`
				Patch, Status string
			}
			err = a.providerGet(r, repo.Provider, token, fmt.Sprintf("%s/files?per_page=100&page=%d", path, page), &rows)
			count = len(rows)
			for _, f := range rows {
				files = append(files, DiffFile{f.Filename, f.Previous, f.Patch, f.Status, f.Patch == ""})
			}
		} else {
			var rows []struct {
				NewPath   string `json:"new_path"`
				OldPath   string `json:"old_path"`
				Diff      string
				TooLarge  bool `json:"too_large"`
				Collapsed bool
			}
			err = a.providerGet(r, repo.Provider, token, fmt.Sprintf("%s/diffs?per_page=100&page=%d", path, page), &rows)
			count = len(rows)
			for _, f := range rows {
				files = append(files, DiffFile{f.NewPath, f.OldPath, f.Diff, "modified", f.Diff == "" || f.TooLarge || f.Collapsed})
			}
		}
		if err != nil {
			return nil, err
		}
		if count < 100 {
			return files, nil
		}
	}
	return nil, reject(422, "diff exceeds the 3,000-file preview limit; inspect it at the provider")
}

var hunkPattern = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func diffHasLine(f DiffFile, line int, side string) bool {
	old, newLine := 0, 0
	inHunk := false
	for _, text := range strings.Split(f.Patch, "\n") {
		if h := hunkPattern.FindStringSubmatch(text); h != nil {
			old, _ = strconv.Atoi(h[1])
			newLine, _ = strconv.Atoi(h[2])
			inHunk = true
			continue
		}
		if !inHunk || len(text) == 0 {
			continue
		}
		switch text[0] {
		case ' ':
			if side == "old" && old == line || side == "new" && newLine == line {
				return true
			}
			old++
			newLine++
		case '-':
			if side == "old" && old == line {
				return true
			}
			old++
		case '+':
			if side == "new" && newLine == line {
				return true
			}
			newLine++
		}
	}
	return false
}
func syncMR(m *MergeRequest, fresh MergeRequest) {
	previous := m.Remote
	if previous.HeadSHA != fresh.Remote.HeadSHA {
		m.Approvals = []string{}
	}
	fresh.Remote.Reviews = previous.Reviews
	fresh.Remote.MergeAttempt = previous.MergeAttempt
	m.Title, m.Body, m.Source, m.Target, m.State, m.Remote = fresh.Title, fresh.Body, fresh.Source, fresh.Target, fresh.State, fresh.Remote
	if m.State == "merged" {
		m.Remote.MergeAttempt = ""
	}
}
func mergeBlockers(s *State, p *Project, m *MergeRequest) []string {
	blocks := []string{}
	v := m.Remote
	if m.State != "open" {
		blocks = append(blocks, "Request is not open")
	}
	if v.Draft {
		blocks = append(blocks, "Request is a draft")
	}
	if v.NeedsSync {
		blocks = append(blocks, "Refresh provider status")
	}
	if !v.Mergeable {
		blocks = append(blocks, "Provider merge status: "+v.MergeStatus)
	}
	if p.RequirePipeline && v.CI != "passed" {
		blocks = append(blocks, "Provider CI must pass ("+v.CI+")")
	}
	latest := map[int64]CommitReview{}
	for _, review := range v.Reviews {
		latest[review.ProviderUserID] = review
	}
	approved := 0
	for _, review := range latest {
		if review.ProviderUserID == v.AuthorID || role(s, p, review.UserID) < 2 {
			continue
		}
		if review.Decision == "request_changes" {
			blocks = append(blocks, "Changes requested by a reviewer")
		}
		if review.Decision == "approve" && review.HeadSHA == v.HeadSHA {
			approved++
		}
	}
	if approved < p.RequiredApprovals {
		blocks = append(blocks, fmt.Sprintf("%d / %d current-commit approvals", approved, p.RequiredApprovals))
	}
	for _, c := range m.Comments {
		if !c.Resolved {
			blocks = append(blocks, "Resolve all review discussions")
			break
		}
	}
	return blocks
}

func (a *App) reviews(w http.ResponseWriter, r *http.Request) {
	u := a.require(w, r)
	if u == "" {
		return
	}
	var c reviewCommand
	if err := decode(w, r, &c); err != nil {
		fail(w, 400, err.Error())
		return
	}
	switch c.Action {
	case "link", "sync", "diff", "review", "comment", "merge":
	default:
		fail(w, 400, "unknown review action")
		return
	}
	a.reviewMu.Lock()
	defer a.reviewMu.Unlock()
	var p Project
	var m MergeRequest
	a.store.mu.Lock()
	proj := project(&a.store.state, c.ProjectID)
	allowed := proj != nil && role(&a.store.state, proj, u) >= 2 && !proj.Archived
	if allowed {
		p = *proj
		if found := findMR(&a.store.state, p.ID, c.ID); found != nil {
			m = *found
		}
	}
	maintainer := allowed && role(&a.store.state, proj, u) >= 3
	a.store.mu.Unlock()
	if !allowed {
		fail(w, 403, "active project developer required")
		return
	}
	if c.Action == "merge" && !maintainer {
		fail(w, 403, "maintainer role required")
		return
	}
	if c.Action != "link" && m.Remote == nil {
		fail(w, 404, "linked merge request not found")
		return
	}
	token := a.connection(u, p.Repository.Provider)
	if token == "" {
		fail(w, 401, "connect your provider account before reviewing")
		return
	}
	n := c.Number
	if c.Action != "link" {
		n = m.Remote.Number
	}
	fresh, err := a.fetchRemote(r, p.Repository, token, n)
	if err != nil {
		actionError(w, err)
		return
	}
	// Persist refreshed state even when an old browser subsequently submits a stale review.
	err = a.transaction(func(s *State) error {
		if c.Action == "link" {
			for _, existing := range s.MergeRequests {
				if existing.ProjectID == p.ID && existing.Remote != nil && existing.Remote.Number == n {
					return reject(409, "request is already linked")
				}
			}
			fresh.ID, fresh.ProjectID, fresh.AuthorID, fresh.CreatedAt = id(), p.ID, u, now()
			fresh.Approvals = []string{}
			fresh.Comments = []ReviewComment{}
			fresh.Remote.Reviews = []CommitReview{}
			s.MergeRequests = append(s.MergeRequests, fresh)
			m = fresh
			record(s, p.ID, p.OrgID, u, "mr.link", fresh.Title, true)
		} else {
			stored := findMR(s, p.ID, c.ID)
			if stored.Remote.HeadSHA != fresh.Remote.HeadSHA || stored.State != fresh.State {
				record(s, p.ID, p.OrgID, u, "mr.provider.updated", fresh.Title+" @ "+fresh.Remote.HeadSHA+" · "+fresh.State, true)
			}
			syncMR(stored, fresh)
			m = *stored
		}
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	if c.Action == "link" || c.Action == "sync" {
		a.reviewResponse(w, p, m)
		return
	}
	if c.HeadSHA != m.Remote.HeadSHA {
		fail(w, 409, "new commits arrived; refresh the diff and review the current commit")
		return
	}
	if c.Action == "diff" || c.Action == "comment" && c.Path != "" {
		files, e := a.remoteFiles(r, p.Repository, token, n)
		if e != nil {
			actionError(w, e)
			return
		}
		check, e := a.fetchRemote(r, p.Repository, token, n)
		if e != nil {
			actionError(w, e)
			return
		}
		if check.Remote.HeadSHA != m.Remote.HeadSHA || check.Remote.BaseSHA != m.Remote.BaseSHA {
			fail(w, 409, "branches changed while loading the diff; refresh and retry")
			return
		}
		if c.Action == "diff" {
			jsonResponse(w, 200, map[string]any{"files": files, "head_sha": m.Remote.HeadSHA})
			return
		}
		found := false
		for _, f := range files {
			if f.Path == c.Path && !f.Unavailable && c.Line > 0 && diffHasLine(f, c.Line, c.Side) {
				found = true
			}
		}
		if !found {
			fail(w, 400, "select a visible old/new line from the current diff")
			return
		}
	}
	if m.State != "open" {
		fail(w, 409, "merge request is not open")
		return
	}
	if c.Action == "merge" {
		a.executeMerge(w, r, u, p, m, token)
		return
	}
	var identity struct{ ID int64 }
	if c.Action == "review" {
		if c.Decision != "approve" && c.Decision != "request_changes" {
			fail(w, 400, "choose approve or request_changes")
			return
		}
		if e := a.providerGet(r, p.Repository.Provider, token, "/user", &identity); e != nil {
			actionError(w, e)
			return
		}
		if identity.ID <= 0 || identity.ID == m.Remote.AuthorID {
			fail(w, 403, "provider authors cannot review their own request")
			return
		}
		if c.Decision == "request_changes" && strings.TrimSpace(c.Body) == "" {
			fail(w, 400, "explain the requested changes")
			return
		}
	}
	if len(c.Body) > 20000 || c.Action == "comment" && strings.TrimSpace(c.Body) == "" {
		fail(w, 400, "comment required, maximum 20,000 characters")
		return
	}
	if c.Action == "comment" && c.Path == "" && (c.Line != 0 || c.Side != "") {
		fail(w, 400, "general comments cannot specify a line or side")
		return
	}
	err = a.transaction(func(s *State) error {
		stored := findMR(s, p.ID, m.ID)
		if c.Action == "review" {
			stored.Remote.Reviews = append(stored.Remote.Reviews, CommitReview{u, identity.ID, c.HeadSHA, c.Decision, c.Body, now()})
			stored.Approvals = []string{}
		} else {
			stored.Comments = append(stored.Comments, ReviewComment{ID: id(), AuthorID: u, Body: c.Body, Path: c.Path, Line: c.Line, Side: c.Side, CommitSHA: c.HeadSHA, CreatedAt: now()})
		}
		record(s, p.ID, p.OrgID, u, "mr."+c.Action, m.Title+" @ "+c.HeadSHA, true)
		notify(s, p.ID, m.AuthorID, "Review updated: "+m.Title)
		m = *stored
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	a.reviewResponse(w, p, m)
}
func (a *App) reviewResponse(w http.ResponseWriter, p Project, m MergeRequest) {
	a.store.mu.Lock()
	blocks := mergeBlockers(&a.store.state, &p, &m)
	a.store.mu.Unlock()
	jsonResponse(w, 200, map[string]any{"merge_request": m, "blockers": blocks})
}
func (a *App) executeMerge(w http.ResponseWriter, r *http.Request, u string, p Project, m MergeRequest, token string) {
	// Write intent first: a failed database CAS cannot cause a remote side effect.
	err := a.transaction(func(s *State) error {
		stored := findMR(s, p.ID, m.ID)
		if blocks := mergeBlockers(s, &p, stored); len(blocks) > 0 {
			return reject(409, strings.Join(blocks, "; "))
		}
		if stored.Remote.MergeAttempt != "" {
			return reject(409, "previous merge outcome is uncertain; check the provider before attempting another merge")
		}
		stored.Remote.MergeAttempt = m.Remote.HeadSHA
		record(s, p.ID, p.OrgID, u, "mr.merge.requested", m.Title+" @ "+m.Remote.HeadSHA, true)
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	path, _ := remotePath(p.Repository, m.Remote.Number)
	payload := map[string]any{"sha": m.Remote.HeadSHA}
	if p.Repository.Provider == "github" {
		payload["merge_method"] = "merge"
	}
	body, _ := json.Marshal(payload)
	config, _ := provider(p.Repository.Provider)
	req, _ := http.NewRequestWithContext(r.Context(), "PUT", config.APIURL+path+"/merge", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Forge-MVP")
	res, callErr := a.client.Do(req)
	definiteRejection := false
	if callErr == nil {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		definiteRejection = res.StatusCode >= 400 && res.StatusCode < 500
	}
	// Always read the authoritative outcome; never infer a merge from HTTP 200 alone.
	fresh, fetchErr := a.fetchRemote(r, p.Repository, token, m.Remote.Number)
	err = a.transaction(func(s *State) error {
		stored := findMR(s, p.ID, m.ID)
		if fetchErr == nil {
			syncMR(stored, fresh)
		}
		if definiteRejection {
			stored.Remote.MergeAttempt = ""
		}
		if stored.State == "merged" {
			record(s, p.ID, p.OrgID, u, "mr.merged", stored.Title, true)
			notify(s, p.ID, stored.AuthorID, "Merged: "+stored.Title)
		}
		m = *stored
		return nil
	})
	if err != nil {
		fail(w, 503, "provider may have merged, but saving the result failed; refresh before any retry")
		return
	}
	if m.State != "merged" {
		fail(w, 409, "provider did not confirm a merge; refresh its status and check branch protections or permissions")
		return
	}
	a.reviewResponse(w, p, m)
}
