# uplink

An open, read-only interface between an LLM API provider and the resellers that buy from it.

A reseller that connects to an upstream today pastes an API key into its own gateway and then guesses: prices change without notice, cache discounts are not carried over, balances are checked by hand, and reconciling usage means matching timestamps and token counts. uplink gives the reseller, with the same API key, five things:

| Endpoint | Answers |
|---|---|
| `/.well-known/uplink.json` | Does this host speak uplink, and what does it support? |
| `/uplink/v0/catalog` | Which models can this key call, at what exact price? |
| `/uplink/v0/account` | How much balance and headroom does this key have? |
| `/uplink/v0/usage` | Every settled charge, with the request id the upstream returned |
| `/uplink/v0/status` | Is each model healthy right now (self-reported)? |

It runs beside existing gateways without modifying them: a small **publisher** beside the upstream gateway, and a **sync** tool beside the downstream one. The first adapter is for [New API](https://github.com/QuantumNous/new-api) on SQLite; sub2api, and New API on MySQL or PostgreSQL, are planned.

> **Status: v0 draft.** The specification is drafted. The New API publisher (`uplink export` + `uplink publish`) and the conformance check (`uplink check`) pass end-to-end tests against New API v1.0.0-rc.40 and a v1.0.0-rc.34-based build; see [docs/adapters/newapi.md](docs/adapters/newapi.md). The downstream sync tool (`uplink sync`) passes an end-to-end test with two New API instances, an upstream and a reseller; see [docs/sync.md](docs/sync.md). None of it has run against production traffic yet.

## 中文简介

uplink 是 API 服务商（上游）和分销商（下游）之间的开放对接规范。下游用自己本来就有的 API 密钥，就能自动拿到可用模型、这把密钥的实际价格、余额、逐条扣费记录和健康状态，从而自动同步价格、逐条对账。不需要改动网关本体。目前支持 New API（SQLite 数据库）；sub2api 以及使用 MySQL/PostgreSQL 的 New API 在计划中。

## Repository layout

```
spec/            the specification — start with spec/README.md
  openapi.yaml   HTTP surface
  schemas/       JSON Schema 2020-12 for every response
  examples/      one valid example per schema
  vectors/       cost test vectors every implementation must reproduce
cmd/uplink       uplink export | publish | check | sync
internal/        protocol (wire types, cost formula), export, exportdb, publish, check,
                 client (uplink consumer), downstream (sync: pricing plan, ownership state, reconciliation)
adapters/newapi  New API: database reader, /api/pricing translation, billing-expression parser and writer, admin client
e2e/newapi       end-to-end tests against real New API binaries (publisher; two-station sync)
deploy/          example configs
scripts/         spec checks
docs/            design rationale, adapter guides
```

## Checks

```sh
go test ./...                    # Go reference cost formula against spec/vectors
python3 scripts/validate_spec.py # examples vs schemas, plus an independent Python cost formula
npx @redocly/cli lint spec/openapi.yaml
make e2e NEWAPI=/path/to/new-api   # both end-to-end tests against a New API binary
```

## License

Apache-2.0. See [LICENSE](LICENSE).
