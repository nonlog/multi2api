# WorkBuddy2API built-in Providers

This directory belongs to the existing **WorkBuddy2API** process and Docker
image. It is **not** another public 2API gateway, separate Docker service, or
Magpie dependency. Go supervises a Bun subprocess listening exclusively on an
owner-private Unix socket. The same existing API key protects every HTTP
entrypoint; no additional port or domain is opened.

## Pinned reference implementations and attribution

Source: [magpie-community/plugins](https://github.com/magpie-community/plugins)
at commit `2e580cfead8c5fe10add40d749a29a07803f745d`.

| Bundled source | Upstream package | Bundled version | License |
| --- | --- | --- | --- |
| `magpie-plugins/zcode/` | `@magpie-community/opencode-zcode-auth` | 0.1.39 | MIT |
| `magpie-plugins/qoder/` | `@magpie-community/opencode-qoder-auth` | 0.2.50 | MIT, includes MIT notice `LICENSE-CLIProxyAPI` |
| `magpie-plugins/codearts/` | `@magpie-community/opencode-codearts-auth` | 0.2.6 | MIT, see bundled `LICENSE` |

Vendored packages are verbatim. Their copyrights and authorship remain with
their upstream owners. WorkBuddy2API's `provider-host/bridge.mjs` adapts their
native protocols; it is separate code and does not replace their algorithms.

## Configuration

Set `providers.enabled=true` and `providers.auth_file=./data/provider-auth.json`
in the existing WorkBuddy2API `config.json`. The optional Bun runtime is
bundled in the **same** production image, and no provider worker starts when
the feature is disabled. On non-Docker hosts the user must install Bun.

The credential JSON uses the Magpie `plugin-auth.json` account-key layout:
`zcode`, `qoder`, `qoder-cn`, `qwenwork`, `codearts`, plus optional
per-account `provider#id` entries. Copy **only those providers' credential
entries**, not credentials belonging to other services, to the dedicated
WorkBuddy2API file. Permissions **0600**, stored in the existing private
data volume and excluded from Git/Docker build context.

All original Magpie **quota queries and sign-in/check-in hooks** are retained:

| Provider | Quota usage | Daily check-in |
| --- | --- | --- |
| ZCode | Coding Plan, free gift plans and team allowances | Gift plans, when signed in with a compatible ZCode session |
| Qoder / Qoder CN | Credit and add-on usage | Yes, **only with the authentic device identity supplied by the official client runtime** |
| QwenWork / QwenWork Enterprise | Account-context / wallet credits | Not supported by upstream plugin; displayed as unsupported |
| CodeArts | Plan usage, benefits and batch credits | Yes, with its upstream credential/signing mechanism |

The admin panel can query balances on demand, inspect daily results and run
manual check-in. Automatic daily check-in is **off until
`providers.auto_checkin=true`**; when enabled, the private worker attempts
each supported account at most once per Beijing calendar day, keeps state in
`data/provider-maintenance.json` and never forges missing Qoder device
identity. If the official Qoder CLI/runtime is absent on Linux, its check-in
returns an explanatory `ineligible` result rather than a fake success.

The plugin worker may refresh credentials and atomically rewrite this private
copy; it does not write to Magpie's original store. Never point both services
at a single mutable token store. All per-account refreshes continue to use
the original plugins' single-flight and signature logic.

Downstream model examples (actual model names come from each signed-in
provider's model catalogue):

- `zcode:glm-5.3` (Anthropic Messages upstream; converted to/from Chat API)
- `qoder:<model>` and `qoder-cn:<model>`
- `qwenwork:<model>` and `qwenwork-enterprise:<model>`
- `codearts:<model>`

All requests use the existing `/v1/chat/completions`, `/v1/responses` and
`/v1/messages` endpoints. `GET /v1/models` includes the provider catalogues,
and authenticated `GET /v1/providers` displays safe account counts. There is
no second API host or key.

## Known boundaries

- **No automatic vendor sign-in or interactive OAuth UI yet.** Existing
  credentials can be copied from the locally deployed Magpie plugin store
  without re-login, subject to actual expiry and successful request tests.
- ZCode Chat→Messages conversion supports text, image, function tools, tool
  results, and streamed arguments. Vendor-specific image/audio formats,
  proprietary reasoning signatures, and external server-side tools are not
  magically supported.
- Only providers with **imported nonempty access/refresh credentials** appear
  in `/v1/models`; that does not itself prove that a token is still valid.
  A model-listing failure may fall back to the plugin's static catalogue,
  which is not proof of live model entitlement. Verify a selected model
  with a small real request before treating it as available.
- A provider's signing protocol or permission changes can still cause 401/403
  or account restrictions. Using a plugin does not bypass the vendor's rules.
