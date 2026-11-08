# Usage

## Requirements

- CLIProxyAPI v8 with dynamic plugin support and the quota provider.
- A CPA build that accepts the plugin's declared ABI and RPC schema: the plugin
  registers ABI 1 and schema 10 from its pinned SDK, and an older host refuses it at
  registration with `plugin schema version 10 is not supported`.
- **CPA v8.0.49 or later** to load the plugin (schema 10 first shipped there), and
  **CPA v8.0.51 or later** for the separate Trae and Trae CN sign-in cards:
  per-entry name and logo first shipped in v8.0.51. On an older host both entries
  still work; a panel that predates the field shows one card per *identifier*
  decorated with the plugin's own name and logo.
- A browser on the CPA host, or loopback reachable through a forwarded port. Trae's
  authorization page only sends its callback to `127.0.0.1`.
- Go 1.27.1 for local development. Release builds are produced by CI.

Sign-in and a live chat are not covered by an offline test. The unit tests drive
every upstream call through a fake sender, so the requests and responses the plugin
builds are checked, but a live browser flow needs real Trae and Trae CN accounts.
Verify a login against your own accounts before relying on a build, and report what
you observe.

## Install from the plugin store

```yaml
plugins:
  enabled: true
  dir: /path/to/plugins
  store-sources:
    - https://raw.githubusercontent.com/omc-community/omc-plugins/main/official.json
  configs:
    omc-trae-oauth:
      enabled: true
      max_concurrency: 3
      client_version: 0.1.69
      max_tokens: 32000
      # trae (default) or trae-cn. Only a fallback: a login request that names
      # an entry picks that entry's portal.
      site: trae
```

Use CPA's plugin store to install `omc-trae-oauth`, or call:

```text
POST /v0/management/plugin-store/omc-trae-oauth/install
POST /v8/management/plugins/store/omc-trae-oauth/install
```

Add `?version=<version>` to install a specific release. The store requires the
release to carry `omc-trae-oauth_<version>_<os>_<arch>.zip` and `checksums.txt`,
and verifies the archive against the checksum before unpacking it.

The archive contains one library file and is installed under
`<plugins-dir>/<os>/<arch>/` as `omc-trae-oauth-v<version>.so`, `.dylib` or
`.dll`, depending on the target platform: the installed version is part of the
file name. CPA also loads a hand-placed unversioned `omc-trae-oauth.so`; that file
carries no version, so a pinned version in `plugins.configs` does not match it.
Restart CPA if it reports that the loaded plugin must be replaced.

Manual layout, for the version you install:

```text
<plugins-dir>/linux/amd64/omc-trae-oauth-v<version>.so
<plugins-dir>/linux/arm64/omc-trae-oauth-v<version>.so
<plugins-dir>/darwin/amd64/omc-trae-oauth-v<version>.dylib
<plugins-dir>/darwin/arm64/omc-trae-oauth-v<version>.dylib
<plugins-dir>/windows/amd64/omc-trae-oauth-v<version>.dll
<plugins-dir>/windows/arm64/omc-trae-oauth-v<version>.dll
```

## Description declaration

A release may publish an optional `<plugin_id>.omc.json` (`schema_version` 1) with
localized descriptions. When it does, the file is `omc-trae-oauth.omc.json` and the
plugin serves the same bytes at:

```text
GET /v0/resource/plugins/omc-trae-oauth/omc.json
```

The parts that matter here:

- `plugin_id` is `omc-trae-oauth`, and `plugin_version` is the release version
  without a leading `v`. A document naming another id or version is ignored, not
  guessed at.
- `i18n.description` may contain any subset of `zh`, `en`, `zh-Hant` and `ms`; each
  value is plain text.
- It declares no menu, settings label or action, and it never changes the plugin
  name, logo or the description CPA's store already shows.
- CPA serves plugin resources without management authentication, and answers 404 for
  them in Home mode. When the resource is unavailable, a reader uses the release
  asset; when neither exists, the description in the official plugin source is the
  result.

The served body is never re-serialized: the plugin embeds the same file that
packaging copies into the release, so served and published bytes are comparable by
hash. Bump the file together with the plugin version.

## Build locally

