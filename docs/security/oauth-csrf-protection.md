# GitHub OAuth CSRF Protection

## Overview

The GitHub OAuth flow uses the **state parameter** as a CSRF (Cross-Site Request Forgery) defense. This document describes the implemented protections, test coverage, and security guarantees.

## Attack Vectors Defended

### 1. **Missing State Attack**

**Attack:** An attacker constructs a callback URL without a state parameter, hoping to bypass CSRF checks.

**Defense:** `CallbackUnified` rejects requests where `code == "" || encodedState == ""` with `400 missing_code_or_state` (line ~246 in `internal/handlers/github_oauth.go`).

**Test:** `TestGitHubOAuthCSRF_MissingState`

### 2. **Mismatched / Never-Issued State Attack**

**Attack:** An attacker generates their own authorization URL with a random or controlled state value, hoping it won't be validated against the server's records.

**Defense:** The handler decodes the state parameter to extract the CSRF token, then performs a database lookup:
```sql
SELECT kind, user_id, redirect_uri
FROM oauth_states
WHERE state = $1 AND expires_at > now()
```
If no row is found (`pgx.ErrNoRows`), the request is rejected with `400 invalid_or_expired_state` (line ~277).

**Tests:**
- `TestGitHubOAuthCSRF_MismatchedState` — random state that was never inserted
- `TestGitHubOAuthCSRF_MismatchedState_ValidBase64` — plausible-looking base64 state

### 3. **State Replay Attack**

**Attack:** An attacker captures a valid callback URL (containing `code` and `state`) and submits it a second time, hoping to link their account to the victim's session.

**Defense:** The `oauth_states` row is **deleted before** `ExchangeCode` is called (line ~280):
```go
_, _ = h.db.Pool.Exec(c.Context(), `DELETE FROM oauth_states WHERE state = $1`, csrfToken)

tr, err := github.ExchangeCode(c.Context(), code, github.OAuthConfig{...})
```

This ordering is critical: even if the token exchange fails (network error, expired code), the state cannot be reused.

**Tests:**
- `TestGitHubOAuthCSRF_StateReplay` — direct replay of consumed state
- `TestGitHubOAuthCSRF_StateDeletedBeforeExchange` — confirms DELETE happens before exchange, even when exchange fails

### 4. **Expired State Attack**

**Attack:** An attacker captures a callback URL and tries to use it hours or days later.

**Defense:** States have a 10-minute TTL (`expires_at = now() + 10min`). The database lookup uses `WHERE expires_at > now()`, so expired rows are treated as nonexistent.

**Test:** `TestGitHubOAuthCSRF_ExpiredState`

## Security Guarantees

| Property | Guarantee | Verified By |
|----------|-----------|-------------|
| **No callback without state** | Any callback missing `state` param is rejected before DB access | `TestGitHubOAuthCSRF_MissingState` |
| **Only server-issued states accepted** | Only states previously stored in `oauth_states` are valid | `TestGitHubOAuthCSRF_MismatchedState*` |
| **Single-use enforcement** | Each state can be consumed exactly once; replay is rejected | `TestGitHubOAuthCSRF_StateReplay` |
| **DELETE before exchange** | State deletion precedes token exchange, preventing race conditions | `TestGitHubOAuthCSRF_StateDeletedBeforeExchange` |
| **TTL enforcement** | States older than 10 minutes are invalid | `TestGitHubOAuthCSRF_ExpiredState` |
| **Legitimate flows unaffected** | Valid OAuth flows pass all CSRF checks | `TestGitHubOAuthCSRF_LegitimateFlowUnaffected` |

## State Lifecycle

1. **Issuance** (`LoginStart`):
   ```go
   state := randomState(32) // cryptographically random 32 bytes, base64-encoded
   expiresAt := time.Now().UTC().Add(10 * time.Minute)
   INSERT INTO oauth_states (state, user_id, kind, expires_at) VALUES (...)
   ```

2. **GitHub Authorization**: User is redirected to `https://github.com/login/oauth/authorize?state=<state>&...`

3. **GitHub Callback**: GitHub redirects back with `?code=<code>&state=<state>`

4. **Validation** (`CallbackUnified`):
   ```go
   if code == "" || encodedState == "" { return 400 } // Reject missing params
   csrfToken, redirectURI, err := decodeStateWithRedirect(encodedState)
   SELECT ... FROM oauth_states WHERE state = csrfToken AND expires_at > now()
   // If no row → 400 invalid_or_expired_state
   DELETE FROM oauth_states WHERE state = csrfToken  // Single-use enforcement
   tr, err := github.ExchangeCode(...) // Proceed to token exchange
   ```

## State Encoding

The state parameter encodes both a CSRF token and an optional redirect URI:

