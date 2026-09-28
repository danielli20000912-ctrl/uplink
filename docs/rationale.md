# Design rationale

Why uplink v0 looks the way it does. Facts about existing platforms were checked against source code on 2026-09-28/29; file paths are given so they can be re-checked when those projects change.

## Why a publisher on the upstream side is required

A reseller only holds an ordinary API key. On the two most common platforms that key cannot read everything a reseller needs:

| Need | New API | sub2api |
|---|---|---|
| Models the key can call | yes, `GET /v1/models` | yes, `GET /v1/models` |
| Balance | yes, `GET /api/usage/token/` (`router/api-router.go`, `controller/token.go` `GetTokenUsage`) — in internal quota units | yes, `GET /v1/usage` (`backend/internal/server/routes/gateway.go`, `handler/gateway_handler.go` `Usage`) — in USD |
| Prices after group discounts | **no** — `/api/pricing` resolves the user from a dashboard session or personal access token only (`middleware/header_nav.go` `HeaderNavModuleAuth` → `TryUserAuth`) | partly — `GET /v1/sub2api/billing` returns the key's effective multiplier (`handler/gateway_key_billing.go`); absolute prices are on `/v1/model-plaza`, which uses optional login, not the key |
| Per-request records | **no** — `/api/log/self` requires user auth | **no** — `/usage` list requires login |

So a small process run by the upstream operator has to answer these questions on the key's behalf. That is the publisher.

## Why absolute prices, not ratios

Each platform has its own ratio convention (New API: model ratio 1 = $2 per 1M input tokens, 500,000 quota = $1; sub2api: group and user multipliers over a catalog price). Resellers already mis-convert between them. Absolute prices in a declared currency leave one conversion, done once, by the consumer that knows its own pricing.

## Why disjoint token buckets

OpenAI-style usage counts cached tokens inside `prompt_tokens`, while Anthropic-style usage excludes cache reads and writes from `input_tokens`. Any single overlapping definition would be misread by half the implementations. Disjoint buckets make the cost formula a plain sum.

## Tiers: whole-request only, exclusive threshold

sub2api's context pricing supports only whole-request tiers; its marginal basis was removed (`backend/internal/service/billing_context_schedule.go`, comment on `ContextPricingBasis`). v0 keeps a `basis` field with the single value `whole_request` so another value can be added without breaking consumers.

Platforms disagree on whether the threshold itself belongs to the upper tier (sub2api has a `thresholdInclusive` flag). v0 fixes one convention — the tier applies when context tokens are strictly greater than `above_tokens` — and adapters for inclusive platforms publish `threshold − 1`. Each adapter must confirm which tokens its platform counts as context.

## Time-of-day pricing

sub2api multiplies the whole request cost by a period multiplier on `[start, end)` in a configured zone, optionally weekdays only (`TimePricingPeriod`, `TimePricingSchedule` in the same file). v0 adopts the same semantics.

## Request identifiers make per-record reconciliation possible

New API returns `X-Oneapi-Request-Id` on every response. When New API is itself the downstream, it stores the upstream's value in the dedicated, indexed `logs.upstream_request_id` column, next to its own `logs.request_id`, on both the consume and error paths (`common/constants.go` `UpstreamRequestIdKey`, `service/http.go` `ShouldCopyUpstreamHeader`, `model/log.go` `RecordConsumeLog` / `RecordErrorLog`; checked on QuantumNous/new-api main). Publishing `request_id` therefore lets a New API downstream join records one-to-one with no changes on its side. Whether sub2api stores an upstream request id has not been checked.

## Out of scope for v0

- In-platform referral commissions: sub2api and several New API forks already implement them.
- Cross-publisher commission settlement, white-label hosting, multi-level distribution, payments.
- Signed statements: a signature proves who issued a statement, not what model served the request. Deferred until v0 sees real use.
- Push notifications (webhooks): polling is sufficient at reseller scale.
