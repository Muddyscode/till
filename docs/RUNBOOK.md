# Till runbook

Operating `tilld` in production. One stateless API process (run several behind a load balancer if you like) and one Postgres 16.

## 1. Deploy shape

```text
client --TLS--> reverse proxy (Caddy / nginx / cloud LB) --HTTP--> tilld :8080 --> Postgres
```

- **TLS terminates at the proxy.** `tilld` speaks plain HTTP. Never expose `:8080` to the internet. Redirect 80 to 443, enable HSTS at the proxy, and use TLS 1.2+.
- Set `TILL_TRUST_PROXY=true` only when every request really passes through your proxy and the proxy appends the client address to `X-Forwarded-For`. Otherwise the login rate limiter sees every user as the proxy's IP.
- Block `/metrics` from the public internet at the proxy. Scrape it from the private network. Probes: `/livez` (process is up), `/readyz` (database reachable, sweeper alive, not shutting down), `/v1/health` (legacy database ping).
- Use `sslmode=require` (or `verify-full`) in `TILL_DATABASE_URL` for any database that is not on the same host.
- Run the container read-only if you can; it writes nothing to disk. Image: `docker build -t till .` (non-root, `Africa/Lagos` tzdata, port 8080).

## 2. Environment variables

| Variable | Default | Notes |
|---|---|---|
| `TILL_ENV` | `development` | `production` refuses example secrets, short secrets, `TILL_SEED=true`, `TILL_CORS_ORIGINS=*`. The Docker image defaults it to `production`. |
| `TILL_DATABASE_URL` | required | pgx URL. |
| `TILL_HTTP_ADDR` | `:8080` | |
| `TILL_JWT_SECRET` | required | 32+ chars (48+ in production). Signs staff tokens. |
| `TILL_PAYSTACK_SECRET` | required | 16+ chars (24+ in production). Verifies the webhook signature. |
| `TILL_TOKEN_TTL` | `8h` | Go duration, `1m` to `720h`. |
| `TILL_CORS_ORIGINS` | empty | Comma-separated exact origins. Empty means CORS is off. |
| `TILL_TRUST_PROXY` | `false` | See above. |
| `TILL_LOGIN_RATE_IP` / `TILL_LOGIN_RATE_EMAIL` | `30` / `6` | Attempts per minute. In memory, per instance: with N instances the effective limit is N times higher. The 10-failures-in-15-minutes lockout is stored in Postgres and is shared. |
| `TILL_HOLD_MINUTES` | `15` | Transfer hold. |
| `TILL_SEED`, `TILL_SEED_PASSWORD` | `false` | Demo data. Never in production. |

Generate secrets with `openssl rand -base64 48`. Keep them in your platform's secret store, not in the image or the repository.

## 3. First boot

1. Provision Postgres, create the `till` database and role.
2. Start `tilld` once with the production env. It migrates on boot.
3. Create the first admin. There is no public registration and `TILL_SEED` is refused in production, so insert the first merchant and admin staff row with SQL (hash the password with argon2id, for example by running `HashPassword` from a Go test or a seeded scratch database). Then log in and change the password with `POST /v1/auth/password`. A proper bootstrap command is an open item.

## 4. Backups and point-in-time recovery

The journal is the source of truth for money, so losing even a few minutes of writes is a bookkeeping incident. Prefer managed Postgres with PITR (RDS, Cloud SQL, Neon, Supabase). If you run Postgres yourself:

**Base backup (daily):**

```bash
pg_basebackup -h $DB_HOST -U replicator -D /backups/base/$(date +%F) -Ft -z -X none -c fast --checkpoint=fast
```

**WAL archiving (continuous)** in `postgresql.conf`:

```text
wal_level = replica
archive_mode = on
archive_command = 'test ! -f /backups/wal/%f && cp %p /backups/wal/%f'   # or wal-g / pgBackRest to object storage
archive_timeout = 60
```

Copy `/backups` off the host (object storage with versioning and a different account). Keep 14 daily base backups and the WAL between them. Encrypt backups at rest. Better tools: pgBackRest or WAL-G handle retention, compression and verification for you.

