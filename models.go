package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"-"`
}
type Account struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
	CreatedAt    string `json:"created_at"`
}
type Organization struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"`
}
type Team struct {
	ID      string   `json:"id"`
	OrgID   string   `json:"org_id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}
type Project struct {
	ID                string            `json:"id"`
	OrgID             string            `json:"org_id"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	TeamID            string            `json:"team_id"`
	Members           map[string]string `json:"members"`
	Repository        Repository        `json:"repository"`
	RequiredApprovals int               `json:"required_approvals"`
	RequirePipeline   bool              `json:"require_pipeline"`
	Archived          bool              `json:"archived"`
	WebhookSecret     string            `json:"webhook_secret,omitempty"`
}
type Repository struct {
	Provider      string `json:"provider"`
	ExternalID    string `json:"external_id"`
	FullName      string `json:"full_name"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
}
type Label struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
}
type Milestone struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	DueDate   string `json:"due_date"`
	State     string `json:"state"`
}
type Issue struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	AuthorID    string   `json:"author_id"`
	AssigneeID  string   `json:"assignee_id"`
	Labels      []string `json:"labels"`
	MilestoneID string   `json:"milestone_id"`
	CreatedAt   string   `json:"created_at"`
}
type ReviewComment struct {
	CommitSHA string `json:"commit_sha"`
	Side      string `json:"side"`
	ID        string `json:"id"`
	AuthorID  string `json:"author_id"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Resolved  bool   `json:"resolved"`
	CreatedAt string `json:"created_at"`
}
type MergeRequest struct {
	Remote    *RemoteRequest  `json:"remote"`
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Source    string          `json:"source"`
	Target    string          `json:"target"`
	AuthorID  string          `json:"author_id"`
	State     string          `json:"state"`
	Approvals []string        `json:"approvals"`
	Comments  []ReviewComment `json:"comments"`
	CreatedAt string          `json:"created_at"`
}
type Pipeline struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"project_id"`
	MRID      string   `json:"mr_id"`
	Ref       string   `json:"ref"`
	State     string   `json:"state"`
	Outcome   string   `json:"outcome"`
	Logs      []string `json:"logs"`
	CreatedAt string   `json:"created_at"`
}
type Event struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	OrgID        string `json:"org_id"`
	ActorID      string `json:"actor_id"`
	Action       string `json:"action"`
	Detail       string `json:"detail"`
	CreatedAt    string `json:"created_at"`
	PreviousHash string `json:"previous_hash"`
	Hash         string `json:"hash"`
}
type Notification struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ProjectID string `json:"project_id"`
	Message   string `json:"message"`
	Read      bool   `json:"read"`
	CreatedAt string `json:"created_at"`
}
type State struct {
	Accounts      []Account       `json:"accounts,omitempty"`
	Users         []User          `json:"users"`
	Organizations []Organization  `json:"organizations"`
	Teams         []Team          `json:"teams"`
	Projects      []Project       `json:"projects"`
	Labels        []Label         `json:"labels"`
	Milestones    []Milestone     `json:"milestones"`
	Issues        []Issue         `json:"issues"`
	MergeRequests []MergeRequest  `json:"merge_requests"`
	Pipelines     []Pipeline      `json:"pipelines"`
	Activity      []Event         `json:"activity"`
	Audit         []Event         `json:"audit"`
	Notifications []Notification  `json:"notifications"`
	Deliveries    map[string]bool `json:"deliveries"`
}
type Store struct {
	pg    *postgresStore
	mu    sync.Mutex
	path  string
	state State
}

func id() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func openStore(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
		if err = verifyChain(s.state.Activity); err != nil {
			return nil, err
		}
		if err = verifyChain(s.state.Audit); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	s.state = seed()
	return s, s.save()
}
func (s *Store) save() error {
	if s.pg != nil {
		return s.savePostgres()
	}
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, s.path)
}
func seed() State {
	return State{
		Users:         []User{{ID: "alex", Name: "Alex Morgan"}, {ID: "sam", Name: "Sam Rivera"}, {ID: "jordan", Name: "Jordan Lee"}},
		Organizations: []Organization{{ID: "acme", Name: "Acme Engineering", Members: map[string]string{"alex": "owner", "sam": "maintainer", "jordan": "developer"}}},
		Teams:         []Team{{ID: "platform", OrgID: "acme", Name: "Platform", Members: []string{"alex", "sam", "jordan"}}},
		Projects:      []Project{{ID: "orbit", OrgID: "acme", TeamID: "platform", Name: "orbit-api", Description: "The platform that connects everything. Core services, thoughtfully built.", Members: map[string]string{}, RequiredApprovals: 1, RequirePipeline: true, WebhookSecret: id(), Repository: Repository{Provider: "local", FullName: "acme/orbit-api", DefaultBranch: "main"}}},
		Labels:        []Label{{ID: "feature", ProjectID: "orbit", Name: "feature", Color: "#7c5cfc"}, {ID: "bug", ProjectID: "orbit", Name: "bug", Color: "#e05b74"}, {ID: "backend", ProjectID: "orbit", Name: "backend", Color: "#349b8f"}},
		Milestones:    []Milestone{{ID: "v1", ProjectID: "orbit", Title: "v1.0 · Foundation", DueDate: time.Now().AddDate(0, 1, 0).Format("2006-01-02"), State: "open"}},
		Issues:        []Issue{{ID: "101", ProjectID: "orbit", Title: "Add cursor-based pagination to the projects API", Body: "Support stable cursors and a configurable page size.", State: "open", AuthorID: "alex", AssigneeID: "jordan", Labels: []string{"feature", "backend"}, MilestoneID: "v1", CreatedAt: now()}, {ID: "102", ProjectID: "orbit", Title: "Handle expired sessions gracefully", Body: "Show a helpful message when a session expires.", State: "open", AuthorID: "sam", AssigneeID: "sam", Labels: []string{"bug"}, MilestoneID: "v1", CreatedAt: now()}, {ID: "103", ProjectID: "orbit", Title: "Document the local development workflow", Body: "Make the first contribution a little easier.", State: "closed", AuthorID: "alex", AssigneeID: "alex", Labels: []string{}, CreatedAt: now()}},
		MergeRequests: []MergeRequest{{ID: "42", ProjectID: "orbit", Title: "Add project access middleware", Body: "Centralize project permissions and validate every mutation.", Source: "feature/project-access", Target: "main", AuthorID: "jordan", State: "open", Approvals: []string{}, Comments: []ReviewComment{}, CreatedAt: now()}},
		Pipelines:     []Pipeline{{ID: "210", ProjectID: "orbit", MRID: "42", Ref: "feature/project-access", State: "passed", Outcome: "passed", Logs: []string{"✓ Prepare workspace", "✓ Build Go packages", "✓ Run test suite", "✓ Simulation completed"}, CreatedAt: now()}},
		Activity:      []Event{}, Audit: []Event{}, Notifications: []Notification{}, Deliveries: map[string]bool{},
	}
}
