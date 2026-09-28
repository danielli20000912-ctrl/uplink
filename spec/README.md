# uplink v0 — specification (draft)

uplink is a small read-only HTTP interface that an API provider (the **publisher**) exposes to the resellers that buy from it (the **consumers**). With the API key it already uses for model calls, a consumer can discover the publisher's models, the prices that apply to that key, its balance, every settled charge, and model health.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be interpreted as described in RFC 2119.

Field definitions live in [`schemas/`](schemas/) (JSON Schema 2020-12) and the HTTP surface in [`openapi.yaml`](openapi.yaml). This document holds the rules that schemas cannot express. When they disagree, the schemas define field shapes and this document defines behavior.

## 1. Transport and authentication

1. Publishers MUST serve uplink over HTTPS.
2. Every endpoint except discovery MUST authenticate with `Authorization: Bearer <api key>`, using the same key that works for model calls. Publishers MUST NOT accept the key in the query string.
3. **Key scoping.** Every authenticated response MUST reflect only what the calling key can use and has spent. A key MUST NOT be able to read another key's usage, even if both belong to the same user, unless the publisher documents otherwise with an `x-` extension.
4. An unknown, disabled or expired key MUST get `401` with code `invalid_api_key`. A valid key whose owner has not been granted a capability MUST get `403` with code `permission_denied`.

## 2. Discovery

1. Publishers MUST serve the discovery document at `GET /.well-known/uplink.json` without authentication. It SHOULD be cacheable for at least 5 minutes.
2. `endpoints` gives the path of each endpoint, relative to the host that served discovery. Consumers MUST use these paths rather than assume the defaults.
3. `capabilities` lists what the publisher implements. `catalog` is mandatory. A consumer MUST NOT call an endpoint whose capability is absent. `errors.redacted` asserts that model-call errors (not only uplink errors) follow rule 10.3.
4. `currency` is the only currency this publisher uses. Every monetary value in every response MUST be in it.
5. `cost_precision` is the number of decimal places of every `cost` the publisher reports. `cost_quantum`, if present, is the smallest amount the publisher actually deducts. For example, a platform that deducts whole internal quota units, 500,000 to the dollar, sold at ¥1 per dollar, has a quantum of `0.000002`.

## 3. Numbers and money

1. Every monetary value, price and multiplier is a **decimal string** (`"0.25"`), never a JSON number. Consumers MUST parse them as exact decimals.
2. Prices are absolute amounts in the publisher's currency. uplink has no concept of ratios, multipliers relative to an official price, or internal quota units. Publishers that store prices that way MUST convert before publishing.
3. Token prices are per 1,000,000 tokens.

## 4. Catalog

1. `GET /uplink/v0/catalog` MUST list exactly the models the calling key can call, each with the price that key will be charged.
2. The response MUST NOT contain the names of groups, channels, upstream accounts or upstream providers.
3. `catalog_version` MUST change whenever any other field in the response changes, except `generated_at`. The publisher MUST send it as a strong `ETag` too, and MUST answer `304` to a matching `If-None-Match`.
4. `pending_changes` lists announced future prices. When `effective_at` passes, the publisher MUST move the change into `models` and remove it from `pending_changes`.
5. A model with `price.unit = "custom"` is callable but its price cannot be expressed in v0. Consumers MUST NOT derive a resale price from it automatically.
6. `capabilities.cache_affinity` names the request field the publisher uses to keep a conversation on the same backend, which drives prompt-cache hit rate. Consumers that proxy requests SHOULD forward that field unchanged.

## 5. Token buckets

Usage is reported in **disjoint** buckets. No token is counted in more than one billed bucket.

| Bucket | Meaning |
|---|---|
| `input` | Input tokens that were neither read from nor written to a cache |
| `cached_input` | Input tokens read from a cache |
| `cache_write` | Input tokens written to a cache with the default lifetime |
| `cache_write_1h` | Input tokens written to a cache with a 1-hour lifetime |
| `output` | Output tokens, including reasoning |
| `reasoning` | Informational subset of `output`; never billed on its own |

Publishers MUST convert from their native usage shape. For example, OpenAI-style `prompt_tokens` includes cached tokens, so `input = prompt_tokens − cached_tokens − cache writes`. Anthropic-style `input_tokens` already excludes cache reads and writes, so it maps to `input` directly.

**Context tokens** = `input + cached_input + cache_write + cache_write_1h`.

## 6. Cost

Every conforming implementation MUST compute the same cost for the same price, time pricing, record and precision. [`vectors/cost.json`](vectors/cost.json) holds the test cases, and implementations MUST reproduce every one exactly.

**Step 1: raw cost.**