```sh
cd go
go mod download
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-trae-oauth.so .
```

Name the output with your platform's suffix: `.so` on Linux, `.dylib` on macOS,
`.dll` on Windows. `build/` is ignored by git. Official release artifacts for
Linux, macOS and Windows on both amd64 and arm64 are built by the release workflow
when a `v*` tag is pushed.

## Sign in

The plugin registers **two OAuth login entries**, one per portal:

| Entry identifier | Portal | Sign-in page | Model host |
| --- | --- | --- | --- |
| `trae` | Trae | `https://www.trae.ai/authorization` | `coresg-normal.trae.ai` (SG), `coreva-normal.trae.ai` (US) |
| `trae-cn` | Trae CN | `https://www.trae.cn/authorization` | `trae-api-cn.mchost.guru` |

Both produce the same credential shape (`"type": "trae"`) and use the same
executor. On a panel that supports it, each entry renders as its own card with its
own name and logo (CPA v8.0.51+); the identifier is what CPA routes the login, poll
and callback by.

Start a login through CPA's shared OAuth endpoint, which is the one plugin providers
use:

```text
GET /v8/management/oauth/auth-url?provider=trae
GET /v8/management/oauth/auth-url?provider=trae-cn
```

The caller must supply the management key in the `Authorization: Bearer ...`
header. Do not put the key in a URL. Use the URL returned by CPA to complete the
sign-in in the browser. A request without that header cannot initiate a flow.
Public plugin resources serve only the static `logo.png`, `trae-color.svg`,
`trae.svg` and `omc.json` assets; other paths answer 404 and a non-GET request
answers 405.

Portal resolution order is: the entry the request names, then a `site` value in the
login request's metadata, then the configured `site`, then Trae. The entry wins, so
`provider=trae&site=trae-cn` signs in to Trae. `site` matters only for a login
start that names no entry, for example a host that calls the plugin under its own
identifier; an explicit invalid value there is rejected rather than falling back to
the configuration.

### Flow

1. The plugin creates the account's device (a device id, a machine id and a P-256
   key pair), seals the portal, the expiry and a flow token into CPA's opaque
   state, and opens a callback listener on `127.0.0.1` at a free port. It returns
   the portal's `authorization` page with the IDE's client id, the device ids, the
   IDE build, and `auth_callback_url=http://127.0.0.1:<port>/authorize`.
2. Sign in on the page and allow the sign-in. Trae sends the browser back to the
   listener with `userJwt` (the Cloud-IDE-JWT and refresh token), `userInfo` (the
   account) and, for an enterprise account, the tenant's `host` / `authHost`.
3. The listener writes those values to a private sidecar beside the auth material
   and answers a small confirmation page. The flow stays open until CPA polls, so a
   slow poll cannot miss the callback.
4. The plugin reads the callback, builds the credential, and for Trae CN renews the
   JWT once through the device-bound `ExchangeToken` so the token is bound to the
   device the page was opened with. When Trae refuses that, the page's token is
   kept as it is.
5. CPA saves the auth record. The flow's listener, callback file and device sidecar
   are removed.

The opaque state CPA carries is sealed with AES-GCM under
`<auth-dir>/.trae-oauth-state.key` and expires after 10 minutes, the window Trae's
own client allows. The device sidecar and the callback file are named after a
random flow token, written with mode `0600`, and removed when the flow ends. Keep
the auth directory private and writable.

Poll a pending flow with the state CPA returned:

```text
GET /v8/management/oauth/status?state=<opaque-state>
```

`/v0/management/get-auth-status?state=<opaque-state>` is the v0 alias. Cancel with
`DELETE /v8/management/oauth/session?state=<opaque-state>` (or
`DELETE /v0/management/oauth-session?state=<opaque-state>`). Cancellation is handled
by CPA; the plugin does not implement a separate cancellation endpoint. The
listener is closed when CPA unloads the plugin.

### Remote deployments

Trae accepts no callback other than `127.0.0.1`. The listener is opened on the CPA
host, so the browser must reach that host's loopback. For a remote CPA, run the
browser on the CPA host through a remote desktop, or forward the flow's port (it is
random, so read it from the authorization URL) over SSH and open the URL locally.
The panel's callback-paste field does not help here: Trae's callback carries
`userJwt` / `userInfo`, not a `code`, and CPA's callback endpoint only forwards a
`code`.

