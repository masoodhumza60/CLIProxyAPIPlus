# Freebuff protocol constants (pinned)

Source: `github.com/CodebuffAI/freebuff` at ref `a8d0795cb07ac93f1855adc26ea7c669b617ccea`.

**Scope reminder (plan §0).** This client exists to exercise the Freebuff protocol
against the local mock in `mock.go`. It is not a route to the live service: there is
no configurable base URL, no credential acquisition, no fingerprint spoofing, and no
system-prompt marker. Every constant below is recorded so the mock and the client
agree on literals instead of inventing them.

Anything not verified against a source file is marked `UNCONFIRMED` and **must not be
guessed in code**. Where a value is `UNCONFIRMED`, the client must omit the field
rather than fabricate it.

## 1. Wire headers — `common/src/constants/freebuff-models.ts`

Comment at 3575: "Wire headers for the free-mode session endpoints
(`/api/v1/freebuff/session`). Shared so the server handlers and every client
(CLI, desktop) agree on the exact strings instead of redefining literals."

| Constant | Literal | Line |
| --- | --- | --- |
| `FREEBUFF_INSTANCE_HEADER` | `x-freebuff-instance-id` | 3578 |
| `FREEBUFF_REUSE_INSTANCE_HEADER` | `x-freebuff-reuse-instance-id` | 3580 |
| `FREEBUFF_MODEL_HEADER` | `x-freebuff-model` | 3581 |
| `FREEBUFF_WALLET_SPEND_LIMIT_HEADER` | `x-freebuff-wallet-spend-limit` | 3582 |
| `FREEBUFF_ACTING_USER_HEADER` | `x-freebuff-acting-user-id` | 3593 |
| `FREEBUFF_PRIVILEGED_USER_HEADER` | `x-freebuff-privileged-user` | 3600 |
| `FREEBUFF_AD_CLICK_ID_HEADER` | `x-freebuff-bfcid` | 3612 |
| `FREEBUFF_AD_CLICK_ID_COOKIE` | `bfcid` | 3615 |
| `FREEBUFF_INCLUDE_UNUSED_RATE_LIMITS_HEADER` | `x-freebuff-include-unused-rate-limits` | 3620 |
| `FREEBUFF_COMPACT_SESSION_HEADER` | `x-freebuff-compact-session` | 3625 |
| `FREEBUFF_MULTI_SESSION_HEADER` | `x-freebuff-multi-session` | 3628 |
| `FREEBUFF_HEARTBEAT_HEADER` | `x-freebuff-heartbeat` | 3634 |
| `FREEBUFF_TAKEOVER_INSTANCE_HEADER` | `x-freebuff-takeover-instance-id` | 3649 |

Only `FREEBUFF_COMPACT_SESSION_HEADER` and `FREEBUFF_HEARTBEAT_HEADER` are exercised by
this client; the rest are recorded for completeness.

`UNCONFIRMED` — named in the plan's research but **not found** in the grep of
`freebuff-models.ts`, so their literals are unknown and they are **not** sent:

- `FREEBUFF_PURCHASE_CONTINUITY_HEADER`
- `FREEBUFF_DESKTOP_ATTEMPT_HEADER`
- the first-tab-discount header (whatever `FIRST_TAB_DISCOUNT_HEADER` resolves to)
- `freebucksTimeZoneHeaders()` and `clientEnvironmentHeaders()` output

`FREEBUFF_ACTING_USER_HEADER` and `FREEBUFF_PRIVILEGED_USER_HEADER` are **server-to-server
trusted** headers, honored only when the caller authenticates as the Freebuff Web service
account (lines 3590–3599). This client never sets them.

## 2. Paths — `common/src/constants/freebuff-models.ts`

| Constant | Literal | Line |
| --- | --- | --- |
| `FREEBUFF_SESSION_ADMISSION_PATH` | `/api/v1/freebuff/session/admission` | 3585 |
| `FREEBUFF_SESSION_REUSE_PATH` | `/api/v1/freebuff/session/reuse` | 3587 |

Combined with the polling endpoints from `cli/src/utils/freebuff-session-api.ts`:

| Method | Path |
| --- | --- |
| `POST` | `/api/v1/freebuff/session/admission` (admit) |
| `GET` | `/api/v1/freebuff/session` (poll / heartbeat) |
| `DELETE` | `/api/v1/freebuff/session` (release) |
| `DELETE` | `/api/v1/freebuff/session/attempt` (release with attempt id) |

