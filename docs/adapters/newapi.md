# New API adapter

Tested against New API **v1.0.0-rc.40** (commit `0aec08f`) on SQLite, by `e2e/newapi/run.py`.

## How it runs

Two processes, so that the internet-facing one cannot read New API's secrets:

| Process | Network | Reads | Writes |
|---|---|---|---|
| `uplink export` | none (outbound to New API's own HTTP port only) | New API database, read-only; `/api/pricing` as probe users | the export file |
| `uplink publish` | serves `/.well-known/uplink.json` and `/uplink/v0/*` | the export file only | nothing |

New API's SQLite file stores every API key in plaintext and payment secrets in `options`. SQLite has no per-user permissions, so column-level restriction is impossible there. The exporter therefore copies only what the publisher needs:

- key **SHA-256 hashes** with each key's status, quota, expiry, model limits and group
- resolved catalogs per group
- usage rows with token counts, quota, model, request id and first-token latency

It never copies prompts, completions, IPs, channel data, or the admin-only parts of `logs.other`.

Mounts:

- **Exporter**: New API's data directory, **read-write**. New API runs SQLite in WAL mode, and readers need to open its shared-memory file. The exporter's connection is still read-only (`mode=ro`, `query_only`).
- **Exporter**: the export directory, read-write.
- **Publisher**: the export **directory** (not the single file), **read-only**. The export file uses a rollback journal, not WAL, so a reader needs no write access. A single-file bind mount would keep pointing at a stale inode.

Start the exporter first. The publisher refuses to start until the export file exists, and it answers `503` once the export is older than `stale_after`.

The e2e test covers all of this. The exporter runs as a live loop while traffic flows, and the publisher is then run against a `chmod 555` directory. The test also checks that every published `request_id` equals the `X-Oneapi-Request-Id` the caller received.

## Prices come from New API itself

uplink does not re-implement New API's billing rules. For every group whose members are resellers, the operator creates a **probe user**:

1. Create a user, put it in the reseller group, and leave its quota at 0.
2. Log in as that user and generate an access token (Settings → Access token).
3. Put the token in the exporter's environment and list it under `newapi.probes`.

The exporter calls `GET /api/pricing` with that token. New API then returns exactly what that group sees, with defaults, group-to-group ratios and billing expressions already resolved. The pricing page module must stay enabled.

Translation:

| New API pricing | uplink price |
|---|---|
| ratio mode | input = `model_ratio × group_ratio × 1e6 / QuotaPerUnit` USD per 1M tokens<br>output = input × `completion_ratio`<br>cached_input = input × `cache_ratio` (default 1)<br>cache_write = input × `create_cache_ratio` (default 1.25)<br>cache_write_1h = cache_write × 6/3.75 |
| per-request `model_price` | `per_request`, `model_price × group_ratio` |
| `tiered_expr`: tiers on `len` over `p, c, cr, cc, cc1h` | `per_1m_tokens` with `tiers` (`len <= N` → `above_tokens: N`) |
| `tiered_expr`: a single `tier(..., fixed(x))` | `per_request` |
| any other expression: request rules (`\|\|\|`), `param`/`header`/time conditions, image/audio variables | `custom` |
| `model_group_ratio` (present in some forks) | replaces the group ratio for that model |

All USD amounts are multiplied by `price_per_usd`, which is what the operator charges for one USD of quota. `cost_quantum` = `price_per_usd / QuotaPerUnit`, and `cost_precision` is the number of decimal places that value needs.

## Known limitations (v0)

- **Keys on the `auto` group** get every price as `custom`, because the group is chosen per request.
- **Anthropic-format requests on an expression that does not price `cc`**: New API does not bill their cache writes, but the catalog prices them at the input price. The consumer's recomputed cost is then higher than the actual charge.
- **Tool-call surcharges, image-token and audio-token pricing, and request-rule multipliers** are not expressible in v0. Records that include them carry `x-unmodeled` with the reason, and `uplink check` skips them.
- **Subscription plans** that pay for requests are not reflected in `balance`.
- **Per-group top-up ratios** (`TopupGroupRatio`) are not supported. `price_per_usd` is one value for the whole station, so a station that charges groups differently per USD of quota will publish wrong prices for some of them.
- **Status** covers only requests in the calling key's group, so resellers cannot see other groups' traffic.
- **Probe failures** are logged and that group keeps its last catalog. Keys and usage still export.
- **Errors on model calls are not redacted.** New API's "no available channel" errors name the group, so the publisher does not claim `errors.redacted`.
- **Consume logging must be on.** Without it there are no usage records, and the exporter logs a warning.
- **Minimum charge and rounding.** New API rounds each charge to whole quota units and charges at least one. Both stay within `cost_quantum`, which widens the matching tolerance.
- **MySQL and PostgreSQL** are not implemented yet. That path must also re-read a short `created_at` look-back on each cycle, because reading by `id > last` can skip rows that commit out of order. SQLite commits serially and does not have this problem.
