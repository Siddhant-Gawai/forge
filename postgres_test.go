package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfiguredDatabaseConnection(t *testing.T) {
	if os.Getenv("FORGE_TEST_DB") != "1" {
		t.Skip("set FORGE_TEST_DB=1 to verify the configured database")
	}
	loadDotEnv()
	db, err := connectPostgres(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatal("database read check failed")
	}
	t.Log("Configured PostgreSQL connection and read query succeeded")
}

func TestConfiguredMigration(t *testing.T) {
	if os.Getenv("FORGE_MIGRATE_VERIFY") != "1" {
		t.Skip("set FORGE_MIGRATE_VERIFY=1 to apply the configured migration and verify it")
	}
	if err := loadDotEnv(); err != nil {
		t.Fatal(err)
	}
	s, err := openPostgres(os.Getenv("DATABASE_URL"), env("FORGE_DATA", "data/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.pg.db.Close()
	t.Logf("PostgreSQL active: %d users, %d organizations, %d projects, %d issues, %d merge requests, %d pipelines, %d activity records", len(s.state.Users), len(s.state.Organizations), len(s.state.Projects), len(s.state.Issues), len(s.state.MergeRequests), len(s.state.Pipelines), len(s.state.Activity))
	if err := s.savePostgres(); err != nil {
		t.Fatal("unchanged-state persistence failed:", err)
	}
	reopened, err := openPostgres(os.Getenv("DATABASE_URL"), "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.pg.db.Close()
	if !reflect.DeepEqual(flattenState(s.state), flattenState(reopened.state)) {
		t.Fatal("database reload changed domain state")
	}
	// Check writes in a transaction that is always rolled back; no verification records remain.
	tx, err := s.pg.db.Begin()
	if err != nil {
		t.Fatal("could not begin verification transaction")
	}
	defer tx.Rollback()
	key := "verify-" + id()
	stmts := []string{
		"INSERT INTO forge.users(id,name,position) VALUES($1,'Verification user',-1)",
		"INSERT INTO forge.accounts(id,email,password_hash,created_at,position) VALUES($1,$1||'@example.invalid','verification-only', 'now',-1)",
		"INSERT INTO forge.organizations(id,name,position) VALUES($1,'Verification workspace',-1)",
		"INSERT INTO forge.organization_members(id,org_id,user_id,role,position) VALUES($1,$1,$1,'owner',-1)",
		"INSERT INTO forge.projects(id,org_id,name,description,required_approvals,require_pipeline,archived,webhook_secret,position) VALUES($1,$1,'Verification project','',1,true,false,'verification',-1)",
		"INSERT INTO forge.issues(id,project_id,title,body,state,author_id,created_at,position) VALUES($1,$1,'Verification issue','','open',$1,'now',-1)",
		`INSERT INTO forge.merge_requests(id,project_id,title,body,source,target,author_id,state,created_at,position,remote) VALUES($1,$1,'Verification request','','feature','main',$1,'open','now',-1,'{"number":1,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reviews":[]}'::jsonb)`,
		"INSERT INTO forge.review_comments(id,mr_id,author_id,body,path,line,resolved,created_at,position,commit_sha,side) VALUES($1,$1,$1,'Review','main.go',1,false,'now',-1,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','new')",
		"INSERT INTO forge.sessions(token_hash,user_id,expires_at) VALUES($1,$1,now()+interval '1 hour')",
	}
	for _, stmt := range stmts {
		if _, err = tx.Exec(stmt, key); err != nil {
			t.Fatal(safeDatabaseError(err))
		}
	}
	var title string
	if err = tx.QueryRow("SELECT title FROM forge.issues WHERE id=$1", key).Scan(&title); err != nil || title != "Verification issue" {
		t.Fatal("write/read verification failed")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal("verification rollback failed")
	}
	var count int
	if err = s.pg.db.QueryRow("SELECT count(*) FROM forge.users WHERE id=$1", key).Scan(&count); err != nil || count != 0 {
		t.Fatal("verification records were not rolled back")
	}
	for _, table := range []string{"activity", "audit"} {
		probe, e := s.pg.db.Begin()
		if e != nil {
			t.Fatal("could not begin event protection check")
		}
		_, e = probe.Exec("UPDATE forge." + table + " SET detail=detail WHERE false")
		probe.Rollback()
		if e == nil {
			t.Fatal("immutable event trigger missing on", table)
		}
	}
	// A session token is stored only as a digest and remains valid after reopening the store.
	if len(s.state.Users) > 0 {
		a := newApp(s, false, "http://localhost")
		defer a.cancel()
		w := httptest.NewRecorder()
		uid := s.state.Users[0].ID
		if err = a.startSession(w, uid); err != nil {
			t.Fatal(err)
		}
		cookie := w.Result().Cookies()[0]
		defer s.pg.db.Exec("DELETE FROM forge.sessions WHERE token_hash=$1", tokenHash(cookie.Value))
		app2 := newApp(reopened, false, "http://localhost")
		defer app2.cancel()
		r := httptest.NewRequest("GET", "/api/state", nil)
		r.AddCookie(cookie)
		if app2.user(r) != uid {
			t.Fatal("database session did not survive reopening")
		}
	}
	t.Log("Migration, reload, transactional writes, rollback, immutable event triggers, and persistent sessions verified")
}

func TestRelationalStateRoundTrip(t *testing.T) {
	s := seed()
	record(&s, "orbit", "acme", "alex", "test.created", "Roundtrip", true)
	s.Accounts = []Account{{ID: "alex", Email: "test@example.com", PasswordHash: "hash", CreatedAt: now()}}
	s.Notifications = []Notification{{ID: "note", UserID: "alex", ProjectID: "orbit", Message: "hello", CreatedAt: now()}}
	s.MergeRequests[0].Approvals = []string{"sam"}
	s.MergeRequests[0].Comments = []ReviewComment{{ID: "comment", AuthorID: "sam", Body: "Review", Path: "main.go", Line: 10, CreatedAt: now()}}
	s.Deliveries["github:orbit:delivery-1"] = true
	want := flattenState(s)
	b, _ := json.Marshal(want)
	var tables dbTables
	json.Unmarshal(b, &tables)
	got, err := inflateState(tables)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flattenState(got), want) {
		t.Fatal("relational roundtrip lost state")
	}
	if err = verifyChain(got.Activity); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresPersistence(t *testing.T) {
	// Only a caller-provided, local disposable database is allowed for this mutating test.
	raw := os.Getenv("FORGE_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set FORGE_TEST_DATABASE_URL to a disposable local PostgreSQL database")
	}
	if !strings.HasPrefix(raw, "postgresql://postgres@127.0.0.1:55432/") {
		t.Fatal("integration test requires the disposable local test server on port 55432")
	}
	s, err := openPostgres(raw, filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.pg.db.Close()
	s.pg.db.SetMaxOpenConns(1)
	a := newApp(s, false, "http://localhost")
	defer a.cancel()
	body := map[string]string{"name": "Database Test", "email": "db-" + id() + "@example.com", "password": "test-password-12345"}
	signup := request(a, "POST", "/api/signup", "", body)
	if signup.Code != 201 {
		t.Fatal(signup.Body)
	}
	cookie := signup.Result().Cookies()[0]
	var signed map[string]string
	json.Unmarshal(signup.Body.Bytes(), &signed)
	uid := signed["user_id"]
	orgID := ""
	for _, o := range s.state.Organizations {
		if o.Members[uid] == "owner" {
			orgID = o.ID
		}
	}
	req := func(app *App, path string, c any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(c)
		method := "POST"
		if c == nil {
			method = "GET"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		r.Header.Set("X-Requested-With", "forge")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	w := req(a, "/api/action", Command{Kind: "project.create", OrgID: orgID, Name: "Persisted project"})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var p Project
	json.Unmarshal(w.Body.Bytes(), &p)
	w = req(a, "/api/action", Command{Kind: "issue.create", ProjectID: p.ID, Title: "Survive restart", AssigneeID: uid})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	reopened, err := openPostgres(raw, "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.pg.db.Close()
	reopened.pg.db.SetMaxOpenConns(1)
	app2 := newApp(reopened, false, "http://localhost")
	defer app2.cancel()
	w = req(app2, "/api/state", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Survive restart") {
		t.Fatal("state/session did not survive reopen", w.Body)
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), body["email"]) {
		t.Fatal("credentials exposed")
	}
	// Verify database constraints, not just application validation.
	if _, err = s.pg.db.Exec("INSERT INTO forge.issues(id,project_id,title,body,state,author_id,created_at,position) VALUES('bad','missing','x','','open','alex','now',999)"); err == nil {
		t.Fatal("foreign key not enforced")
	}
	for _, table := range []string{"activity", "audit"} {
		if _, err = s.pg.db.Exec("UPDATE forge." + table + " SET detail='tampered'"); err == nil {
			t.Fatal("event mutation allowed")
		}
		if _, err = s.pg.db.Exec("TRUNCATE forge." + table); err == nil {
			t.Fatal("event truncation allowed")
		}
	}
	// An old snapshot must not overwrite newer records.
	w = req(app2, "/api/action", Command{Kind: "issue.create", ProjectID: p.ID, Title: "Newer record"})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = req(a, "/api/action", Command{Kind: "issue.create", ProjectID: p.ID, Title: "Stale write"})
	if w.Code != 409 {
		t.Fatal("stale write accepted", w.Code, w.Body)
	}
	w = req(app2, "/api/logout", map[string]string{})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = req(a, "/api/state", nil)
	if w.Code != 401 {
		t.Fatal("logout did not revoke persistent session")
	}
	// Verify an unchanged state can be re-saved despite immutable event triggers.
	if err = reopened.savePostgres(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var count int
	if err = reopened.pg.db.QueryRowContext(ctx, "SELECT count(*) FROM forge.issues WHERE title='Stale write'").Scan(&count); err != nil || count != 0 {
		t.Fatal("stale transaction was not rolled back")
	}
}