**Format:** `base64(csrf_token + "|" + redirect_uri)`

**Examples:**
- Simple: `"abc123"` (just the CSRF token, backward compatible)
- With redirect: `base64("abc123|https://example.com")` → `"YWJjMTIzfGh0dHBzOi8vZXhhbXBsZS5jb20="`

**Decoding** (`decodeStateWithRedirect`):
1. Attempt base64 decode
2. If successful, split on `|` to extract `csrf_token` and `redirect_uri`
3. If decode fails or no `|`, treat entire string as CSRF token (backward compat)

The CSRF token is always the part looked up in `oauth_states`.

## Test Coverage

### Unit Tests (`internal/github/oauth_test.go`)

- `TestAuthorizeURL_StatePassthrough` — state parameter round-trips through URL unchanged
- `TestAuthorizeURL_EmptyStateIsInsecure` — documents that empty state is allowed but insecure

### Integration Tests (`internal/handlers/github_oauth_csrf_test.go`)

All tests require `TEST_DB_URL` and skip gracefully when unset.

| Test | Scenario | Expected Result |
|------|----------|-----------------|
| `TestGitHubOAuthCSRF_MissingState` | Callback without `state` param | `400 missing_code_or_state` |
| `TestGitHubOAuthCSRF_MissingCode` | Callback without `code` param | `400 missing_code_or_state` |
| `TestGitHubOAuthCSRF_MismatchedState` | State never inserted in DB | `400 invalid_or_expired_state` |
| `TestGitHubOAuthCSRF_MismatchedState_ValidBase64` | Plausible but fake state | `400 invalid_or_expired_state` |
| `TestGitHubOAuthCSRF_StateReplay` | Reused state after first use | `400 invalid_or_expired_state` |
| `TestGitHubOAuthCSRF_StateDeletedBeforeExchange` | DELETE ordering verification | State deleted even if exchange fails |
| `TestGitHubOAuthCSRF_ExpiredState` | State with `expires_at` in past | `400 invalid_or_expired_state` |
| `TestGitHubOAuthCSRF_LegitimateFlowUnaffected` | Valid state, passes checks | Proceeds to token exchange |

### Running Tests

```bash
# Unit tests (no DB required)
go test ./internal/github/... -run OAuth -v

# Integration tests (requires TEST_DB_URL)
TEST_DB_URL="postgresql://user:pass@localhost/test_db" \
  go test ./internal/handlers/... -run GitHubOAuthCSRF -v

# All OAuth tests with race detection
TEST_DB_URL="..." go test -race ./internal/...  -run OAuth -v
```

## Threat Model

### In Scope

- **Classic CSRF attack:** Attacker tricks victim into completing attacker's OAuth flow
- **Replay attacks:** Captured callback URL reused
- **Timing attacks:** Expired state used after TTL
- **Malformed state:** Invalid base64 or unexpected format

### Out of Scope

- **Authorization code interception:** Defended by GitHub's HTTPS and short-lived codes
- **Client secret compromise:** Requires infrastructure-level controls
- **Session fixation:** Separate concern (JWT issuance)

### Assumptions

1. **Database integrity:** `oauth_states` table is authoritative
2. **Clock synchronization:** Server clock is reasonably accurate for TTL enforcement
3. **Cryptographic randomness:** `crypto/rand` provides unpredictable state values
4. **Transport security:** All OAuth flows use HTTPS (enforced by GitHub)

## Known Limitations

### False Negatives (Cannot Detect)

- **State harvesting:** If an attacker can repeatedly trigger `LoginStart` for the victim, they could collect valid states. Mitigation: rate-limit `LoginStart` per IP/session.
- **Man-in-the-middle:** An HTTPS MITM could steal the callback. Mitigation: rely on TLS infrastructure, certificate pinning (out of scope).

### False Positives (Legitimate Use Cases Blocked)

- **User takes >10 minutes:** If a user completes GitHub authorization after 10 minutes, their state is expired. UX mitigation: inform user and restart flow.
- **Browser issues:** If a user double-clicks the callback link, the second click is rejected (single-use). UX mitigation: disable link after first click on frontend.

## Security Contact

For security issues related to OAuth CSRF protection, file a private security advisory on GitHub or contact the maintainers directly. Do not open public issues for vulnerabilities.

## References

- [OAuth 2.0 RFC 6749 § 10.12: CSRF](https://datatracker.ietf.org/doc/html/rfc6749#section-10.12)
- [OAuth 2.0 Threat Model RFC 6819 § 4.4.1.8: CSRF Attack](https://datatracker.ietf.org/doc/html/rfc6819#section-4.4.1.8)
- [OWASP: Cross-Site Request Forgery Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)
