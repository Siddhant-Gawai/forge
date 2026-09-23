package main

import (
	"context"
	"crypto/x509"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Public CA downloaded from Supabase's published certificate endpoint.
//
//go:embed certificates/supabase-ca.crt
var supabaseRootCA []byte

func connectPostgres(raw string) (*sql.DB, error) {
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("DATABASE_URL is not a valid PostgreSQL connection string")
	}
	// Require verified TLS for online databases. Plaintext is only allowed on loopback for tests.
	if config.Host != "localhost" && config.Host != "127.0.0.1" && config.Host != "::1" {
		if config.TLSConfig == nil {
			return nil, errors.New("online DATABASE_URL must enable TLS (sslmode=verify-full)")
		}
		config.TLSConfig.InsecureSkipVerify = false
		config.TLSConfig.ServerName = config.Host
		config.Fallbacks = nil
		root := []byte(nil)
		if strings.HasSuffix(config.Host, ".pooler.supabase.com") || strings.HasSuffix(config.Host, ".supabase.co") {
			root = supabaseRootCA
		}
		if path := os.Getenv("DATABASE_SSL_ROOT_CERT"); path != "" {
			root, err = os.ReadFile(path)
			if err != nil {
				return nil, errors.New("cannot read DATABASE_SSL_ROOT_CERT")
			}
		}
		if len(root) > 0 {
			pool := config.TLSConfig.RootCAs
			if pool == nil {
				pool, _ = x509.SystemCertPool()
				if pool == nil {
					pool = x509.NewCertPool()
				}
			}
			if !pool.AppendCertsFromPEM(root) {
				return nil, errors.New("database root certificate is not valid PEM")
			}
			config.TLSConfig.RootCAs = pool
		}
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.ConnectTimeout = 10 * time.Second
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, safeDatabaseError(err)
	}
	return db, nil
}
func safeDatabaseError(err error) error {
	var cert x509.UnknownAuthorityError
	if errors.As(err, &cert) {
		return errors.New("database TLS certificate is signed by an untrusted authority; download the Supabase SSL root certificate and add sslrootcert to DATABASE_URL")
	}
	var host x509.HostnameError
	if errors.As(err, &host) {
		return errors.New("database TLS certificate does not match the configured hostname")
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return errors.New("database TLS certificate is invalid or expired")
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return fmt.Errorf("PostgreSQL rejected the operation (SQLSTATE %s); check credentials, permissions, and schema", pg.Code)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return errors.New("database hostname could not be resolved; check the Supabase connection host")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("database connection timed out; the direct Supabase host may need IPv6; use the dashboard Session pooler URL")
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return fmt.Errorf("database network operation failed (%s); check the connection host and port", op.Op)
	}
	return errors.New("database connection or operation failed; check network reachability, database password, and TLS certificate settings")
}

//go:embed migrations/001_initial.sql
var initialSchema string

//go:embed migrations/002_reviews.sql
var reviewSchema string

type dbRow map[string]any
type dbTables map[string][]dbRow
type postgresStore struct {
	db       *sql.DB
	revision int64
	baseline dbTables
}

var tableOrder = []string{"users", "accounts", "organizations", "organization_members", "teams", "team_members", "projects", "repositories", "project_members", "labels", "milestones", "issues", "issue_labels", "merge_requests", "approvals", "review_comments", "pipelines", "activity", "audit", "notifications", "deliveries"}

func openPostgres(raw, importPath string) (*Store, error) {
	db, err := connectPostgres(raw)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, safeDatabaseError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(704613824)"); err != nil {
		return nil, safeDatabaseError(err)
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT to_regclass('forge.schema_version') IS NOT NULL").Scan(&exists); err != nil {
		return nil, safeDatabaseError(err)
	}
	if !exists {
		if _, err = tx.ExecContext(ctx, initialSchema); err != nil {
			return nil, safeDatabaseError(err)
		}
	}
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version FROM forge.schema_version WHERE id=1").Scan(&version); err != nil {
		return nil, safeDatabaseError(err)
	}
	if version == 1 {
		if _, err = tx.ExecContext(ctx, reviewSchema); err != nil {
			return nil, safeDatabaseError(err)
		}
		version = 2
	}
	if version != 2 {
		return nil, errors.New("unsupported Forge database schema version")
	}
	if err = tx.Commit(); err != nil {
		return nil, safeDatabaseError(err)
	}
	s := &Store{pg: &postgresStore{db: db}}
	if err = s.loadPostgres(ctx); err != nil {
		return nil, err
	}
	if len(s.state.Users) == 0 {
		// Read an existing local file without creating or modifying it.
		if b, e := os.ReadFile(importPath); e == nil {
			if err = json.Unmarshal(b, &s.state); err != nil {
				return nil, errors.New("local state import is invalid")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, e
		} else {
			s.state = seed()
		}
		if err = verifyChain(s.state.Activity); err != nil {
			return nil, err
		}
		if err = verifyChain(s.state.Audit); err != nil {
			return nil, err
		}
		if err = s.savePostgres(); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (s *Store) loadPostgres(ctx context.Context) error {
	tx, err := s.pg.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return safeDatabaseError(err)
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM forge.schema_version WHERE id=1").Scan(&revision); err != nil {
		return safeDatabaseError(err)
	}
	var parts []string
	for _, table := range tableOrder {
		parts = append(parts, "'"+table+"',COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY position) FROM forge."+table+" t),'[]'::jsonb)")
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT jsonb_build_object("+strings.Join(parts, ",")+")").Scan(&raw); err != nil {
		return safeDatabaseError(err)
	}
	var tables dbTables
	if err = json.Unmarshal(raw, &tables); err != nil {
		return err
	}
	next, err := inflateState(tables)
	if err != nil {
		return err
	}
	if err = verifyChain(next.Activity); err != nil {
		return err
	}
	if err = verifyChain(next.Audit); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return safeDatabaseError(err)
	}
	s.state = next
	s.pg.revision = revision
	s.pg.baseline = flattenState(next)
	return nil
}

