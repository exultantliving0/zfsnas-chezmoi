# Plan: Version 6.3.23 — Security Hardening

**Source:** Security audit by Claude Opus 4.6 (issue #16), v6.2.0 codebase
**Scope:** All HIGH and MEDIUM findings from the audit

---

## 1. H1 — HTTP Security Headers Middleware

**File:** `handlers/router.go`

Add a `SecurityHeaders` middleware and register it globally alongside the existing `EnforceOrigin` middleware.

```go
func SecurityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "DENY")
        w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
        next.ServeHTTP(w, r)
    })
}
```

Register via `r.Use(SecurityHeaders)` in `NewRouter()`.

Note: Omit `Content-Security-Policy` for now — the SPA loads Chart.js from CDN and uses inline scripts; a full CSP policy would require significant frontend refactoring and is out of scope for this release.

---

## 2. M1 — Encrypt iSCSI CHAP Credentials at Rest

**Files:** `internal/config/config.go`, `system/iscsi.go`

The `ISCSICredential` struct has `InPassword` and `OutPassword` stored as plaintext in `config.json`. Apply the same AES-256-GCM pattern already used for SMTP passwords and TOTP secrets via the `internal/secret` package.

### Steps

1. **New key file:** `config/keys/chap.key` — generated on first use by `internal/secret`, mode 0600. Reuse the existing `internal/secret.Encrypt` / `secret.Decrypt` functions.

2. **Encrypt on save:** In the config save path for ISCSI credentials, call `secret.Encrypt(password)` before writing to JSON. Store the `enc:...` ciphertext.

3. **Decrypt on load:** Wherever `InPassword` / `OutPassword` are read for actual use (passed to `targetcli`), call `secret.Decrypt()`. The `enc:` prefix makes it safe to call unconditionally (plaintext values without the prefix pass through unchanged — handles migration of existing configs).

4. **Never return passwords to frontend:** Already correct — `HandleListISCSICredentials` returns `InPasswordSet: bool` only. No change needed there.

### Migration

Existing plaintext passwords are transparently migrated: on first save after upgrade, they get encrypted. No explicit migration function needed thanks to the `enc:` prefix guard in `secret.Decrypt`.

---

## 3. M2 — Eliminate `sh -c` in Replication

**File:** `system/replication.go`

This is the only place in the codebase that uses `exec.Command("sh", "-c", shellCmd)` with user-supplied values interpolated into the shell string. Replace with a Go pipe between two `exec.Command` calls.

### Current code (lines ~72–78)

```go
shellCmd := fmt.Sprintf("sudo zfs %s | ssh -o BatchMode=yes -o StrictHostKeyChecking=no %s '%s'",
    strings.Join(sendArgs, " "), sshTarget, receiveCmd)
cmd := exec.Command("sh", "-c", shellCmd)
```

### Replacement

```go
sendCmd := exec.Command("sudo", append([]string{"zfs"}, sendArgs...)...)
sshArgs := []string{"-o", "BatchMode=yes", sshTarget, receiveCmd}
// M3 fix applied here — StrictHostKeyChecking=no removed (see section 4)
sshCmd := exec.Command("ssh", sshArgs...)

pr, pw := io.Pipe()
sendCmd.Stdout = pw
sshCmd.Stdin = pr

var sshStderr bytes.Buffer
sshCmd.Stderr = &sshStderr

if err := sshCmd.Start(); err != nil { ... }
if err := sendCmd.Run(); err != nil { pw.CloseWithError(err); ... }
pw.Close()
if err := sshCmd.Wait(); err != nil { ... }
```

`sshTarget` is composed from `RemoteUser@RemoteHost` (both validated as non-empty at the handler layer); `receiveCmd` is `"sudo zfs receive ..."` built from validated dataset path components. With shell removed, no interpolation attack surface remains.

---

## 4. M3 — Remove `StrictHostKeyChecking=no` from Replication SSH

**File:** `system/replication.go`

Remove `-o StrictHostKeyChecking=no` from the SSH invocation (done as part of the M2 rewrite above).

### UX impact

On first replication to a new host, SSH will fail if the host key is not in `~/.ssh/known_hosts`. To handle this gracefully:

- The replication error message returned to the UI should detect the `"Host key verification failed"` string in stderr and return a user-friendly message: *"Host key not trusted. Connect to this host via SSH once to accept its key, or use the terminal to run: `ssh-keyscan <host> >> ~/.ssh/known_hosts`"*
- No automatic `StrictHostKeyChecking=accept-new` — that still bypasses verification on first connection and can be MITMed.

---

## File Changes Summary

| File | Change |
|------|--------|
| `handlers/router.go` | Add `SecurityHeaders` middleware, register with `r.Use()` |
| `system/replication.go` | Replace `sh -c` pipe with Go `io.Pipe()`; remove `StrictHostKeyChecking=no`; improve error message for host key failure |
| `internal/config/config.go` | `ISCSICredential.InPassword` / `OutPassword` encrypted via `internal/secret` on save; decrypted on use |
| `system/iscsi.go` | Decrypt CHAP passwords before passing to `targetcli` |

---

## Out of Scope (deferred to roadmap)

- **L1** (install script NOPASSWD:ALL) — install script improvement, separate from the portal binary
- **L2** (sessions lost on restart) — usability, not security risk
- **L3** (chmod 777 on SMB shares) — behavioral change, needs UX consideration
- **L4** (config TOCTOU races) — low-risk in single-writer scenario, separate refactor
- **CSP header** — requires frontend audit of inline scripts and CDN dependencies

---

## Version Bump

`internal/version/version.go`: `"6.3.23"`
