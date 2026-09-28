# uplink sync (for resellers on New API)

`uplink sync` runs beside a reseller's New API. For every model the reseller buys from an uplink upstream, it:

1. reads the upstream's catalog with the key the upstream issued,
2. writes the price into the reseller's New API as a billing expression (upstream price × markup),
3. matches every request the reseller billed to the upstream's charge for it, and reports what each one actually earned.

Tested by `e2e/newapi/sync_run.py` with two New API **v1.0.0-rc.40** instances: one upstream, one reseller.

## What it needs

- A **root access token** for the reseller's New API, generated under Settings → Access token. Model pricing is root-only there. The token returned by logging in is a session token that expires, so it will not do. Channel keys are never read; New API leaves them out of channel lists.
- The **API key** each upstream issued, the same key that sits in the reseller's channels.
- The **channel ids** that use that key. A downstream model name is priced from the upstream models those channels route it to, following each channel's `model_mapping`.

It has no listener and only makes outbound calls: to the upstream publisher and to the reseller's own New API.

## How a price is computed

For each downstream model name served by the configured channels:

```
sell (quota USD per 1M tokens) = upstream price × fx × markup ÷ price_per_quota_usd, rounded up to round_to
```

This is done for each token bucket (input, cached input, cache write, 1-hour cache write, output) and each whole-request tier. It is written as a New API expression such as:

```
len <= 200000 ? tier("base", p * 0.267124 + c * 1.335617 + cr * 0.026713 + cc * 0.333905 + cc1h * 0.534247)
              : tier("above_200000", p * 0.534247 + ...)
```

All five variables are always written. This prevents New API from falling back to defaults: for example, Claude-named models get free cache writes when the expression leaves them out. New API group ratios still apply on top. `min_group_ratio` is the lowest one you sell at, and a price is refused if it would not keep `min_margin_pct` there.

A name is **not priced** (and a line says why) when:

- another enabled channel that is not an uplink upstream also serves it. A New API model name has one price for the whole station, so sync cannot price it for only one route.
- two upstreams price it in different shapes. When the shapes match, the higher price per bucket is used.
- the upstream uses time-of-day pricing or a `custom` unit. sync v0 cannot express these.
- the margin guard fails.

## Who owns a price

sync never overwrites a price the operator set unless told to:

| Situation | What sync does |
|---|---|
| Model not managed yet | Prints `TODO`. `-adopt name` (or `all`) saves the current pricing, then writes the expression. |
| Managed, upstream price unchanged | Nothing. |
| Managed, change ≤ `max_auto_change_pct`, `apply: auto`, no hold | Writes it. |
| Any other change | Prints `HELD` with the new expression. `-approve name` writes exactly that expression. If the upstream moved again in the meantime, the change is held again with the new price, so nothing is written that the operator has not seen. |
| Someone edited the price in New API | Stops managing it (`ALERT` once, `WARN` each cycle after). |
| `-release name` | Restores the saved pricing if the price is still sync's own; otherwise leaves it as is. Either way the model stops being managed. |

Every write carries the version New API returned when the price was read. New API rejects the write if the price changed in between. After writing, sync reads the price back and checks that the stored expression is the one it wrote. New API versions are a hash of the stored pricing, so "edited by someone else" is detected exactly.

## Reconciliation

Each cycle compares one window, from `lookback` ago up to the upstream's `usage_lag_seconds` ago (at least one minute ago):

- the reseller's consume logs on the configured channels, joined by `upstream_request_id`. New API stores the upstream's `X-Oneapi-Request-Id` there, which is the `request_id` uplink publishes.
- the upstream's usage records for the key.

The `RECON` line reports:

| Field | Meaning |
|---|---|
| matched | requests found on both sides; charged and cost are totals in the reseller's currency |
| lowest | the lowest margin of any single request |
| token mismatch | token counts differ between the two sides |
| low margin | the shortfall is larger than rounding explains (one quota unit downstream plus one cost step upstream) |
| upstream-only | the upstream charged for it but the reseller has no log, e.g. the key was used somewhere else |
| downstream-only | billed by the reseller but not yet or never charged upstream |

Low margin and upstream-only requests raise an `ALERT` once per request. A low-margin alert also sets a **hold**: no automatic price change until the operator checks and runs `-ack`. A typical cause is an upstream price increase still waiting for `-approve`.

## Running

```sh
uplink sync -config sync.yaml -once                 # one cycle, prints the plan; writes nothing new in preview mode
uplink sync -config sync.yaml -adopt all            # take over every priceable model
uplink sync -config sync.yaml -approve m1,m2        # apply held changes
uplink sync -config sync.yaml -release m1           # hand a model back
uplink sync -config sync.yaml -ack                  # clear holds
uplink sync -config sync.yaml                       # loop every `interval`
```

Output lines start with `DONE`, `TODO`, `HELD`, `HOLD`, `RECON`, `INFO`, `WARN` or `ALERT`.

## Known limitations (v0)

- Alerts go to standard output only; there is no webhook yet.
- The downstream must be New API. The upstream can be any uplink publisher.
- Channel model lists are not changed. Adding a new upstream model to a channel is still manual.
- Each `upstreams` entry holds one key. Buying from the same upstream with several keys means one entry per key.
- Time-of-day pricing, `custom` units, and tool, image or audio surcharges are not written. Records carrying `x-unmodeled` are counted as `unmodeled` and left out of the margin figures.
- The reseller's recharge discounts (TopupGroupRatio) and subscription plans are not taken into account by the margin guard.
