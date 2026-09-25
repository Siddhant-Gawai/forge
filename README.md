# Forge

A runnable GitLab-inspired collaboration MVP written in Go, with an embedded responsive web UI, email/password accounts, a personal dashboard, and PostgreSQL storage. The frontend requires no npm build. Go dependencies include the PostgreSQL driver and bcrypt.

## Run

Requires Go 1.23 or newer.

```powershell
go run .
```

On macOS/Linux:

```sh
go run .
```

Open [localhost:8080](http://127.0.0.1:8080). Choose **Sign up** to create an account and private workspace, then create a project from the dashboard. `/login`, `/signup`, and `/dashboard` are directly accessible routes. Accounts use normalized unique emails and bcrypt password hashes. Sessions use HttpOnly, SameSite cookies and expire after 12 hours. HTTPS enables Secure cookies. Login and signup share a per-IP rate limit. Email confirmation and password recovery are not implemented.

The server loads `.env` automatically; explicit environment variables take precedence. Set `DATABASE_URL` to use Supabase/PostgreSQL. Without it, the app uses `data/state.json` for local development. Database failures stop startup; there is no silent fallback to the local file. Set `FORGE_DEMO=1` only to enable the three sample identities (Alex, Sam, Jordan). Normal signup never gives access to the sample organization's data. An optional `FORGE_ADMIN_TOKEN` retains API access as Alex. OAuth imports connect providers to an existing Forge session; they do not log users into Forge.

## Supabase migration

1. Put the exact **Connect → Session pooler** PostgreSQL URI, with the database password, in `.env` as `DATABASE_URL`. URL-encode reserved password characters. The direct host often requires IPv6.
2. Start `go run .`. The server validates TLS, applies pending migrations (`001_initial.sql`, `002_reviews.sql`) in a transaction, and opens the database store.
3. On an empty Forge database, the server imports the existing `FORGE_DATA` JSON file, preserving identifiers, membership, issues, reviews, pipelines, notifications, webhook deduplication, and event hashes. The file is preserved. Later starts load the database and do not re-import it.
4. In Supabase's table editor, select the **forge** schema. It contains normal relational tables with foreign keys and unique constraints, plus separate memberships, issue labels, repositories, approvals, and review comments. Pipeline logs use a JSONB array.

All app tables are in a private schema with browser-role grants revoked and RLS enabled. The Go server connects using the database owner/migration role and enforces project permissions. No Supabase secret API key is required. Supabase Auth/JWKS is not used by the Go email/password implementation.

The connection verifies the server hostname and certificate. Supabase's public root CA is bundled in `certificates/supabase-ca.crt` and trusted only for Supabase database connections. If a custom CA is required, point `DATABASE_SSL_ROOT_CERT` or the URI's `sslrootcert` parameter at the provider root certificate. TLS cannot be disabled for an online database. Connection errors omit credentials.

PostgreSQL sessions store only a SHA-256 token digest, survive restart, and are deleted on logout. Account hashes are never included in API snapshots. New accounts can see only their own workspace and shared project collaborators.

## Included workflows

| Area | Behavior |
| --- | --- |
| Organizations | Create organizations; assign organization roles; preserve at least one owner |
| Teams & projects | Create/edit teams, assign members, create projects, assign a team, archive/unarchive projects |
| Roles | Guest reads; developer manages work; maintainer manages project settings/access and merges; owner manages organization membership |
| Repository import | GitHub and GitLab.com authorization-code OAuth with state validation and PKCE; paginated repository selection; server-verified metadata import |
| Issues | Create/edit/close/reopen; search titles, descriptions, and IDs; combine status, assignee, label, and milestone filters; sort by date or title; keep filters per project during the session |
| Milestones | Due dates, completion progress, close/reopen, open the milestone's issue list |
| Merge requests | Link existing GitHub/GitLab requests, load diffs, comment on old/new lines, approve or request changes, execute provider merges |
| Merge gates | Current commit, distinct non-author provider identities, reviewer access, required approvals, resolved discussions, real provider CI and mergeability |
| Webhooks | GitHub HMAC-SHA256 validation; GitLab secret token validation; persistent per-project delivery deduplication |
| Activity | Append-only API behavior, actor/action/time details, SHA-256 hash chain verified on startup |
| CI | Persistent queued → running → passed/failed simulations, incremental logs, restart recovery |
| Notifications | Assignments, reviews, approvals, merges, and pipeline outcomes; per-user read state |
| Live updates | Authenticated server-sent events, heartbeat, automatic reconnect, state refresh |
| Audit | Separate hash-linked log for membership, settings, imports, approvals, merges, and secret rotation |

Organization roles are inherited by all projects. A project role can raise access, but cannot reduce inherited access. Teams organize people and ownership; they do not add a separate permission tier. Archived projects reject work mutations and webhook ingestion. All authorization is enforced server-side.

## Try the complete flow

1. Sign in as Alex. Create an issue and assign Jordan; select a label and milestone.
2. Open the seeded **Add project access middleware** merge request. Its author is Jordan.
3. Add a review comment. Approval alone will not permit a merge until the discussion is resolved.
4. Approve and resolve the discussion. Run a failed pipeline; the latest failed run blocks merging.
5. Run a passed pipeline for the same merge request. Watch its logs and notifications update live.
6. Merge. Inspect Activity and Audit log. Restart the app and verify that the records remain.

## Configuration

| Variable | Default / purpose |
| --- | --- |
| `DATABASE_URL` | Enables PostgreSQL storage and automatic schema migration |
| `DATABASE_SSL_ROOT_CERT` | Optional provider root certificate path |
| `FORGE_ENV_FILE` | `.env`; shell environment overrides values in this file |
| `FORGE_ADDR` | `127.0.0.1:8080` |
| `FORGE_BASE_URL` | `http://` + address; exact public origin for OAuth callback and CSRF checks |
| `FORGE_DATA` | `data/state.json` |
| `FORGE_DEMO` | Set to `1` to enable sample-user login |
| `FORGE_ADMIN_TOKEN` | Optional API administrator token; use at least 24 random characters |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | GitHub OAuth application credentials |
| `GITLAB_CLIENT_ID`, `GITLAB_CLIENT_SECRET` | GitLab.com OAuth application credentials |

The default local listener is intentionally loopback-only. When running behind a proxy, set `FORGE_ADDR` and the exact `FORGE_BASE_URL`, terminate HTTPS at the proxy, and disable response buffering for `/api/events`. HTTPS base URLs enable secure session cookies.

## OAuth import

Register an OAuth application with each provider and configure these callbacks:

```text
http://127.0.0.1:8080/api/oauth/github/callback
http://127.0.0.1:8080/api/oauth/gitlab/callback
```

Use your configured base URL if different. GitHub requests `repo` scope; GitLab requests `api` scope to support merging (reconnect older read-only authorizations). Select **Import repository**, connect the provider, then choose a repository. Import stores metadata; code stays at the provider.

## Provider merging and reviews

1. Configure a provider OAuth application, connect it in Project settings, and import a repository.
2. Open Merge requests → **Link provider request** and enter an existing pull/merge request number. Creating remote requests and hosting Git objects are not implemented.
3. Each reviewer connects their own provider account. Load the diff, select a line number to comment, and submit **Approve** or **Request changes**. Reviews and comments are stored in Forge; they are not posted as native provider reviews.
4. New head commits invalidate approvals. Change requests remain blocking until the same provider identity approves, and unresolved discussions remain blocking even on older commits. Accounts sharing a provider identity count only once. Provider authors cannot approve their own request.
5. Maintainers can confirm a merge after all gates pass. Forge fetches fresh status and supplies the exact reviewed head SHA to the provider merge API. GitHub uses a merge commit; GitLab uses its configured project merge method. Provider branch protections still apply. Forge conservatively requires GitHub `clean` / GitLab `mergeable` status, plus successful provider CI when configured. Simulated pipelines do not satisfy this gate.

Webhook deliveries flag linked requests for refresh; they cannot overwrite authoritative state with an out-of-order payload. Refresh, diff loading, review submission, and merge all fetch provider status. The page shows the last synchronization time; continuous background provider polling is not implemented. External merges are reconciled on refresh. Repository replacement is blocked once requests are linked.

Merge intent is persisted before calling the provider. If the network loses the result, automatic retries are blocked to prevent duplicate side effects. Refresh reconciles confirmed merges; if the provider still reports an open request, inspect it there and finish the merge there if appropriate. No unsafe force-retry endpoint is exposed. This MVP supports a single active application writer, not distributed merge coordination.

Patches are paginated up to 3,000 files; binary/oversized/omitted patches are marked unavailable. Inspect such files at the provider. Provider approval rules and checks may add further blockers. Tokens remain in memory and must be reconnected after restart.

The seeded/local requests retain their explicitly labeled metadata-only simulation workflow.

OAuth tokens remain only in server memory, are never returned to the browser, and are lost on restart. Reconnect after a restart or provider token expiry. Refresh-token rotation and self-managed GitLab hosts are not supported yet.

Provider references: [GitHub OAuth authorization](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps), [GitLab OAuth API](https://docs.gitlab.com/api/oauth2/).

## API

Use the session cookie obtained from `POST /api/login`, or `Authorization: Bearer <FORGE_ADMIN_TOKEN>`. All non-webhook POST requests require `X-Requested-With: forge`. Browser origins must match `FORGE_BASE_URL`.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Health check |
| `GET /api/session` | Current identity and demo flag |
| `POST /api/signup` | `{ "name": "Your Name", "email": "you@example.com", "password": "12-or-more-characters" }` |
| `POST /api/login` | `{ "email": "you@example.com", "password": "..." }`; optional demo/admin credentials also supported |
| `POST /api/logout` | Invalidate session |
| `GET /api/state` | Permission-filtered application snapshot |
| `GET /api/events` | SSE `ready` and `update` events; clients refetch state |
| `POST /api/reviews` | `action`: `link`, `sync`, `diff`, `review`, `comment`, `merge`; requires `project_id`; linked actions use `id`; diff/review/comment/merge require `head_sha` |
| `POST /api/action` | Validated domain command; examples below |
| `GET /api/oauth/{provider}/start?project_id=...` | Start OAuth connection |
| `GET /api/oauth/{provider}/repositories?page=1` | List authorized repository metadata |
| `POST /api/import` | `{ "project_id": "orbit", "provider": "github", "external_id": "123" }` |
| `POST /api/webhooks/{provider}/{project_id}` | Authenticated event ingestion |

Command example:

```json
{
  "kind": "issue.create",
  "project_id": "orbit",
  "title": "Add health endpoint",
  "body": "Return service readiness.",
  "assignee_id": "jordan",
  "labels": ["backend"],
  "milestone_id": "v1"
}
```

Supported command kinds: `organization.create`, `organization.member`, `team.create`, `team.update`, `project.create`, `project.settings`, `project.member`, `webhook.rotate`, `label.create`, `milestone.create`, `milestone.state`, `issue.create`, `issue.update`, `mr.create`, `mr.comment`, `mr.resolve`, `mr.approve`, `mr.merge`, `mr.close`, `pipeline.create`, `notification.read`. See `Command` in `actions.go` for payload fields. Updates replace the editable fields; they are not JSON patches.

### Webhook setup

Copy the endpoint and shared secret from Project settings. GitHub requests must include `X-Hub-Signature-256: sha256=<HMAC of raw body>`, `X-GitHub-Delivery`, and `X-GitHub-Event`. GitLab requests must include `X-Gitlab-Token`, `X-Gitlab-Event-UUID`, and `X-Gitlab-Event`. Payloads must be JSON objects under 1 MiB. Valid events append activity; they do not execute commands or automatically mutate imported repository state. The same delivery ID is accepted once per provider/project. Rotating a secret requires updating the remote webhook configuration.

## Storage and deployment boundary

This remains a single-application-process MVP with an in-memory working state. Each mutation validates permissions and invariants, writes changed relational rows inside a PostgreSQL transaction, then publishes an SSE invalidation. A database revision check rejects stale writers rather than overwriting another process's changes. After a revision conflict or an ambiguous failed commit, restart the process before retrying. Do not run multiple app instances against the same database: distributed cache invalidation, session rate limiting, and CI scheduling are not implemented. Database sessions themselves persist across restarts. On startup, unfinished simulations resume and event chains are verified.

In local-file mode, state uses a synced temporary file and rename; sessions remain in memory. Do not share that file between processes.

The application offers no update/delete API for activity or audit entries. PostgreSQL triggers also reject UPDATE, DELETE, and TRUNCATE on these tables. A database owner can still disable those protections; independently anchored audit storage is outside this MVP. Complete API snapshots, in-memory OAuth connections, unbounded event retention, and simulated runners remain MVP boundaries. Git object hosting, remote request creation, native provider review publication, distributed jobs, and email/push notifications are outside this version.

## Test and build

Custom dropdown DOM tests (test-only dependency, no frontend production build): `pnpm --dir tools/ui-test install --frozen-lockfile`, then `node --test tests/*.test.cjs tools/ui-test/selects.test.cjs`. These verify form submission, multi-select, search, keyboard selection, dialog dismissal, validation, resets, and dynamically replaced controls. Popover layout still needs a browser visual check.

```sh
go test ./... -count=1 -cover
go vet ./...
go build -o forge .
```

Tests cover account signup/login/logout, tenant isolation, rate limiting, relational state conversion, plus the existing permissions, merge gates, CI, webhook, OAuth mock, and SSE workflows. `FORGE_TEST_DB=1 go test -run TestConfiguredDatabaseConnection -v` opts into a read-only check of `.env`. `TestPostgresPersistence` is opt-in and requires `FORGE_TEST_DATABASE_URL` pointing to a disposable local database at `postgresql://postgres@127.0.0.1:55432/postgres?sslmode=disable`; it creates records and checks constraints, persisted sessions, immutable events, and stale-write rollback. It refuses remote databases. Real OAuth provider consent/import needs your credentials.

`FORGE_MIGRATE_VERIFY=1 go test -run TestConfiguredMigration -v` explicitly applies the migration/import to the database in `.env`, verifies reload and persistent sessions, and checks writes inside a transaction that is rolled back. It is an operational migration command, not part of the default test suite. `/healthz` reports `storage: postgres` and checks database reachability.

Container example (use a non-demo token for shared deployments):

```sh
docker build -t forge .
docker run --rm -p 127.0.0.1:8080:8080 -v forge-data:/data \
  -e FORGE_DEMO=1 -e FORGE_BASE_URL=http://127.0.0.1:8080 forge
```

## Source map

- `main.go`: HTTP server, authentication, filtered snapshots, transactions, SSE, event hashing
- `models.go`: domain models, file persistence, demo seed
- `postgres.go`, `migrations/`: relational persistence, migration, import, TLS
- `auth.go`: signup, bcrypt password validation, persistent sessions, rate limits
- `actions.go`: permissions, domain commands, CI worker
- `integrations.go`: OAuth, metadata import, webhook verification
- `reviews.go`, `reviews_test.go`: provider diffs, commit-bound reviews, merge execution and safety tests
- `web/`: embedded HTML/CSS/JavaScript application
- `app_test.go`: integration tests
