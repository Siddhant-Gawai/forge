package main

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Command struct {
	Kind              string   `json:"kind"`
	ProjectID         string   `json:"project_id"`
	OrgID             string   `json:"org_id"`
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Title             string   `json:"title"`
	Body              string   `json:"body"`
	Description       string   `json:"description"`
	UserID            string   `json:"user_id"`
	Role              string   `json:"role"`
	TeamID            string   `json:"team_id"`
	Members           []string `json:"members"`
	AssigneeID        string   `json:"assignee_id"`
	Labels            []string `json:"labels"`
	MilestoneID       string   `json:"milestone_id"`
	State             string   `json:"state"`
	Color             string   `json:"color"`
	DueDate           string   `json:"due_date"`
	Source            string   `json:"source"`
	Target            string   `json:"target"`
	Path              string   `json:"path"`
	Line              int      `json:"line"`
	CommentID         string   `json:"comment_id"`
	RequiredApprovals int      `json:"required_approvals"`
	RequirePipeline   bool     `json:"require_pipeline"`
	Archived          bool     `json:"archived"`
	Ref               string   `json:"ref"`
	MRID              string   `json:"mr_id"`
	Outcome           string   `json:"outcome"`
}
type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string          { return e.message }
func reject(status int, msg string) error { return apiError{status, msg} }
func actionError(w http.ResponseWriter, err error) {
	var e apiError
	if errors.As(err, &e) {
		fail(w, e.status, e.message)
	} else {
		fail(w, 500, "could not persist operation")
	}
}
func nonempty(s string) bool { return len(strings.TrimSpace(s)) > 0 && len(s) <= 500 }
func (a *App) action(w http.ResponseWriter, r *http.Request) {
	a.reviewMu.Lock()
	defer a.reviewMu.Unlock()
	u := a.require(w, r)
	if u == "" {
		return
	}
	var c Command
	if err := decode(w, r, &c); err != nil {
		fail(w, 400, err.Error())
		return
	}
	var result any
	pipelineID := ""
	err := a.transaction(func(s *State) error {
		if c.Kind == "organization.create" {
			if !nonempty(c.Name) {
				return reject(400, "organization name is required")
			}
			o := Organization{ID: id(), Name: strings.TrimSpace(c.Name), Members: map[string]string{u: "owner"}}
			s.Organizations = append(s.Organizations, o)
			record(s, "", o.ID, u, c.Kind, o.Name, true)
			result = o
			return nil
		}
		if c.Kind == "organization.member" {
			if orgRole(s, c.OrgID, u) < 4 {
				return reject(403, "organization owner required")
			}
			if !hasUser(s, c.UserID) || ranks[c.Role] == 0 {
				return reject(400, "valid user and role required")
			}
			for i := range s.Organizations {
				o := &s.Organizations[i]
				if o.ID == c.OrgID {
					if o.Members[c.UserID] == "owner" && c.Role != "owner" {
						owners := 0
						for _, r := range o.Members {
							if r == "owner" {
								owners++
							}
						}
						if owners < 2 {
							return reject(409, "the organization must retain an owner")
						}
					}
					o.Members[c.UserID] = c.Role
					record(s, "", o.ID, u, c.Kind, c.UserID+" → "+c.Role, true)
					result = o
					return nil
				}
			}
			return reject(404, "organization not found")
		}
		if c.Kind == "team.create" || c.Kind == "team.update" {
			if orgRole(s, c.OrgID, u) < 3 {
				return reject(403, "organization maintainer required")
			}
			if !nonempty(c.Name) {
				return reject(400, "team name is required")
			}
			for _, uid := range c.Members {
				if orgRole(s, c.OrgID, uid) == 0 {
					return reject(400, "team members must belong to the organization")
				}
			}
			t := Team{ID: id(), OrgID: c.OrgID, Name: c.Name, Members: c.Members}
			if c.Kind == "team.update" {
				found := false
				for i := range s.Teams {
					if s.Teams[i].ID == c.ID && s.Teams[i].OrgID == c.OrgID {
						t.ID = c.ID
						s.Teams[i] = t
						found = true
					}
				}
				if !found {
					return reject(404, "team not found")
				}
			} else {
				s.Teams = append(s.Teams, t)
			}
			record(s, "", c.OrgID, u, c.Kind, c.Name, true)
			result = t
			return nil
		}
		if c.Kind == "project.create" {
			if orgRole(s, c.OrgID, u) < 3 {
				return reject(403, "organization maintainer required")
			}
			if !nonempty(c.Name) {
				return reject(400, "project name is required")
			}
			if err := validateTeam(s, c.TeamID, c.OrgID); err != nil {
				return err
			}
			p := Project{ID: id(), OrgID: c.OrgID, Name: c.Name, Description: c.Description, TeamID: c.TeamID, Members: map[string]string{}, RequiredApprovals: 1, RequirePipeline: true, WebhookSecret: id(), Repository: Repository{Provider: "local", DefaultBranch: "main"}}
			s.Projects = append(s.Projects, p)
			record(s, p.ID, p.OrgID, u, c.Kind, p.Name, true)
			result = p
			return nil
		}
		if c.Kind == "notification.read" {
			for i := range s.Notifications {
				n := &s.Notifications[i]
				if n.UserID == u && (c.ID == "" || n.ID == c.ID) {
					n.Read = true
				}
			}
			result = map[string]bool{"ok": true}
			return nil
		}
		p := project(s, c.ProjectID)
		if p == nil || role(s, p, u) == 0 {
			return reject(404, "project not found")
		}
		level := role(s, p, u)
		if level < 2 {
			return reject(403, "developer role required")
		}
		if p.Archived && c.Kind != "project.settings" {
			return reject(409, "project is archived")
		}
		sensitive := false
		detail := c.Title
		switch c.Kind {
		case "project.settings":
			if level < 3 {
				return reject(403, "maintainer role required")
			}
			if !nonempty(c.Name) || c.RequiredApprovals < 0 || c.RequiredApprovals > 10 {
				return reject(400, "name and approval count between 0 and 10 required")
			}
			if err := validateTeam(s, c.TeamID, p.OrgID); err != nil {
				return err
			}
			p.Name = c.Name
			p.Description = c.Description
			p.TeamID = c.TeamID
			p.RequiredApprovals = c.RequiredApprovals
			p.RequirePipeline = c.RequirePipeline
			p.Archived = c.Archived
			sensitive = true
			detail = fmt.Sprintf("%s · approvals=%d · pipeline=%t · archived=%t", p.Name, p.RequiredApprovals, p.RequirePipeline, p.Archived)
			result = p
		case "project.member":
			if level < 3 {
				return reject(403, "maintainer role required")
			}
			if !hasUser(s, c.UserID) || ranks[c.Role] == 0 || c.Role == "owner" {
				return reject(400, "valid user and guest/developer/maintainer role required")
			}
			if p.Members == nil {
				p.Members = map[string]string{}
			}
			p.Members[c.UserID] = c.Role
			sensitive = true
			detail = c.UserID + " → " + c.Role
			result = p
		case "webhook.rotate":
			if level < 3 {
				return reject(403, "maintainer role required")
			}
			p.WebhookSecret = id()
			sensitive = true
			detail = "Webhook secret rotated"
			result = map[string]string{"secret": p.WebhookSecret}
		case "label.create":
			if !nonempty(c.Name) || !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(c.Color) {
				return reject(400, "label name and hex color required")
			}
			for _, l := range s.Labels {
				if l.ProjectID == p.ID && strings.EqualFold(l.Name, c.Name) {
					return reject(409, "label already exists")
				}
			}
			v := Label{ID: id(), ProjectID: p.ID, Name: c.Name, Color: c.Color}
			s.Labels = append(s.Labels, v)
			result = v
			detail = c.Name
		case "milestone.create":
			if !nonempty(c.Title) {
				return reject(400, "milestone title required")
			}
			if c.DueDate != "" {
				if _, err := time.Parse("2006-01-02", c.DueDate); err != nil {
					return reject(400, "due date must be YYYY-MM-DD")
				}
			}
			v := Milestone{ID: id(), ProjectID: p.ID, Title: c.Title, DueDate: c.DueDate, State: "open"}
			s.Milestones = append(s.Milestones, v)
			result = v
		case "milestone.state":
			if c.State != "open" && c.State != "closed" {
				return reject(400, "invalid milestone state")
			}
			found := false
			for i := range s.Milestones {
				v := &s.Milestones[i]
				if v.ID == c.ID && v.ProjectID == p.ID {
					v.State = c.State
					result = v
					detail = v.Title
					found = true
				}
			}
			if !found {
				return reject(404, "milestone not found")
			}
		case "issue.create", "issue.update":
			if !nonempty(c.Title) {
				return reject(400, "issue title required")
			}
			if c.AssigneeID != "" && (!hasUser(s, c.AssigneeID) || role(s, p, c.AssigneeID) == 0) {
				return reject(400, "assignee must have project access")
			}
			for _, lid := range c.Labels {
				found := false
				for _, l := range s.Labels {
					if l.ID == lid && l.ProjectID == p.ID {
						found = true
					}
				}
				if !found {
					return reject(400, "label does not belong to project")
				}
			}
			if c.MilestoneID != "" {
				found := false
				for _, m := range s.Milestones {
					if m.ID == c.MilestoneID && m.ProjectID == p.ID {
						found = true
					}
				}
				if !found {
					return reject(400, "milestone does not belong to project")
				}
			}
			if c.Kind == "issue.create" {
				v := Issue{ID: id(), ProjectID: p.ID, Title: c.Title, Body: c.Body, AuthorID: u, AssigneeID: c.AssigneeID, Labels: c.Labels, MilestoneID: c.MilestoneID, State: "open", CreatedAt: now()}
				s.Issues = append(s.Issues, v)
				result = v
				notify(s, p.ID, c.AssigneeID, "Assigned: "+c.Title)
			} else {
				if c.State != "open" && c.State != "closed" {
					return reject(400, "invalid issue state")
				}
				found := false
				for i := range s.Issues {
					v := &s.Issues[i]
					if v.ID == c.ID && v.ProjectID == p.ID {
						if v.AssigneeID != c.AssigneeID {
							notify(s, p.ID, c.AssigneeID, "Assigned: "+c.Title)
						}
						v.Title = c.Title
						v.Body = c.Body
						v.AssigneeID = c.AssigneeID
						v.Labels = c.Labels
						v.MilestoneID = c.MilestoneID
						v.State = c.State
						result = v
						found = true
					}
				}
				if !found {
					return reject(404, "issue not found")
				}
			}
		case "mr.create":
			if !nonempty(c.Title) || !nonempty(c.Source) || !nonempty(c.Target) || c.Source == c.Target {
				return reject(400, "title and different source/target branches required")
			}
			v := MergeRequest{ID: id(), ProjectID: p.ID, Title: c.Title, Body: c.Body, Source: c.Source, Target: c.Target, AuthorID: u, State: "open", Approvals: []string{}, Comments: []ReviewComment{}, CreatedAt: now()}
			s.MergeRequests = append(s.MergeRequests, v)
			result = v
		case "mr.comment", "mr.resolve", "mr.approve", "mr.merge", "mr.close":
			var mr *MergeRequest
			for i := range s.MergeRequests {
				if s.MergeRequests[i].ID == c.ID && s.MergeRequests[i].ProjectID == p.ID {
					mr = &s.MergeRequests[i]
				}
			}
			if mr == nil {
				return reject(404, "merge request not found")
			}
			if mr.Remote != nil && c.Kind != "mr.resolve" {
				return reject(409, "use the provider-backed review workflow for this request")
			}
			if mr.State != "open" {
				return reject(409, "merge request is not open")
			}
			detail = mr.Title
			switch c.Kind {
			case "mr.comment":
				if strings.TrimSpace(c.Body) == "" || c.Line < 0 || (c.Line > 0 && c.Path == "") {
					return reject(400, "comment body and a valid file/line required")
				}
				mr.Comments = append(mr.Comments, ReviewComment{ID: id(), AuthorID: u, Body: c.Body, Path: c.Path, Line: c.Line, CreatedAt: now()})
				if u != mr.AuthorID {
					notify(s, p.ID, mr.AuthorID, "New review: "+mr.Title)
				}
			case "mr.resolve":
				found := false
				for i := range mr.Comments {
					v := &mr.Comments[i]
					if v.ID == c.CommentID {
						if u != v.AuthorID && level < 3 {
							return reject(403, "comment author or maintainer required")
						}
						v.Resolved = true
						found = true
					}
				}
				if !found {
					return reject(404, "comment not found")
				}
			case "mr.approve":
				if u == mr.AuthorID {
					return reject(403, "authors cannot approve their own merge request")
				}
				if !contains(mr.Approvals, u) {
					mr.Approvals = append(mr.Approvals, u)
					notify(s, p.ID, mr.AuthorID, "Approved: "+mr.Title)
				}
				sensitive = true
			case "mr.close":
				if u != mr.AuthorID && level < 3 {
					return reject(403, "author or maintainer required")
				}
				mr.State = "closed"
			case "mr.merge":
				if level < 3 {
					return reject(403, "maintainer role required")
				}
				approvals := 0
				for _, uid := range mr.Approvals {
					if uid != mr.AuthorID && role(s, p, uid) >= 2 {
						approvals++
					}
				}
				if approvals < p.RequiredApprovals {
					return reject(409, "approval rule has not been satisfied")
				}
				for _, v := range mr.Comments {
					if !v.Resolved {
						return reject(409, "resolve all review discussions before merging")
					}
				}
				if p.RequirePipeline {
					latest := ""
					for _, v := range s.Pipelines {
						if v.MRID == mr.ID && v.ProjectID == p.ID {
							latest = v.State
						}
					}
					if latest != "passed" {
						return reject(409, "latest merge request pipeline must pass")
					}
				}
				mr.State = "merged"
				sensitive = true
				notify(s, p.ID, mr.AuthorID, "Merged: "+mr.Title)
			}
			result = mr
		case "pipeline.create":
			if c.Outcome != "passed" && c.Outcome != "failed" {
				return reject(400, "simulation outcome must be passed or failed")
			}
			if !nonempty(c.Ref) {
				return reject(400, "branch/ref required")
			}
			if c.MRID != "" {
				found := false
				for _, mr := range s.MergeRequests {
					if mr.ID == c.MRID && mr.ProjectID == p.ID && mr.State == "open" && mr.Source == c.Ref {
						found = true
					}
				}
				if !found {
					return reject(400, "pipeline must match an open merge request and its source branch")
				}
			}
			v := Pipeline{ID: id(), ProjectID: p.ID, MRID: c.MRID, Ref: c.Ref, State: "queued", Outcome: c.Outcome, Logs: []string{now() + " Queued · awaiting simulated runner"}, CreatedAt: now()}
			s.Pipelines = append(s.Pipelines, v)
			pipelineID = v.ID
			result = v
			detail = c.Ref
		default:
			return reject(400, "unknown action")
		}
		record(s, p.ID, p.OrgID, u, c.Kind, detail, sensitive)
		return nil
	})
	if err != nil {
		actionError(w, err)
		return
	}
	if pipelineID != "" {
		a.runPipeline(pipelineID)
	}
	jsonResponse(w, 200, result)
}
func validateTeam(s *State, tid, org string) error {
	if tid == "" {
		return nil
	}
	for _, t := range s.Teams {
		if t.ID == tid && t.OrgID == org {
			return nil
		}
	}
	return reject(400, "team must belong to the project organization")
}
func (a *App) runPipeline(pid string) {
	a.workers.Add(1)
	go func() {
		defer a.workers.Done()
		steps := []string{"Prepare isolated simulation workspace", "Build Go packages (simulated)", "Run unit tests (simulated)", "Publish simulation result"}
		for n, step := range steps {
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
			}
			err := a.transaction(func(s *State) error {
				for i := range s.Pipelines {
					p := &s.Pipelines[i]
					if p.ID != pid {
						continue
					}
					if p.State == "passed" || p.State == "failed" {
						return nil
					}
					p.State = "running"
					p.Logs = append(p.Logs, now()+" "+step)
					if n == len(steps)-1 {
						p.State = p.Outcome
						p.Logs = append(p.Logs, now()+" Simulation "+p.State)
						proj := project(s, p.ProjectID)
						if proj != nil {
							record(s, p.ProjectID, proj.OrgID, "runner", "pipeline."+p.State, p.Ref, false)
							for _, u := range s.Users {
								if role(s, proj, u.ID) > 0 {
									notify(s, p.ProjectID, u.ID, "Pipeline "+p.State+" on "+p.Ref)
								}
							}
						}
					}
				}
				return nil
			})
			if err != nil {
				return
			}
		}
	}()
}
func (a *App) recoverPipelines() {
	a.store.mu.Lock()
	var ids []string
	for _, p := range a.store.state.Pipelines {
		if p.State == "queued" || p.State == "running" {
			ids = append(ids, p.ID)
		}
	}
	a.store.mu.Unlock()
	for _, pid := range ids {
		a.runPipeline(pid)
	}
}