`FREEBUFF_SESSION_UNSUPPORTED_MESSAGE` (line 3588), the copy used when a server predates
the admission route: "This server cannot safely start or resume your session yet. Reload
or update Freebuff and try again shortly. No purchase was made."

## 3. `FREEBUFF_GATE_CODES` — `common/src/types/freebuff-session.ts:1228`

This is the authoritative status contract for the chat-completions gate and the single
most important table in this file. Upstream's comment is explicit that **both** halves
must match to identify a gate: "A rejection is identified by its `error` code paired with
its HTTP status, never by its `message` … 409/410 are ordinary provider outcomes on their
own, and the codes are generic enough that an upstream error body can echo one, so a
status-only or code-only test lets an unrelated failure impersonate the gate."

| Code | HTTP status | `endsTheSession` |
| --- | --- | --- |
| `waiting_room_required` | 428 | true |
| `session_expired` | 410 | true |
| `session_superseded` | 409 | true |
| `session_model_mismatch` | 409 | true |
| `session_limit_reached` | 409 | false |
| `waiting_room_queued` | 429 | false |
| `model_unavailable` | 410 | false |

`endsTheSession: true` means the caller's session row is GONE; every such code has the
same recovery (forget the window and re-admit on the same instance id), which is why
upstream collapses them into one state. `false` is a refusal the session survives.

Note `waiting_room_required` is **428**, not 403/503 — a distinction the third-party
Freebuff2API gets wrong, since it treats the waiting room as a plain 503.

## 4. Session response union — `common/src/types/freebuff-session.ts`

`FreebuffSessionServerResponse` (line 1170) = `FreebuffSessionAdmissionResponse` | the
`superseded` variant. `FreebuffSessionAdmissionResponse` starts at line 836.

Documented `status` members and their source lines:

| `status` | Line |
| --- | --- |
| `first_tab_discount_changed` | 838 |
| `consent_required` | 843 |
| `none` | 852 |
| `active` | 893 |
| `ended` | 926 |
| `country_blocked` | 960 |
| `model_locked` | 972 |
| `model_unavailable` | 979 |
| `banned` | 1055 |
| `ip_capped` | 1064 |
| `rate_limited` | 1078 |
| `spend_limited` | 1117 |
| `purchase_claim_released` | 1129 |
| `purchase_in_use` | 1133 |
| `purchase_capacity` | 1133 |
| `premium_slot_taken` | 1150 |
| `superseded` | 1178 |

`purchase_in_use` and `purchase_capacity` share one variant (line 1133). `superseded`
lives on `FreebuffSessionServerResponse` rather than the admission response.

Related unions:

- `FreebuffCountryBlockReason` (line 737): `country_not_allowed`, `recent_limited_country`,
  `anonymized_or_unknown_country`, `anonymous_network`, `missing_client_ip`,
  `unresolved_client_ip`, `ip_privacy_lookup_failed`.
- `FreebuffIpPrivacySignal` (line 750): `anonymous`, `vpn`, `proxy`, `tor`, `relay`,
  `res_proxy`, `hosting`, `service`.
- `FreebuffWalletSpendLimit` (line 829): `number | 'session'`.
- `FreebuffGateCode` (line 1254) = `keyof typeof FREEBUFF_GATE_CODES`.

## 5. API-key surface — `packages/agent-runtime/src/llm-api/codebuff-web-api.ts`

Headers on every request: `Authorization: Bearer <CODEBUFF_API_KEY>` **and**
`x-codebuff-api-key: <CODEBUFF_API_KEY>`, plus `Content-Type: application/json`.
Base URL resolved at call time from `NEXT_PUBLIC_CODEBUFF_APP_URL`.

| Endpoint | Request | Success |
| --- | --- | --- |
| `POST /api/v1/token-count` | `{messages, system?, model?, tools?}` | `{inputTokens}` |
| `POST /api/v1/web-search` | `{query, depth?, repoUrl?}` | `{result, creditsUsed}` |
| `POST /api/v1/docs-search` | `{libraryTitle, topic?, maxTokens?, repoUrl?}` | `{documentation, creditsUsed}` |
| `POST /api/v1/gravity-index` | arbitrary JSON | JSON + `creditsUsed` |

Policy to mirror: `MAX_RETRIES = 3`, `RETRY_BASE_DELAY_MS = 1000` exponential backoff,
retry only on `{408, 429, 500, 502, 503, 504}`, 30 s per-request timeout. Error text is
extracted in order `json.error` → `json.message` → raw text → `'Request failed'`.

Only `token-count` is implemented here; the other three are recorded, not built.

