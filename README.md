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

It works with existing gateways (New API, sub2api) without modifying them: a small **publisher** runs beside the upstream gateway, and a **sync** tool beside the downstream one.

> **Status: v0 draft.** The specification is drafted. The New API publisher (`uplink export` + `uplink publish`) and the conformance check (`uplink check`) pass an end-to-end test against New API v1.0.0-rc.40; see [docs/adapters/newapi.md](docs/adapters/newapi.md). The downstream sync tool is not written yet.

## 中文简介

uplink 是 API 服务商（上游）和分销商（下游）之间的开放对接规范。下游用自己本来就有的 API 密钥，就能自动拿到可用模型、这把密钥的实际价格、余额、逐条扣费记录和健康状态，从而自动同步价格、逐条对账。不需要改动 New API 或 sub2api 本体。

## Repository layout

```
spec/            the specification — start with spec/README.md
  openapi.yaml   HTTP surface
  schemas/       JSON Schema 2020-12 for every response
  examples/      one valid example per schema
  vectors/       cost test vectors every implementation must reproduce
cmd/uplink       uplink export | publish | check
internal/        protocol (wire types, cost formula), export, exportdb, publish, check
adapters/newapi  New API: database reader, /api/pricing translation, billing-expression subset parser
e2e/newapi       end-to-end test against a real New API binary
deploy/          example configs
scripts/         spec checks
docs/            design rationale, adapter guides
```

## Checks

```sh
go test ./...                    # Go reference cost formula against spec/vectors
python3 scripts/validate_spec.py # examples vs schemas, plus an independent Python cost formula
npx @redocly/cli lint spec/openapi.yaml
make e2e NEWAPI=/path/to/new-api   # end-to-end against a New API binary
```

## License

Apache-2.0. See [LICENSE](LICENSE).