### Credential

The saved file is `trae-<hash>.json` and carries `"type": "trae"`, the portal as
`site`, the same value as `oauth_provider`, the account identity, the device
identity, the device key pair, the model host and region the sign-in named, and the
tokens. Its card label is the account's screen name, else the email, else the user
id.

Refresh tokens are single-use, so two concurrent refreshes for the same credential
share one exchange instead of spending the token twice. The plugin asks CPA to renew
about ten minutes before the JWT ends. Trae CN renewals first try the IDE's
device-bound exchange with the account's P-256 key and fall back to the cloudide
endpoint. A refresh refused with `400`, `401`, `403`, code `10101`, or a message
naming a spent, revoked or expired refresh token means the credential needs a new
sign-in. An existing `"type": "trae"` auth file is recognized and republished when
CPA imports it, and the host-managed metadata (note, prefix, priority, proxy, and
the rest) is preserved.

## Models and routing

The plugin reads the account's model list from the IDE endpoint
`POST /api/ide/v1/batch_get_detail_param`, across the chat functions
`chat_v3`, `solo_work_lite`, `solo_agent`, `solo_agent_lite` and `solo_coder`. A
function's list that fails is skipped; if the batch answer names no lists, each
function is asked through `get_detail_param` instead.

- Model IDs are Trae's own `config_name` values (`glm-5.2`,
  `DeepSeek-V4-Flash`, ...). A `vendor/model` prefix is trimmed before lookup, and
  a display name in any casing resolves to the same entry.
- An entry that names a `__dev` model is sent that `model_name`, with its
  `max_tokens` as the output cap.
- An entry that names a `__max` model with a larger context window also publishes
  a `<id>-max` variant (for example `glm-5.2-max`). A Max request is sent under the
  base config name with the `__max` model name and its larger window.
- Reasoning levels come from the entry's `reasoning_effort_config` and are
  advertised in OpenAI's names (`low`, `high`, `xhigh`, ...). A level the model
  does not list is not sent, because Trae rejects it.
- Image support comes from the entry's `multimodal` flag. A model that does not see
  images is sent a text placeholder instead of the image.
- Custom-model slots, the IDE's helper entries (`usage` other than
  `chat_completion`) and switched-off configs are not published. The international
  picker's `is_invisible_to_user` entries are hidden; Trae CN's lists are served
  whole.
- The list is cached per account for five minutes. A failed read keeps the last
  good list; with no cached list at all, the shipped fallback is used. The Trae
  fallback names GPT-5.4, Gemini 3 Flash, Kimi K2.5, MiniMax M2 and DeepSeek V3.2;
  the Trae CN fallback names GLM-5.2, GLM-5, Kimi K2.6, Qwen 3.7 Plus, DeepSeek V4
  Pro and DeepSeek V4 Flash.

Execution is OpenAI chat completions in and out. The plugin sends Trae's
`llm_utils_chat` request: the conversation as `{role, content:[...]}` messages,
the function that lists the model, `config_name` / `model`, the optional
`model_name`, a stable `session_id`, and the tools as native functions with
`parameters` as a JSON string. A past tool call goes back as Trae's own
`assistant.tool_calls` turn and its result as the `tool` turn after it; a request
that names no tools keeps the text `<tool_call>` form.

Trae answers in its own SSE events (`output`, `token_usage`, `done`, `error`). The
plugin turns them into OpenAI chunks or one completion, including reasoning
content, native tool calls, tool calls written as `<tool_call>` JSON or GLM's
`arg_key`/`arg_value` template, and usage with `cached_tokens` from Trae's
prompt-cache counts.

If Trae answers `400` with code `4001`, `4023` or `1005`, the model is tried
through another chat function once per function, and the one that worked is used
for later turns. Other refusals are mapped, not retried.

Token counting is not supported and answers `501`. Trae documents no token-count
endpoint, and a guess would misbill clients that trust it.

## Quota

```text
GET  /v8/management/plugins/omc-trae-oauth/quota
POST /v8/management/plugins/omc-trae-oauth/quota
{"auth_index":"<auth index>"}
```

