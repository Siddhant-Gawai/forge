CREATE SCHEMA IF NOT EXISTS forge;
REVOKE ALL ON SCHEMA forge FROM PUBLIC;

CREATE TABLE forge.schema_version (id integer PRIMARY KEY CHECK (id=1), version integer NOT NULL, revision bigint NOT NULL DEFAULT 0);
INSERT INTO forge.schema_version(id,version) VALUES(1,1);

CREATE TABLE forge.users (id text PRIMARY KEY, name text NOT NULL, position bigint NOT NULL);
CREATE TABLE forge.accounts (
 id text PRIMARY KEY REFERENCES forge.users(id), email text NOT NULL UNIQUE CHECK(email=lower(email)),
 password_hash text NOT NULL, created_at text NOT NULL, position bigint NOT NULL
);
CREATE TABLE forge.organizations (id text PRIMARY KEY, name text NOT NULL, position bigint NOT NULL);
CREATE TABLE forge.organization_members (
 id text PRIMARY KEY, org_id text NOT NULL REFERENCES forge.organizations(id), user_id text NOT NULL REFERENCES forge.users(id),
 role text NOT NULL CHECK(role IN ('guest','developer','maintainer','owner')), position bigint NOT NULL, UNIQUE(org_id,user_id)
);
CREATE TABLE forge.teams (id text PRIMARY KEY, org_id text NOT NULL REFERENCES forge.organizations(id), name text NOT NULL, position bigint NOT NULL, UNIQUE(id,org_id));
CREATE TABLE forge.team_members (
 id text PRIMARY KEY, team_id text NOT NULL, org_id text NOT NULL, user_id text NOT NULL, position bigint NOT NULL,
 FOREIGN KEY(team_id,org_id) REFERENCES forge.teams(id,org_id),
 FOREIGN KEY(org_id,user_id) REFERENCES forge.organization_members(org_id,user_id), UNIQUE(team_id,user_id)
);
CREATE TABLE forge.projects (
 id text PRIMARY KEY, org_id text NOT NULL REFERENCES forge.organizations(id), name text NOT NULL, description text NOT NULL,
 team_id text, required_approvals integer NOT NULL CHECK(required_approvals BETWEEN 0 AND 10),
 require_pipeline boolean NOT NULL, archived boolean NOT NULL, webhook_secret text NOT NULL, position bigint NOT NULL,
 FOREIGN KEY(team_id,org_id) REFERENCES forge.teams(id,org_id)
);
CREATE TABLE forge.repositories (
 id text PRIMARY KEY REFERENCES forge.projects(id), provider text NOT NULL, external_id text NOT NULL,
 full_name text NOT NULL, url text NOT NULL, default_branch text NOT NULL, position bigint NOT NULL
);
CREATE TABLE forge.project_members (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), user_id text NOT NULL REFERENCES forge.users(id),
 role text NOT NULL CHECK(role IN ('guest','developer','maintainer')), position bigint NOT NULL, UNIQUE(project_id,user_id)
);
CREATE TABLE forge.labels (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), name text NOT NULL, color text NOT NULL,
 position bigint NOT NULL, UNIQUE(id,project_id)
);
CREATE UNIQUE INDEX label_name_unique ON forge.labels(project_id,lower(name));
CREATE TABLE forge.milestones (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), title text NOT NULL, due_date text NOT NULL,
 state text NOT NULL CHECK(state IN ('open','closed')), position bigint NOT NULL, UNIQUE(id,project_id)
);
CREATE TABLE forge.issues (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), title text NOT NULL, body text NOT NULL,
 state text NOT NULL CHECK(state IN ('open','closed')), author_id text NOT NULL REFERENCES forge.users(id),
 assignee_id text REFERENCES forge.users(id), milestone_id text, created_at text NOT NULL, position bigint NOT NULL,
 FOREIGN KEY(milestone_id,project_id) REFERENCES forge.milestones(id,project_id), UNIQUE(id,project_id)
);
CREATE TABLE forge.issue_labels (
 id text PRIMARY KEY, issue_id text NOT NULL, label_id text NOT NULL, project_id text NOT NULL, position bigint NOT NULL,
 FOREIGN KEY(issue_id,project_id) REFERENCES forge.issues(id,project_id),
 FOREIGN KEY(label_id,project_id) REFERENCES forge.labels(id,project_id), UNIQUE(issue_id,label_id)
);
CREATE TABLE forge.merge_requests (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), title text NOT NULL, body text NOT NULL,
 source text NOT NULL, target text NOT NULL CHECK(source<>target), author_id text NOT NULL REFERENCES forge.users(id),
 state text NOT NULL CHECK(state IN ('open','closed','merged')), created_at text NOT NULL, position bigint NOT NULL, UNIQUE(id,project_id)
);
CREATE TABLE forge.approvals (
 id text PRIMARY KEY, mr_id text NOT NULL REFERENCES forge.merge_requests(id), user_id text NOT NULL REFERENCES forge.users(id),
 position bigint NOT NULL, UNIQUE(mr_id,user_id)
);
CREATE TABLE forge.review_comments (
 id text PRIMARY KEY, mr_id text NOT NULL REFERENCES forge.merge_requests(id), author_id text NOT NULL REFERENCES forge.users(id),
 body text NOT NULL, path text NOT NULL, line integer NOT NULL CHECK(line>=0), resolved boolean NOT NULL,
 created_at text NOT NULL, position bigint NOT NULL
);
CREATE TABLE forge.pipelines (
 id text PRIMARY KEY, project_id text NOT NULL REFERENCES forge.projects(id), mr_id text, ref text NOT NULL,
 state text NOT NULL CHECK(state IN ('queued','running','passed','failed')), outcome text NOT NULL CHECK(outcome IN ('passed','failed')),
 logs jsonb NOT NULL CHECK(jsonb_typeof(logs)='array'), created_at text NOT NULL, position bigint NOT NULL,
 FOREIGN KEY(mr_id,project_id) REFERENCES forge.merge_requests(id,project_id)
);
CREATE TABLE forge.activity (
 id text PRIMARY KEY, project_id text REFERENCES forge.projects(id), org_id text NOT NULL REFERENCES forge.organizations(id),
 actor_id text NOT NULL, action text NOT NULL, detail text NOT NULL, created_at text NOT NULL,
 previous_hash text NOT NULL, hash text NOT NULL UNIQUE, position bigint NOT NULL UNIQUE
);
CREATE TABLE forge.audit (LIKE forge.activity INCLUDING ALL);
ALTER TABLE forge.audit ADD FOREIGN KEY(project_id) REFERENCES forge.projects(id);
ALTER TABLE forge.audit ADD FOREIGN KEY(org_id) REFERENCES forge.organizations(id);
CREATE TABLE forge.notifications (
 id text PRIMARY KEY, user_id text NOT NULL REFERENCES forge.users(id), project_id text NOT NULL REFERENCES forge.projects(id),
 message text NOT NULL, read boolean NOT NULL, created_at text NOT NULL, position bigint NOT NULL
);
CREATE TABLE forge.deliveries (id text PRIMARY KEY, position bigint NOT NULL);
CREATE TABLE forge.sessions (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES forge.users(id), expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_expiry ON forge.sessions(expires_at);
CREATE INDEX issues_project_state ON forge.issues(project_id,state);
CREATE INDEX merge_requests_project_state ON forge.merge_requests(project_id,state);
CREATE INDEX pipelines_project ON forge.pipelines(project_id,position DESC);
CREATE INDEX activity_project ON forge.activity(project_id,position DESC);
CREATE INDEX audit_project ON forge.audit(project_id,position DESC);
CREATE INDEX notifications_user ON forge.notifications(user_id,read);

CREATE FUNCTION forge.reject_event_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'Activity and audit records are append-only'; END;
$$;
CREATE TRIGGER activity_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON forge.activity FOR EACH STATEMENT EXECUTE FUNCTION forge.reject_event_mutation();
CREATE TRIGGER audit_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON forge.audit FOR EACH STATEMENT EXECUTE FUNCTION forge.reject_event_mutation();

-- Application tables live outside the public Data API schema. Browser roles get no access.
REVOKE ALL ON ALL TABLES IN SCHEMA forge FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA forge FROM PUBLIC;
DO $$ DECLARE r text; t record; BEGIN
 FOR r IN SELECT rolname FROM pg_roles WHERE rolname IN ('anon','authenticated') LOOP
  EXECUTE format('REVOKE ALL ON SCHEMA forge FROM %I',r);
  EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA forge FROM %I',r);
 END LOOP;
 FOR t IN SELECT tablename FROM pg_tables WHERE schemaname='forge' LOOP
  EXECUTE format('ALTER TABLE forge.%I ENABLE ROW LEVEL SECURITY',t.tablename);
 END LOOP;
END $$;