func (s *Store) savePostgres() error {
	next := flattenState(s.state)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.pg.db.BeginTx(ctx, nil)
	if err != nil {
		return safeDatabaseError(err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE forge.schema_version SET revision=revision+1 WHERE id=1 AND revision=$1", s.pg.revision)
	if err != nil {
		return safeDatabaseError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return reject(409, "database changed in another process; restart Forge before retrying")
	}
	for _, table := range tableOrder {
		old := map[string]dbRow{}
		for _, row := range s.pg.baseline[table] {
			old[row["id"].(string)] = row
		}
		var changed []dbRow
		for _, row := range next[table] {
			key := row["id"].(string)
			prior, found := old[key]
			if !reflect.DeepEqual(prior, row) {
				if found && (table == "activity" || table == "audit") {
					return errors.New("cannot alter existing events")
				}
				changed = append(changed, row)
			}
			delete(old, key)
		}
		if len(old) > 0 {
			if table != "team_members" && table != "issue_labels" && table != "approvals" && table != "organization_members" && table != "project_members" {
				return errors.New("record deletion is not supported")
			}
			ids := []string{}
			for key := range old {
				ids = append(ids, key)
			}
			raw, _ := json.Marshal(ids)
			if _, err = tx.ExecContext(ctx, "DELETE FROM forge."+table+" WHERE id IN (SELECT jsonb_array_elements_text($1::jsonb))", string(raw)); err != nil {
				return safeDatabaseError(err)
			}
		}
		if len(changed) == 0 {
			continue
		}
		columns := []string{}
		for key := range changed[0] {
			columns = append(columns, key)
		}
		sort.Strings(columns)
		quoted := []string{}
		updates := []string{}
		for _, col := range columns {
			q := `"` + col + `"`
			quoted = append(quoted, q)
			if col != "id" {
				updates = append(updates, q+"=EXCLUDED."+q)
			}
		}
		conflict := "DO UPDATE SET " + strings.Join(updates, ",")
		if table == "activity" || table == "audit" {
			conflict = "DO NOTHING"
		}
		raw, _ := json.Marshal(changed)
		query := "INSERT INTO forge." + table + " (" + strings.Join(quoted, ",") + ") SELECT " + strings.Join(quoted, ",") + " FROM jsonb_populate_recordset(NULL::forge." + table + ",$1::jsonb) ON CONFLICT(id) " + conflict
		if _, err = tx.ExecContext(ctx, query, string(raw)); err != nil {
			return fmt.Errorf("save %s: %w", table, safeDatabaseError(err))
		}
	}
	if err = tx.Commit(); err != nil {
		return safeDatabaseError(err)
	}
	s.pg.revision++
	s.pg.baseline = next
	return nil
}

func flattenState(s State) dbTables {
	b, _ := json.Marshal(s)
	var root map[string]json.RawMessage
	_ = json.Unmarshal(b, &root)
	out := dbTables{}
	add := func(table string, row dbRow) { row["position"] = len(out[table]); out[table] = append(out[table], row) }
	for _, table := range tableOrder {
		out[table] = []dbRow{}
		if table == "deliveries" {
			continue
		}
		var rows []dbRow
		_ = json.Unmarshal(root[table], &rows)
		for _, row := range rows {
			add(table, row)
		}
	}
	for _, o := range s.Organizations {
		for _, row := range out["organizations"] {
			if row["id"] == o.ID {
				delete(row, "members")
			}
		}
		keys := sortedKeys(o.Members)
		for _, uid := range keys {
			add("organization_members", dbRow{"id": o.ID + ":" + uid, "org_id": o.ID, "user_id": uid, "role": o.Members[uid]})
		}
	}
	for _, t := range s.Teams {
		for _, row := range out["teams"] {
			if row["id"] == t.ID {
				delete(row, "members")
			}
		}
		for _, uid := range t.Members {
			add("team_members", dbRow{"id": t.ID + ":" + uid, "team_id": t.ID, "org_id": t.OrgID, "user_id": uid})
		}
	}
	for _, p := range s.Projects {
		for _, row := range out["projects"] {
			if row["id"] == p.ID {
				delete(row, "members")
				delete(row, "repository")
				row["webhook_secret"] = p.WebhookSecret
			}
		}
		b, _ := json.Marshal(p.Repository)
		var repo dbRow
		json.Unmarshal(b, &repo)
		repo["id"] = p.ID
		add("repositories", repo)
		for _, uid := range sortedKeys(p.Members) {
			add("project_members", dbRow{"id": p.ID + ":" + uid, "project_id": p.ID, "user_id": uid, "role": p.Members[uid]})
		}
	}
	for _, i := range s.Issues {
		for _, row := range out["issues"] {
			if row["id"] == i.ID {
				delete(row, "labels")
			}
		}
		for _, lid := range i.Labels {
			add("issue_labels", dbRow{"id": i.ID + ":" + lid, "issue_id": i.ID, "project_id": i.ProjectID, "label_id": lid})
		}
	}
	for _, m := range s.MergeRequests {
		for _, row := range out["merge_requests"] {
			if row["id"] == m.ID {
				delete(row, "approvals")
				delete(row, "comments")
			}
		}
		for _, uid := range m.Approvals {
			add("approvals", dbRow{"id": m.ID + ":" + uid, "mr_id": m.ID, "user_id": uid})
		}
		for _, c := range m.Comments {
			b, _ := json.Marshal(c)
			var row dbRow
			json.Unmarshal(b, &row)
			row["mr_id"] = m.ID
			add("review_comments", row)
		}
	}
	for _, key := range sortedKeys(s.Deliveries) {
		if s.Deliveries[key] {
			add("deliveries", dbRow{"id": key})
		}
	}
	for _, table := range tableOrder {
		for _, row := range out[table] {
			for _, key := range []string{"team_id", "assignee_id", "milestone_id", "mr_id", "project_id"} {
				if row[key] == "" {
					row[key] = nil
				}
			}
			if table == "pipelines" && row["logs"] == nil {
				row["logs"] = []string{}
			}
		}
	}
	return out
}
func sortedKeys[V any](m map[string]V) []string {
	keys := []string{}
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func inflateState(t dbTables) (State, error) {
	root := map[string]any{}
	for _, table := range tableOrder {
		root[table] = t[table]
	}
	lookup := func(table, key string) dbRow {
		for _, r := range t[table] {
			if r["id"] == key {
				return r
			}
		}
		return nil
	}
	for _, r := range t["organizations"] {
		r["members"] = map[string]string{}
	}
	for _, r := range t["organization_members"] {
		if p := lookup("organizations", r["org_id"].(string)); p != nil {
			p["members"].(map[string]string)[r["user_id"].(string)] = r["role"].(string)
		}
	}
	for _, r := range t["teams"] {
		r["members"] = []string{}
	}
	for _, r := range t["team_members"] {
		if p := lookup("teams", r["team_id"].(string)); p != nil {
			p["members"] = append(p["members"].([]string), r["user_id"].(string))
		}
	}
	for _, r := range t["projects"] {
		r["members"] = map[string]string{}
	}
	for _, r := range t["project_members"] {
		if p := lookup("projects", r["project_id"].(string)); p != nil {
			p["members"].(map[string]string)[r["user_id"].(string)] = r["role"].(string)
		}
	}
	for _, r := range t["repositories"] {
		if p := lookup("projects", r["id"].(string)); p != nil {
			p["repository"] = r
		}
	}
	for _, r := range t["issues"] {
		r["labels"] = []string{}
	}
	for _, r := range t["issue_labels"] {
		if p := lookup("issues", r["issue_id"].(string)); p != nil {
			p["labels"] = append(p["labels"].([]string), r["label_id"].(string))
		}
	}
	for _, r := range t["merge_requests"] {
		r["approvals"] = []string{}
		r["comments"] = []dbRow{}
	}
	for _, r := range t["approvals"] {
		if p := lookup("merge_requests", r["mr_id"].(string)); p != nil {
			p["approvals"] = append(p["approvals"].([]string), r["user_id"].(string))
		}
	}
	for _, r := range t["review_comments"] {
		if p := lookup("merge_requests", r["mr_id"].(string)); p != nil {
			p["comments"] = append(p["comments"].([]dbRow), r)
		}
	}
	deliveries := map[string]bool{}
	for _, r := range t["deliveries"] {
		deliveries[r["id"].(string)] = true
	}
	root["deliveries"] = deliveries
	b, err := json.Marshal(root)
	if err != nil {
		return State{}, err
	}
	var s State
	err = json.Unmarshal(b, &s)
	return s, err
}