Also take a logical dump weekly (`pg_dump -Fc`) as a second, version-independent copy.

## 5. Restore drill (do this quarterly; an untested backup is not a backup)

1. Provision a scratch Postgres 16 instance. Never restore over production.
2. Restore the newest base backup into its data directory, add `restore_command = 'cp /backups/wal/%f %p'` and `recovery_target_time = '<UTC time just before the incident>'` to `postgresql.conf`, create `recovery.signal`, start Postgres, wait for recovery to finish, then `SELECT pg_wal_replay_resume();` if it paused at target.
3. Point a throwaway `tilld` at it and call `GET /v1/invariants` as an admin. It must report `"holds": true` (books balance, cached balances match the journal, wallet and stock agree).
4. Spot check: the last order before the target time exists, and `GET /v1/reports/daily-sales` matches the previous day's printed Z-report or the Paystack dashboard.
5. Record: time to restore, data lost (target time versus last good write), anything surprising. Update this runbook.

## 6. Key and secret rotation

- **JWT secret.** Changing `TILL_JWT_SECRET` invalidates every session at once (everyone logs in again). Rotate: roll it out to all instances in one deploy, or accept a short window where old-secret instances reject new tokens. To revoke a single person instead, `POST /v1/auth/logout` as them or `POST /v1/staff/{id}/password-reset` as an admin (bumps `token_version`).
- **Paystack secret.** Change it in the Paystack dashboard and in `TILL_PAYSTACK_SECRET` together. Webhooks signed with the old secret return 401 until both match; Paystack retries, and unmatched payments are visible afterwards.
- **API keys.** Create the replacement (`POST /v1/api-keys`), deploy it to the integration, confirm `last_used_at` moves on the new key in `GET /v1/api-keys`, then `DELETE /v1/api-keys/{id}` the old one. Revocation is immediate. A leaked key: revoke first, investigate second; `GET /v1/audit` shows what its `staff_id` did.
- **Database password.** Create a second role, switch `TILL_DATABASE_URL`, redeploy, drop the old role.

## 7. Incident checklist

1. **Is it up?** `curl /livez`, `curl /readyz`. `readyz` 503 tells you which check failed: `database`, `sweeper` (expired transfer holds are not being released; stock stays reserved), or `draining`.
2. **Find the request.** Every response has `X-Request-ID`; every log line has `request_id`. Logs are JSON on stdout: `request` lines carry method, route, status, duration, merchant/staff/api-key ids; 5xx lines include `err`.
3. **Metrics to look at:** `http_requests_total{status=~"5.."}`, `http_request_duration_seconds` (p95), `login_failures_total` and `rate_limited_total` (credential stuffing shows up as both rising), and Postgres connections.
4. **Money correctness:** run `GET /v1/invariants` as admin. If `holds` is false, stop writes at the proxy (maintenance response) and do not "fix" rows by hand. Journals and audit are append-only by trigger; correct with reversals.
5. **Suspected credential leak:** revoke API keys, reset the admin passwords (`password-reset` bumps token version, which kills their sessions), rotate `TILL_JWT_SECRET` if signing could be exposed, review `GET /v1/audit` for `apikey.*`, `staff.*` and unexpected refunds/adjustments.
6. **Locked-out staff:** the lock ends 15 minutes after the last failure. An admin can end it earlier by resetting that person's password.
7. **Database failover:** `tilld` reconnects on its own through the pgx pool. Check `readyz` goes green, then run the invariants.
8. **Afterwards:** write down the timeline, the request ids, what detected it, and one change that would have caught it sooner.

## 8. Graceful shutdown and deploys

On SIGTERM/SIGINT `tilld` flips `/readyz` to 503, stops accepting new connections, lets in-flight requests finish (up to 15 s), then stops the sweeper and waits for it. Set your orchestrator's termination grace period to 30 s or more, and have the load balancer use `/readyz`. Migrations run on boot; deploy one instance first when a release adds a migration.
