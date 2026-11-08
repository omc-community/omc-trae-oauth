# Contributing to OMC Trae OAuth

`omc-trae-oauth` is part of the [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa)
ecosystem. It provides native Trae and Trae CN OAuth sign-in, model discovery,
OpenAI-compatible execution, and normalized quota for CLIProxyAPI (CPA).

We welcome issues, feedback, and pull requests. Please adhere to the ecosystem
standards described below.

---

## Local Development

### Prerequisites

- Go 1.27.1+ with `CGO_ENABLED=1`
- `gcc` (or an equivalent C toolchain for c-shared builds)
- Docker (optional, for end-to-end testing with a local CPA container)

### Building and Testing

All dependencies are pre-cached after `go mod download`; tests run without network
access:

```bash
cd go
go mod download

# Run unit tests and static checks offline
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...

# Build the native shared library for your current platform
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-trae-oauth.so .
```

Use `.dylib` on macOS and `.dll` on Windows. `build/` is ignored by git.

---

## Commit Guidelines

This project follows the **Oh-My-CPA Conventional Commits** standard. Commits must
be atomic, focused, and independently verified.

### Subject Format

```text
<type>(<scope>): <imperative subject>
```

- **Types**:
  - `feat`: A new user-facing feature or configuration option.
  - `fix`: A bug fix or behavioral correction.
  - `refactor`: Code change that neither fixes a bug nor adds a feature.
  - `perf`: A code change that improves performance.
  - `test`: Adding missing tests or correcting existing tests.
  - `docs`: Documentation-only changes.
  - `chore`: Maintenance tasks, build tooling, or release preparations.

- **Scopes**:
  - `oauth`: Sign-in flows, portal selection, token exchange, device binding, and
    refresh.
  - `models`: Model catalogue discovery, fallback lists, and model info mapping.
  - `executor`: Request building, the `llm_utils_chat` envelope, stream pumps, and
    SSE framing.
  - `quota`: Usage reads and quota normalization.
  - `config`: Plugin configuration fields, parsing, and validation.
  - `registry`: Release packaging and plugin store publication.
  - `readme`: Project overview, documentation links, and branding.
  - `tooling`: CI workflows, build scripts, and release packaging.
  - `release`: Release tags and version bumps.

### Commit Body Structure

Each non-trivial commit body should contain:

1. **Why**: The problem, bug symptom, or user need that prompted the change.
   Reference upstream issues or error messages where applicable.
2. **What changed**: The concrete architectural or behavioral changes made across
   the affected subsystems.
3. **Boundaries / Exclusions**: What was intentionally not changed or left out of
   scope.
4. **Verification**: A clear record of tests run, edge cases exercised, and live
   verification results. Say plainly when something was only tested offline.

#### Example

```text
fix(executor): keep a lapsed sign-in on its own status inside an OK stream

Trae can answer a 200 SSE stream whose first event is an error. The pump used
to open the client stream first, so that refusal arrived as HTTP 200 and ended
in an error frame, which the client read as a truncated answer.

Read up to the first answer event before handing the stream to CPA, and map
any failure found there through classifyUpstream so the client receives the
real status.

Verification:
- Go test suite: TestClassifyUpstream, TestExecuteMapsLapsedSignIn.
- Live verification: not run for this change; the lapsed path needs an expired
  account, so the offline tests stand in for it.
```

---

## Testing & Quality Standards

- **Lowest layer first**: Write regression tests at the lowest possible layer
  (`go/*_test.go`). A bug fix must include a test that reproduces the failure
  without the fix.
- **Offline & hermetic**: Unit tests must pass with `GOPROXY=off GOSUMDB=off`.
  Never make live network calls in unit tests; use `fakeSender` for HTTP and inject
  the stream seams (`readStreamChunk`, `emitStreamFrame`, `openHTTPStream`).
- **No flaky sleeps**: Use synchronous channels or dependency injection instead of
  `time.Sleep`.
- **No real accounts**: A live Trae or Trae CN login is operator verification, not
  a unit test. Record it in the commit body when you run one, and never commit the
  tokens, device ids or auth files it produces.
- **Two portals, one path**: A sign-in test should cover both entries, including the
  Trae CN case where the callback is completed and the device-bound exchange runs.
- **Callback lifecycle**: A test that starts a login must close its flow (see
  `stateFlow` in `auth_test.go`) so the listener does not outlive the test.
- **CPA ABI safety**:
  - Catch panics and return `pluginabi.Error` with machine codes and HTTP status.
  - Do not expose raw token bytes, auth directory paths, or upstream network errors
    to client-facing responses.
  - Model IDs are Trae's `config_name` values. Keep them as Trae spells them; CPA
    routing is case-sensitive, and lookup accepts a case-insensitive name as an
    alias.

---

## Documentation & Language Rules

- **Code & technical prose**: Code comments, commit messages, and primary technical
  documentation (`README.md`, `docs/USAGE.md`, `CONTRIBUTING.md`, `NOTICE.md`) are
  written in English.
- **Chinese translations**: User-facing documentation provides a Chinese mirror
  (`README.zh-CN.md`). Keep the English and Chinese versions in sync.
- **Domain vocabulary**: Preserve exact upstream and CPA terminology:
  - Provider identifier: `trae`
  - Plugin identifier: `omc-trae-oauth`
  - Portal names: `Trae` (`trae`), `Trae CN` (`trae-cn`)
  - Credential fields: `site` (the signing portal), `oauth_provider` (the same value)
  - Token names: Cloud-IDE-JWT, refresh token, device proof
- **No unverified claims**: State which artifacts and behaviours were observed, and
  mark anything that was only exercised offline as such.

---

## Security & Secrets

- **Zero credential leaks**: Never commit Cloud-IDE-JWTs, refresh tokens, device
  private keys, device ids, or `.json` auth files.
- `.trae-oauth-state.key`, `.trae-oauth-*.device.json` and
  `.trae-oauth-*.callback.json` live in the auth directory and are gitignored. Test
  fixtures must use synthetic ids or an ephemeral `t.TempDir()`.
- The install state key seals login state; do not log it, shorten it, or replace it
  with a value derived from public input.
- The callback listener binds to `127.0.0.1` only. Do not widen it, and do not
  accept a callback for a flow whose token does not match.
- Report security issues privately through GitHub Security Advisories or by
  contacting the maintainer directly.