- `per_1m_tokens`:
  1. Select the level. Among `tiers`, take the one with the largest `above_tokens` that is strictly less than the context tokens. If none qualifies, use the base prices. The whole request is billed at the selected level (`basis: whole_request`, the only basis in v0).
  2. Fill omitted prices within that level. An omitted `cached_input` or `cache_write` is billed at the level's `input`. An omitted `cache_write_1h` is billed at the level's `cache_write`, after that fallback. Prices never fall back across levels.
  3. `raw = (input·p_input + cached_input·p_cached_input + cache_write·p_cache_write + cache_write_1h·p_cache_write_1h + output·p_output) / 1,000,000`
- `per_request`: `raw = units · price`, where `units` defaults to 1.
- `custom`: not computable.

**Step 2: time multiplier.** Convert `created_at` to `time_pricing.timezone`. If `weekdays_only` is true and the local day is Saturday or Sunday, the multiplier is 1. Otherwise, if the local time of day falls in `[start, end)` of a window, the multiplier is that window's `multiplier`. If no window contains it, the multiplier is 1. Windows MUST NOT overlap, and a window MUST have `start < end` (split windows that cross midnight).

**Step 3: round once.** `cost = round_half_up(raw × multiplier, cost_precision)`. No intermediate value is rounded.

**Validity.** Within `tiers`, `above_tokens` MUST be unique and strictly increasing. Time windows MUST satisfy the rules in step 2. Consumers SHOULD reject a catalog entry that breaks these rules instead of guessing.

**Matching.** The `cost` in a usage record is authoritative, because it is what the publisher deducted. A consumer's recomputed cost matches if it differs by at most the larger of one unit in the last place of `cost_precision` and `cost_quantum`. A larger difference is a reconciliation error to report, not something to correct silently.

## 7. Account

1. `balance` is the amount the calling key can still spend before the publisher refuses it, taking every applicable limit into account: the key's own limit, the owning account's balance, and any other cap. When several apply, it is the smallest.
2. `unlimited` is true only when no finite cap applies to the key. A key with no limit of its own that draws on a finite account balance is **not** unlimited.
3. `rate_limits` lists limits that can refuse a request even when balance remains. `remaining`, when present, is the headroom at the time of the response.

## 8. Usage

1. `GET /uplink/v0/usage` returns settled charges of the calling key with `since <= created_at < until`. Both parameters are required, and `until − since` MUST NOT exceed 31 days (`400 range_too_large`).
2. Records are ordered by `created_at`, then `request_id`, ascending. `cursor` is opaque, and paging with it MUST neither skip nor repeat records.
3. `request_id` MUST equal the request identifier the publisher returned to the caller in a response header on the model call, so the consumer can join its own logs to the publisher's. The discovery document SHOULD name that header in `request_id_header`. For New API it is `X-Oneapi-Request-Id`.
4. Records are immutable once published. A record whose `created_at` is older than `usage_lag_seconds` MUST already be present. Consumers SHOULD reconcile only ranges older than that.
5. Records with `cost` equal to zero MAY be omitted.
6. Records MUST NOT contain prompts, completions, client IPs or upstream identifiers.
7. `GET /uplink/v0/usage/summary` returns per-day, per-model totals for the same range rules. When `usage.records` is also offered, each bucket MUST equal the sum of the corresponding records.

## 9. Status

`GET /uplink/v0/status` is self-reported by the publisher over the last `window_seconds`. It covers only the traffic served the same way as the calling key's requests (for example, the same group or channel set), and MUST NOT let one key infer another group's traffic volume. Consumers SHOULD show it next to their own measurements and SHOULD NOT treat it as proof of service quality.

## 10. Errors

1. Errors use the body in [`schemas/error.json`](schemas/error.json) and these statuses: `invalid_request` and `range_too_large` 400, `invalid_api_key` 401, `permission_denied` 403, `not_found` 404, `rate_limited` 429, `internal` 500, `unavailable` 503.
2. `429` MUST include `Retry-After`. Publishers SHOULD allow at least 60 requests per minute per key.
3. **Redaction.** Error messages MUST NOT contain group, channel, account or upstream names, upstream hosts, or internal identifiers. A publisher that declares `errors.redacted` makes the same promise for errors on model calls.

## 11. Caching

Responses from `account`, `usage` and `usage/summary` MUST carry `Cache-Control: private, no-store`. `catalog` and `status` MUST carry an `ETag` and SHOULD be cheap to serve under `If-None-Match`.

## 12. Extensions and versioning

1. Objects that allow it accept extra members whose names start with `x-`. Consumers MUST ignore extension members they do not understand.
2. v0 is a draft and may change incompatibly. From v1 on, a major version only adds optional members, and removals or meaning changes require a new major version under a new path (`/uplink/v2/...`).

## 13. Security considerations

- A publisher usually runs beside a platform that stores payment secrets, password hashes and every user's keys. It SHOULD read only the columns it needs, through database views or a scoped API. See the adapter docs.
- Publishers MUST NOT log API keys in full.
- The catalog reveals a key's prices to whoever holds the key. That is the purpose. It MUST NOT reveal anything about other keys.
