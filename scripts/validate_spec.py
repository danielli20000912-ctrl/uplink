#!/usr/bin/env python3
"""Validate spec/examples against spec/schemas and check cost consistency.

Needs: pip install jsonschema (>= 4.18, for the referencing library).

Checks:
1. Every schema is a valid JSON Schema 2020-12 document.
2. Every spec/examples/<name>.json validates against spec/schemas/<name>.json.
3. Every price in spec/vectors/cost.json validates against the shared price schema.
4. An independent Python implementation of the cost formula reproduces every
   vector and every cost in examples/usage.json (priced by examples/catalog.json).
   The Go implementation in internal/protocol is checked by `go test`; two
   implementations agreeing is the evidence that the written rules are unambiguous.
"""
import datetime as dt
import json
import sys
from decimal import ROUND_HALF_UP, Decimal
from pathlib import Path
from zoneinfo import ZoneInfo

from jsonschema import Draft202012Validator, FormatChecker
from referencing import Registry, Resource

ROOT = Path(__file__).resolve().parent.parent
SCHEMAS = ROOT / "spec" / "schemas"
EXAMPLES = ROOT / "spec" / "examples"
VECTORS = ROOT / "spec" / "vectors"


def load(p: Path):
    return json.loads(p.read_text(encoding="utf-8"))


def retrieve(uri: str):
    path = Path(uri.removeprefix("file://"))
    return Resource.from_contents(load(path))


REGISTRY = Registry(retrieve=retrieve)


def validator_for(schema_path: Path, schema=None):
    """Build a validator; relative $refs resolve against schema_path's directory."""
    schema = dict(schema if schema is not None else load(schema_path))
    Draft202012Validator.check_schema(schema)
    schema.setdefault("$id", schema_path.as_uri())
    return Draft202012Validator(schema, registry=REGISTRY, format_checker=FormatChecker())


# ---- reference cost formula (mirrors spec/README.md section "Cost") ----

class NotComputable(Exception):
    pass


def level_prices(level):
    inp = Decimal(level["input"])
    cw = Decimal(level["cache_write"]) if "cache_write" in level else inp
    return {
        "input": inp,
        "cached_input": Decimal(level["cached_input"]) if "cached_input" in level else inp,
        "cache_write": cw,
        "cache_write_1h": Decimal(level["cache_write_1h"]) if "cache_write_1h" in level else cw,
        "output": Decimal(level["output"]),
    }


def time_multiplier(tp, created_at: str) -> Decimal:
    if not tp:
        return Decimal(1)
    ts = dt.datetime.fromisoformat(created_at.replace("Z", "+00:00"))
    local = ts.astimezone(ZoneInfo(tp["timezone"]))
    if tp.get("weekdays_only") and local.weekday() >= 5:
        return Decimal(1)
    hm = local.hour * 60 + local.minute
    for w in tp["windows"]:
        sh, sm = map(int, w["start"].split(":"))
        eh, em = map(int, w["end"].split(":"))
        # [start, end) at second resolution: compare seconds since midnight
        sec = hm * 60 + local.second + Decimal(local.microsecond) / 1_000_000
        if (sh * 60 + sm) * 60 <= sec < (eh * 60 + em) * 60:
            return Decimal(w["multiplier"])
    return Decimal(1)


def compute_cost(price, time_pricing, created_at, tokens, units, precision) -> str:
    unit = price["unit"]
    if unit == "custom":
        raise NotComputable
    if unit == "per_request":
        raw = Decimal(price["price"]) * (units or 1)
    else:
        tk = {k: (tokens or {}).get(k, 0) for k in ("input", "cached_input", "cache_write", "cache_write_1h", "output")}
        context = tk["input"] + tk["cached_input"] + tk["cache_write"] + tk["cache_write_1h"]
        level = price
        for tier in sorted(price.get("tiers", []), key=lambda t: t["above_tokens"]):
            if context > tier["above_tokens"]:
                level = tier
        lp = level_prices(level)
        raw = sum(Decimal(tk[k]) * lp[k] for k in tk) / Decimal(1_000_000)
    total = raw * time_multiplier(time_pricing, created_at)
    return str(total.quantize(Decimal(1).scaleb(-precision), rounding=ROUND_HALF_UP))


def validate_responses(directory: Path) -> int:
    """Validate saved live responses (from `uplink check -save`) against the schemas."""
    failures, n = [], 0
    for f in sorted(directory.glob("*.json")):
        name = f.stem.split("-")[0]
        schema = SCHEMAS / f"{name}.json"
        if not schema.exists():
            continue
        n += 1
        for err in validator_for(schema).iter_errors(load(f)):
            failures.append(f"{f.name}: {'/'.join(map(str, err.absolute_path))}: {err.message}")
    if failures:
        print("FAIL")
        for x in failures:
            print("  -", x)
        return 1
    print(f"OK: {n} live responses valid")
    return 0


def main() -> int:
    if len(sys.argv) == 3 and sys.argv[1] == "--responses":
        return validate_responses(Path(sys.argv[2]))
    failures = []

    for schema_path in sorted(SCHEMAS.glob("*.json")):
        v = validator_for(schema_path)
        example = EXAMPLES / schema_path.name
        if schema_path.name == "common.json":
            continue
        if not example.exists():
            failures.append(f"missing example for {schema_path.name}")
            continue
        for err in v.iter_errors(load(example)):
            failures.append(f"examples/{example.name}: {'/'.join(map(str, err.absolute_path))}: {err.message}")

    price_v = validator_for(SCHEMAS / "_price.json", {"$ref": "common.json#/$defs/price"})
    tp_v = validator_for(SCHEMAS / "_time_pricing.json", {"$ref": "common.json#/$defs/timePricing"})

    vectors = load(VECTORS / "cost.json")["cases"]
    for c in vectors:
        for err in price_v.iter_errors(c["price"]):
            failures.append(f"vector '{c['name']}': price: {err.message}")
        if "time_pricing" in c:
            for err in tp_v.iter_errors(c["time_pricing"]):
                failures.append(f"vector '{c['name']}': time_pricing: {err.message}")
        try:
            got = {"cost": compute_cost(c["price"], c.get("time_pricing"), c["created_at"],
                                        c.get("tokens"), c.get("units"), c["cost_precision"])}
        except NotComputable:
            got = {"error": "not_computable"}
        if got != c["expect"]:
            failures.append(f"vector '{c['name']}': expected {c['expect']}, python got {got}")

    catalog = {m["id"]: m for m in load(EXAMPLES / "catalog.json")["models"]}
    precision = load(EXAMPLES / "discovery.json")["cost_precision"]
    for r in load(EXAMPLES / "usage.json")["records"]:
        m = catalog[r["model"]]
        got = compute_cost(m["price"], m.get("time_pricing"), r["created_at"], r["tokens"], r.get("units"), precision)
        if got != r["cost"]:
            failures.append(f"examples/usage.json {r['request_id']}: cost {r['cost']} but catalog prices give {got}")

    if failures:
        print("FAIL")
        for f in failures:
            print("  -", f)
        return 1
    print(f"OK: {len(list(SCHEMAS.glob('*.json')))} schemas, {len(list(EXAMPLES.glob('*.json')))} examples, {len(vectors)} cost vectors")
    return 0


if __name__ == "__main__":
    sys.exit(main())