`GET` describes the provider: `trae`, display name `Trae`, reset not supported.
`POST` fetches one credential's usage with the account's IDE headers.

- **Trae CN** reads `POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage`
  and sums the `credits_limit` and `credits_amount` of the account's entitlement
  packs. A `-1` limit is reported as unlimited. The plan is `Credits` when the
  answer says the account is on credit billing, else `Free`. The bucket carries the
  remaining fraction, a `N / M credits left` description and the soonest pack
  expiry as its reset time.
- **Trae** reads
  `POST https://api-sg-central.trae.ai/trae/api/v1/pay/user_current_entitlement_list`
  (or the US host `api-us-east.trae.ai` for a US account) and sums the pack's
  `basic_usage_limit` and `bonus_usage_limit` against `basic_usage_amount` and
  `bonus_usage_amount`. The bucket reports dollars and the summary metric uses the
  `usd` currency format.

Resets happen on the vendor website; a reset request answers with that message
instead of performing one.

## Error handling

Upstream refusals are classified by Trae's own codes and status, and the client
sees the mapped status:

| Upstream | Client sees |
| --- | --- |
| HTTP `401` / `403`, code `1001` (and `20101` on Trae), or a "not logged in" / "token expired" message | `401`, sign-in expired, sign in again |
| HTTP `429`, codes `4008`, `1005` (and `4011` on Trae), or a quota / credit / limit message on `403` | `429` |
| HTTP `400` with code `4001`, `4023` or `1005` | Tried through another chat function; `502` if none takes it |
| Other HTTP `400` | `400` with Trae's code and message |
| HTTP `403` that is neither a lapsed sign-in nor a quota refusal | `403` |
| Other upstream statuses, or a transport failure | `502` / `503` |

Other behaviour worth knowing:

- A refusal that arrives inside an `OK` stream frame keeps its real status: streaming
  requests read up to the first answer event before any chunk reaches the client.
- A non-streaming answer with no text, no reasoning and no tool call is reported
  as `502` rather than an empty `200`.
- A tool call written as text that cannot be read, or is cut short at the end of the
  stream, ends the turn as `502` instead of being passed on as the answer.
- A stream chunk is emitted as one OpenAI `data:` frame, and the stream ends with
  `data: [DONE]`.
- Panics are caught at the plugin boundary and answered as a machine-coded error, and
  client-facing messages never carry token bytes or internal paths.

## Troubleshooting

- **The provider is absent:** enable the global `plugins` switch and
  `plugins.configs.omc-trae-oauth.enabled`, then restart CPA if the library was
  replaced.
- **`unknown login entry: use trae or trae-cn` or `invalid login site: use trae or
  trae-cn`:** the login was started with another provider key or with an invalid
  `site` value. Start it from the `trae` or `trae-cn` entry, or set the native
  `site` field to one of those two values.
- **Login stays pending:** complete the browser page for the entry you started. The
  browser must reach the CPA host's `127.0.0.1`; a callback sent to another machine
  never arrives. The flow expires after 10 minutes; start a new sign-in after that.
- **The browser says the callback cannot be reached:** the flow's listener was
  closed (the flow expired, was cancelled, or the plugin was reloaded), or the
  browser is on another machine. Start a new sign-in from the CPA host.
- **`Trae sign-in has expired; sign in again`:** the refresh token was refused, so
  the stored credential cannot be renewed. Start a new sign-in.
- **No models appear:** the list read failed and the shipped fallback took over, or
  the credential is not active. Check CPA logs for `model_error` and the credential
  status.
- **`model ... is not available from Trae now`:** the requested name is not in the
  account's list. List the models from the panel and use the ID it shows; a display
  name or a `vendor/` prefix is also accepted.
- **Trae answers `4027` on a Kimi model with tools:** the tools' JSON Schema is not
  in Moonshot's accepted shape. This plugin does not rewrite schemas for Kimi;
  retry without tools or with another model.
- **Login fails with `CPA auth directory unavailable`:** CPA reported no auth
  directory to the plugin. Set one, and keep it writable: it holds the state key,
  the device sidecar and the callback sidecar.