## 6. Cost mode and agents — `common/src/constants/free-agents.ts`

- `FREE_COST_MODE = 'free'` (line 86).
- `FREE_MODE_AGENT_MODELS: Record<string, Set<string>>` (line 694) — agentId → allowed
  models. Shape: `'base2-free-deepseek': new Set([FREEBUFF_DEEPSEEK_V4_PRO_MODEL_ID])`.
  The set is described as existing to prevent "abuse by users trying to use arbitrary
  agents for free" (plan §research). This client resolves agent↔model **for the mock
  fixture only**; it does not attempt to satisfy the real gate.
- `FREEBUFF_ROOT_AGENT_IDS` (line 383), `FREEBUFF_ROOT_AGENT_ID_BY_MODEL: Record<string, string>`
  (line 520, model → root agent), `FREE_TIER_AGENTS` (line 983:
  `file-picker`, `file-picker-max`, `file-lister`, `researcher-web`, `researcher-docs`).
- `isFreebuffRootAgent` (line 999) requires `publisherId === 'codebuff'` when a publisher
  is present, i.e. agent ids may be publisher-qualified.
- Verified agent-id → model-id pairs usable as mock fixtures: `base2-free-deepseek` →
  `deepseek/deepseek-v4-pro`; `base2-free-deepseek-flash` → flash variant; `base2-free-minimax-m3`
  → `minimax/minimax-m3`; `file-picker` → literal `'google/gemini-2.5-flash-lite'`
  (line 901, a string literal, not a constant).
- `getFreebuffRootAgentIdForModel(model)` falls back to `'base2-free'` — **UNCONFIRMED**,
  the resolver body was not read; do not rely on the fallback.

Model ids confirmed as literals in `common/src/constants/freebuff-model-ids.ts`:

| Constant | Literal | Line |
| --- | --- | --- |
| `FREEBUFF_DEEPSEEK_V4_PRO_MODEL_ID` | `deepseek/deepseek-v4-pro` | 10 |
| `FREEBUFF_MINIMAX_M3_MODEL_ID` | `minimax/minimax-m3` | 11 |

`UNCONFIRMED` — every other `FREEBUFF_*_MODEL_ID` referenced by `FREE_MODE_AGENT_MODELS`
(`FREEBUFF_DEEPSEEK_V4_FLASH_MODEL_ID`, `FREEBUFF_MIMO_V25_MODEL_ID`,
`FREEBUFF_GLM_V52_MODEL_ID`, `FREEBUFF_GPT_5_6_LUNA_MODEL_ID`, `FREEBUFF_FABLE_5_1_MODEL_ID`,
the Qwen/Grok/Gemini/Claude rows, …) resolves through other modules
(`model-config`, `freebuff-model-entitlements`, `freebuff-solar-promo`, `freebuff-sol-promo`)
and its literal was **not** verified. Two literals that *were* seen directly elsewhere:
`FREEBUFF_GLM_V52_MODEL_ID = 'z-ai/glm-5.2'` and
`FREEBUFF_DEEPSEEK_V4_FLASH_FIREWORKS_MODEL_ID = 'fireworks/deepseek-v4-flash'` (both in
`freebuff-models.ts`), but the DeepSeek Fireworks id is explicitly marked a **legacy** id
that new code must not use.

The mock therefore hardcodes its own small model→agent table rather than trying to mirror
a catalog that is a moving target.

## 7. Session timings

| Constant | Value | Line |
| --- | --- | --- |
| `FREEBUFF_SESSION_HEARTBEAT_INTERVAL_MS` | `45_000` | 3638 |
| `FREEBUFF_SESSION_GRACE_MS` | `30 * 60 * 1000` | 3656 |

`SESSION_FETCH_TIMEOUT_MS = 20_000` comes from `cli/src/utils/freebuff-session-api.ts`
(plan §1b) and is a credential-acquisition timeout, which `AGENTS.md` permits.

## 8. Not used, by design

`FREEBUFF_ROOT_SYSTEM_PROMPT_OPENINGS` (`free-agents.ts:1019`) holds the five verbatim
opening sentences a free-mode root request must match, and
`hasFreebuffRootSystemPromptOpening` (line 1048) is a byte-exact prefix test used to
enforce them. **This client never emits these strings.** Reproducing them is exactly the
impersonation that plan §0 guardrail 4 forbids. They are recorded here only so a future
reader can see they were found and deliberately declined.

The literals are recorded in the source at lines 1022, 1026, 1028, 1034 and 1037; they are
deliberately **not** transcribed into this file, so that no code in this repository can
copy them.
