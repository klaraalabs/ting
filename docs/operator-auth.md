# Operator authentication

Operators administer the Ting instance. They are separate from consumer App users and App API tokens. There is no public operator signup, password-reset endpoint, or dashboard yet. Operator sessions protect the implemented [Management API](management-api.md).

## Bootstrap and recovery

With the usual database and encryption-key configuration loaded, run:

```sh
bin/ting admin create --username admin
bin/ting admin reset-password --username admin
```

For Compose, use `docker compose -f deploy/compose.yaml exec ting ting admin create --username admin`. Keep the interactive terminal enabled. The reset command uses the same pattern.

Both commands prompt for a password and confirmation without echoing either. Password flags, environment variables, and piped password input are not supported. Usernames are case-insensitive, normalized to lowercase, and contain 3 to 64 ASCII letters, digits, dots, underscores, or hyphens, beginning with a letter or digit. Passwords contain 15 to 128 Unicode characters, at most 512 bytes, without NUL. Passwords are not trimmed or normalized.

`create` refuses an existing username and never overwrites its password. It can also create additional operators; all operators currently have instance-wide authority. Database and CLI access are trusted operator access.

`reset-password` replaces the password and revokes every session for that operator in one transaction. A login racing with a reset cannot retain a session authenticated with the previous password. Reset clears that username's login throttle; peer and global limits still apply. Other operators' sessions are unaffected.

Passwords use salted Argon2id with 19 MiB memory, two iterations, and one lane, following the [OWASP password storage baseline](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html). Hash verification permits only the stored format's bounded parameters. At most two password operations run concurrently per process.

## HTTPS and configuration

| Variable | Default | Behavior |
| --- | --- | --- |
| `TING_ADMIN_ENABLED` | `true` | Set `false` to return 404 for admin endpoints. CLI commands remain available. |
| `TING_ADMIN_ORIGIN` | `https://localhost:8080` | Exact browser-facing HTTPS origin, including any non-default port. No trailing slash, path, query, or credentials. |
| `TING_ADMIN_SESSION_TTL` | `8h` | Absolute session lifetime, from `5m` to `24h`. Activity does not extend expiry. Changes apply to new sessions. |

`ting serve` still listens over HTTP behind a TLS-terminating reverse proxy. Configure the proxy to preserve the external `Host`, overwrite `X-Forwarded-Proto` with `https` for TLS requests, and set `TING_TRUSTED_PROXIES` to its specific source CIDR. Set `TING_ADMIN_ORIGIN` to the origin the browser uses. Forwarded host headers are not used. The localhost default is a configuration value, not a built-in HTTPS listener.

Admin requests require HTTPS even on localhost. `TING_ALLOW_LOCAL_HTTP` applies to the public development API and does not weaken operator cookie security. Compose alone does not provide TLS. Creating an operator through the CLI does not require a running HTTP server or proxy.

Sessions use the host-only `__Host-ting_session` cookie with `Secure`, `HttpOnly`, `SameSite=Strict`, and `Path=/`. The database stores only a SHA-256 hash of the random 256-bit session token. Sessions survive server restarts, expire at their recorded deadline, and can be revoked immediately. No additional session signing key is required. Login rotates the presented session, including when switching operators. Each operator has at most ten sessions; login removes expired/revoked sessions and evicts the oldest excess sessions.

## Session API

All endpoints use the configured HTTPS origin, return `Cache-Control: no-store`, and reject `Authorization` headers. App tokens cannot authenticate operator requests; operator cookies and session tokens cannot authenticate public notification sends.

| Method | Path | Behavior |
| --- | --- | --- |
| `POST` | `/admin/api/v1/session` | Verify username/password, revoke the presented previous session, and set a fresh session cookie. |
| `GET` | `/admin/api/v1/session` | Return the current operator, expiry, and CSRF token. Requires the session cookie. |
| `DELETE` | `/admin/api/v1/session` | Revoke the current session and expire its cookie. Returns 204. |

Login accepts an `application/json` body of at most 4 KiB:

```json
{"username":"admin","password":"<password entered by the operator>"}
```

Login requires `Origin` equal to `TING_ADMIN_ORIGIN` and `X-Ting-CSRF: login`. This custom header and strict JSON content type, together with the origin check and absence of CORS access, protect login from cross-site submission before a session exists. Authenticated mutations require that same exact Origin plus `X-Ting-CSRF` containing the session's returned `csrfToken`. Cross-site and same-site-but-cross-origin Fetch Metadata requests are rejected. These controls follow [OWASP's CSRF guidance](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html).

Successful login and session inspection return:

```json
{
  "operator": {"id":"<uuid>","username":"admin"},
  "expiresAt":"<RFC3339 timestamp>",
  "csrfToken":"<session-bound CSRF token>"
}
```

The raw session token is sent only in the HttpOnly cookie. Keep the CSRF token in browser memory; reload it with `GET /admin/api/v1/session` after a page refresh. Never store passwords or session tokens in browser storage. A same-origin dashboard can use:

```js
const response = await fetch('/admin/api/v1/session', {
  method: 'POST',
  credentials: 'same-origin',
  headers: {'Content-Type': 'application/json', 'X-Ting-CSRF': 'login'},
  body: JSON.stringify({username, password}),
});
if (!response.ok) throw new Error('Sign-in failed');
const session = await response.json();
// The browser supplies Origin on POST and DELETE requests.
await fetch('/admin/api/v1/session', {
  method: 'DELETE',
  credentials: 'same-origin',
  headers: {'X-Ting-CSRF': session.csrfToken},
});
```

Wrong passwords and unknown usernames return the same 401 error. Missing, revoked, or expired sessions also return 401. Invalid origin or CSRF checks return 403. Unsupported content types return 415; oversized login bodies return 413. Rate limits return 429 with `Retry-After: 60`.

## Login limits

PostgreSQL enforces fixed one-minute budgets of five attempts per normalized username, ten per direct network peer, and sixty across the instance. Validly shaped attempts count whether they succeed or fail. Malformed username/password input is rejected before password hashing. Limits survive restarts and are shared across server processes. Expired throttle records are removed during login.

The peer budget intentionally uses the direct network peer, not client-supplied forwarding headers. Operators behind the same reverse proxy share that budget. The existing public request limiter also applies. Repeated unauthorized attempts can temporarily block a legitimate login; this implementation does not provide MFA or a distributed lockout recovery service.

## Verification

The race-enabled PostgreSQL suite tests hashing, session rotation and expiry, logout, password-reset races, persisted and concurrent login throttles, CSRF/origin checks, trusted TLS proxies, disabled admin routes, App-token separation, and an HTTPS cookie-jar login/logout round trip. CLI bootstrap and reset have also been exercised through a real terminal to verify hidden password entry.
