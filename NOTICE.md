# Source note

The plugin's Trae protocol support is ported from the MIT-licensed
`packages/trae` plugin in `magpie-community/plugins`. The Go implementation in
`go/` is independent code, but its wire behaviour follows that source and the
relays it credits:

- **Browser sign-in**: the `www.trae.ai` / `www.trae.cn` authorization page, its
  `client_id`, the callback on `127.0.0.1`, and the `userJwt` / `userInfo` query
  parameters it returns. Trae's own IDE client is the original reference.
- **Token exchange and renewal**: `POST /cloudide/api/v3/trae/oauth/ExchangeToken`
  and Trae CN's device-bound `POST /trae/api/v3/oauth/ExchangeToken`, including the
  `DeviceInfo` fields and the `DeviceProof` signature over method, path, client,
  refresh token, timestamp and nonce with a P-256 key.
- **Model list and chat**: `POST /api/ide/v1/batch_get_detail_param` and
  `POST /api/agent/v3/llm_utils_chat`, the IDE headers, the `chat_v3` /
  `solo_work_*` / `solo_agent*` / `solo_coder` functions, the `__dev` / `__max`
  model names, and Trae's SSE events.
- **Usage pages**: Trae CN's `ide_user_ent_usage` credits page and the
  international `user_current_entitlement_list` dollar page.
- **Supporting references**: `wangqi233/trae2api` and
  `autumnsentiment/Trae2api-cn` (the relays `packages/trae` credits for the CN
  protocol and the realm table), and the `packages/trae` README for the observed
  behaviour of the international deployment.

Trae, Trae CN and the Trae logo belong to their owner. This project is not
affiliated with or endorsed by Trae.

## Logo mapping

- `assets/logo.png` is this plugin's own face in a store: the Trae mark drawn on the
  OMC sticker the ecosystem's plugins share. It identifies the plugin rather than the
  service, and no sign-in card uses it.
- `assets/trae.svg` is Trae's monochrome mark. It is the logo of the `trae` login
  entry.
- `assets/trae-color.svg` is the same mark in Trae's green. It is the logo of the
  `trae-cn` login entry, so the two sign-in cards are distinguishable at a glance.

`go/assets/` holds the copies the plugin embeds and serves, and they must stay
byte-identical to the files under `assets/`; the test suite compares them.

Trae names and logos are used only to identify the service the plugin signs in to.
