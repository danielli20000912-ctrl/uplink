#!/usr/bin/env python3
"""End-to-end test of `uplink sync`: two real New API instances.

Usage:
  sync_run.py --newapi /path/to/new-api-binary --uplink /path/to/uplink --work /tmp/dir

  U (upstream station): New API + mock model upstream, published by uplink
    export/publish exactly as in run.py.
  D (downstream reseller): a second New API whose channels point at U with a
    key U issued to the reseller. One channel renames a model, one speaks the
    Anthropic format, and one direct channel shares a model name with U.

What it checks:
  1. sync writes nothing until models are adopted, and never prices a name
     that a non-uplink channel also serves
  2. after adoption, every request D bills equals U's cost x markup, request
     by request, within one quota unit (computed here, independently of sync)
  3. a small upstream price change is applied automatically, a large one
     waits for -approve
  4. reconciliation matches D's logs to U's records by request id, reports a
     request billed at a loss while a change was held, and a request made on
     U with the reseller's key that D never billed
  5. an operator's manual edit stops sync managing that model; release
     restores the pricing D had before adoption, including "no pricing at all"

Everything is local; nothing leaves 127.0.0.1.
"""
import argparse
import json
import os
import secrets
import signal
import subprocess
import sys
import threading
import time
from decimal import Decimal
from http.server import ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import run as u  # noqa: E402  (helpers and the mock model upstream)

D_PORT = 13001
D = f"http://127.0.0.1:{D_PORT}"
D_ROOT_PASS = "d-root-pass-" + secrets.token_hex(4)
MARKUP = Decimal("1.3")
D_PRICE = Decimal("7.3")     # what D's customers pay for one USD of quota, CNY
QUOTA_PER_UNIT = 500000


def dapi(method, path, body=None, token=None):
    return u.api(method, path, body, token, base=D)


def d_chat(key, model, prompt, cached, completion):
    u.call("POST", D + "/v1/chat/completions",
           {"model": model, "messages": [{"role": "user", "content": "hi"}], "user": f"mock:{prompt}:{cached}:{completion}"}, key)


def d_messages(key, model, inp, cr, c5, c1, out):
    u.call("POST", D + "/v1/messages",
           {"model": model, "max_tokens": 16, "messages": [{"role": "user", "content": "hi"}],
            "metadata": {"user_id": f"mock:{inp}:{cr}:{c5}:{c1}:{out}"}},
           key, headers={"anthropic-version": "2023-06-01"})


def d_pricing(root, models):
    q = "&".join(f"model={m}" for m in models)
    snap = dapi("GET", "/api/option/model_pricing?" + q, token=root)
    got = {e["model_name"]: e for e in snap["entries"]}
    return {m: got.get(m, {"version": snap["empty_version"], "configured": {}}) for m in models}


def d_logs(root):
    page = dapi("GET", "/api/log/?type=2&p=1&page_size=100", token=root)
    return page["items"]


def u_records(key):
    since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 3600))
    until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 60))
    _, page = u.call("GET", u.PUBLISH + f"/uplink/v0/usage?since={since}&until={until}&limit=1000", token=key)
    return page["records"]


def catalog_price(key, model):
    _, cat = u.call("GET", u.PUBLISH + "/uplink/v0/catalog", token=key)
    return next(m for m in cat["models"] if m["id"] == model)["price"]["input"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--newapi", required=True)
    ap.add_argument("--uplink", required=True)
    ap.add_argument("--work", required=True)
    a = ap.parse_args()
    work = Path(a.work)
    (work / "u").mkdir(parents=True, exist_ok=True)
    (work / "d").mkdir(parents=True, exist_ok=True)
    for f in list(work.glob("**/*.db*")):
        f.unlink()
    procs = []
    failures = []

    def expect(cond, what):
        if not cond:
            failures.append(what)
            print("  FAIL:", what)

    mock = ThreadingHTTPServer(("127.0.0.1", u.MOCK_PORT), u.Mock)
    threading.Thread(target=mock.serve_forever, daemon=True).start()

    def start_newapi(port, d):
        env = dict(os.environ, PORT=str(port), SQLITE_PATH=str(d / "newapi.db"),
                   SESSION_SECRET=secrets.token_hex(16), TZ="UTC")
        p = subprocess.Popen([a.newapi], cwd=d, env=env, stdout=open(d / "newapi.log", "w"), stderr=subprocess.STDOUT)
        procs.append(p)

    try:
        # ---------------------------------------------------------- station U
        start_newapi(u.NEWAPI_PORT, work / "u")
        u.wait_http(u.NEWAPI + "/api/status")
        u.api("POST", "/api/setup", {"username": "root", "password": u.ROOT_PASS, "confirmPassword": u.ROOT_PASS})
        uroot = u.login("root", u.ROOT_PASS)
        u.configure(uroot)
        u.make_user(uroot, "probe_partner", "partner", 0)
        u.make_user(uroot, "reseller", "partner", QUOTA_PER_UNIT * 100)
        pat = u.make_pat("probe_partner")
        _, key_a = u.make_key(u.login("reseller", u.USER_PASS), "for-station-d")

        exp_dir = work / "u" / "export"
        exp_dir.mkdir(exist_ok=True)
        (work / "export.yaml").write_text(f"""
newapi:
  base_url: {u.NEWAPI}
  db_path: {work / 'u' / 'newapi.db'}
  probes:
    - group: partner
      access_token_env: UPLINK_PROBE_PARTNER
currency: CNY
price_per_usd: "1"
export_db: {exp_dir / 'export.db'}
interval: 2s
""")
        procs.append(subprocess.Popen([a.uplink, "export", "-config", str(work / "export.yaml")],
                                      env=dict(os.environ, UPLINK_PROBE_PARTNER=pat),
                                      stdout=open(work / "export.log", "w"), stderr=subprocess.STDOUT))
        u.wait_for(lambda: (exp_dir / "export.db").exists(), "first export")
        time.sleep(3)
        (work / "publish.yaml").write_text(f"""
listen: 127.0.0.1:{u.PUBLISH_PORT}
export_db: {exp_dir / 'export.db'}
api_base: {u.NEWAPI}/v1
usage_lag_seconds: 5
stale_after: 30s
""")
        procs.append(subprocess.Popen([a.uplink, "publish", "-config", str(work / "publish.yaml")],
                                      stdout=open(work / "publish.log", "w"), stderr=subprocess.STDOUT))
        u.wait_http(u.PUBLISH + "/.well-known/uplink.json")
        # U runs New API with error logging off (its default): failures leave
        # no trace, so the status endpoint must not be offered.
        _, disc = u.call("GET", u.PUBLISH + "/.well-known/uplink.json")
        code, _ = u.call("GET", u.PUBLISH + "/uplink/v0/status", token=key_a, expect=None)
        expect("status" not in disc["capabilities"] and "status" not in disc["endpoints"] and code == 404,
               f"status offered without error logs: caps={disc['capabilities']} code={code}")

        # ---------------------------------------------------------- station D
        start_newapi(D_PORT, work / "d")
        u.wait_http(D + "/api/status")
        dapi("POST", "/api/setup", {"username": "root", "password": D_ROOT_PASS, "confirmPassword": D_ROOT_PASS})
        droot = u.login("root", D_ROOT_PASS, base=D)
        for k, v in {
            "Price": str(D_PRICE),
            "GroupRatio": json.dumps({"default": 1}),
            "ModelRatio": json.dumps({"m-ratio": 2, "resold-expr": 2, "claude-ratio": 2}),
            "CompletionRatio": json.dumps({"m-ratio": 4, "resold-expr": 4, "claude-ratio": 4}),
            "ModelPrice": json.dumps({"m-fixed": 0.05}),
        }.items():
            dapi("PUT", "/api/option/", {"key": k, "value": v}, droot)
        for ch in [
            {"type": 1, "name": "U-openai", "key": key_a, "base_url": u.NEWAPI, "models": "m-ratio,resold-expr,m-fixed,fresh-name",
             "model_mapping": json.dumps({"resold-expr": "m-expr", "fresh-name": "m-ratio"})},   # fresh-name: no price at D yet
            {"type": 14, "name": "U-anthropic", "key": key_a, "base_url": u.NEWAPI, "models": "claude-ratio"},
            {"type": 1, "name": "direct", "key": "sk-mock", "base_url": f"http://127.0.0.1:{u.MOCK_PORT}", "models": "m-fixed"},
        ]:
            dapi("POST", "/api/channel/", {"mode": "single", "channel": dict(ch, group="default", status=1)}, droot)
        chans = {c["name"]: c["id"] for c in dapi("GET", "/api/channel/?p=1&page_size=100", token=droot)["items"]}
        u.make_user(droot, "cust", "default", QUOTA_PER_UNIT * 100, base=D)
        _, cust = u.make_key(u.login("cust", u.USER_PASS, base=D), "cust-key", base=D)
        models = ["m-ratio", "resold-expr", "claude-ratio", "m-fixed", "fresh-name"]
        before = d_pricing(droot, models)
        # sync authenticates the way an operator would: a long-lived access
        # token from Settings, not a login session (those expire).
        dpat = u.make_pat("root", D_ROOT_PASS, base=D)
        print("setup done; D channels", chans)

        (work / "sync.yaml").write_text(f"""
downstream:
  base_url: {D}
  admin_token_env: UPLINK_D_ADMIN
  currency: CNY
  price_per_quota_usd: "{D_PRICE}"
upstreams:
  - name: our-pool
    base_url: {u.PUBLISH}
    key_env: UPLINK_UP_KEY
    channel_ids: [{chans['U-openai']}, {chans['U-anthropic']}]
    markup: "{MARKUP}"
pricing:
  apply: auto
  max_auto_change_pct: "20"
  min_group_ratio: "1"
  min_margin_pct: "5"
  round_to: "0.000001"
reconcile:
  lookback: 1h
  margin_alert_pct: "10"
state_db: {work / 'sync-state.db'}
""")
        sync_log = open(work / "sync.log", "w")

        def sync(*args):
            r = subprocess.run([a.uplink, "sync", "-config", str(work / "sync.yaml"), "-once", *args],
                               env=dict(os.environ, UPLINK_D_ADMIN=dpat, UPLINK_UP_KEY=key_a), capture_output=True, text=True)
            sync_log.write(f"$ uplink sync -once {' '.join(args)}\n{r.stdout}{r.stderr}\n")
            sync_log.flush()
            print(f"---- sync {' '.join(args)}\n{r.stdout}{r.stderr}".rstrip())
            if r.returncode != 0:
                raise RuntimeError(f"sync exited {r.returncode}")
            return r.stdout

        # 1. Nothing is written before adoption; shared names are refused.
        out = sync()
        for m in ("m-ratio", "resold-expr", "claude-ratio", "fresh-name"):
            expect(f"TODO  {m} " in out, f"no adoption prompt for {m}")
        expect(before["fresh-name"]["configured"] == {}, f"fresh-name already priced at D: {before['fresh-name']}")
        expect("m-fixed also served by non-uplink channel" in out, "shared name m-fixed not refused")
        expect(d_pricing(droot, models) == before, "pricing changed before adoption")

        out = sync("-adopt", "all")
        now = d_pricing(droot, models)
        for m in ("m-ratio", "resold-expr", "claude-ratio", "fresh-name"):
            expect(f"DONE  {m} adopted" in out, f"{m} not adopted")
            expect(now[m]["configured"].get("billing_setting.billing_mode") == "tiered_expr", f"{m} not on an expression")
        expect(now["m-fixed"] == before["m-fixed"], "m-fixed was touched")

        # 2. D bills its customer; compare every request with U's cost.
        d_chat(cust, "m-ratio", 10432, 9984, 812)
        d_chat(cust, "m-ratio", 7, 0, 3)
        d_chat(cust, "resold-expr", 50000, 20000, 1000)
        d_chat(cust, "resold-expr", 200001, 100000, 100)
        d_messages(cust, "claude-ratio", 1200, 30000, 4000, 2000, 500)
        u.chat(key_a, "m-ratio", 100, 0, 10)          # reseller key used on U directly: D never bills it
        u.wait_for(lambda: len(u_records(key_a)) == 6, "6 records published by U", timeout=30)

        recs = {r["request_id"]: r for r in u_records(key_a)}
        logs = [l for l in d_logs(droot) if l.get("channel") in (chans["U-openai"], chans["U-anthropic"])]
        expect(len(logs) == 5, f"D logged {len(logs)} requests, want 5")
        print(f"{'D model':<14}{'U model':<14}{'U cost CNY':>12}{'x1.3 in D quota':>17}{'D quota':>9}")
        for l in sorted(logs, key=lambda x: x["id"]):
            r = recs.get(l.get("upstream_request_id", ""))
            if r is None:
                expect(False, f"D log {l['id']} has no U record (upstream id {l.get('upstream_request_id')!r})")
                continue
            want = Decimal(r["cost"]) * MARKUP / D_PRICE * QUOTA_PER_UNIT
            print(f"{l['model_name']:<14}{r['model']:<14}{r['cost']:>12}{want:>17.2f}{l['quota']:>9}")
            expect(abs(Decimal(l["quota"]) - want) <= 1 + want * Decimal("1e-5"),
                   f"{l['model_name']}: D charged {l['quota']} quota, U cost x markup = {want:.2f}")
        expect(len(set(recs) - {l.get("upstream_request_id") for l in logs}) == 1, "exactly one U record without a D log")

        # 3. Small upstream change applies itself; a large one waits.
        def set_partner_ratio(r):
            old = catalog_price(key_a, "m-ratio")
            u.api("PUT", "/api/option/", {"key": "GroupRatio", "value": json.dumps({"default": 1, "partner": r})}, uroot)
            u.wait_for(lambda: catalog_price(key_a, "m-ratio") != old, "catalog to reflect the new price", timeout=30)

        set_partner_ratio(0.52)                          # +4%
        out = sync()
        for m in ("m-ratio", "resold-expr", "claude-ratio"):
            expect(f"DONE  {m} updated" in out, f"small change not applied to {m}")
        after_small = d_pricing(droot, models)

        set_partner_ratio(0.9)                           # +73%
        out = sync()
        for m in ("m-ratio", "resold-expr", "claude-ratio"):
            expect(f"HELD  {m} " in out, f"large change not held for {m}")
        expect(d_pricing(droot, models) == after_small, "large change written without approval")
        out = sync("-approve", "resold-expr")
        expect("DONE  resold-expr updated" in out and "HELD  m-ratio" in out, "approve did not apply exactly resold-expr")

        # 4. m-ratio is still at the old price: this request loses money.
        d_chat(cust, "m-ratio", 3000, 1000, 200)
        last = time.time()
        u.wait_for(lambda: len(u_records(key_a)) == 7, "7th record", timeout=30)
        wait = 62 - (time.time() - last)
        print(f"waiting {wait:.0f}s for the reconciliation window (records older than 60s)")
        time.sleep(max(0, wait))
        out = sync()
        recon = next((line for line in out.splitlines() if line.startswith("RECON")), "")
        for want in ("matched 6", "token mismatch 0", "low margin 1", "upstream-only 1", "downstream-only 0"):
            expect(want in recon, f"reconciliation: {want!r} not in {recon!r}")
        expect("ALERT m-ratio request" in out and "earned -2" in out, "loss-making request not alerted")
        expect("ALERT m-ratio upstream our-pool charged request" in out, "U-only request not alerted")
        expect("HOLD  low_margin" in out, "no hold after low margin")
        out = sync()
        expect("earned" not in out, "alert repeated on the next cycle")

        # 5. Manual edit, release, ack.
        cur = d_pricing(droot, ["m-ratio"])["m-ratio"]
        dapi("PATCH", "/api/option/model_pricing", {"changes": [
            {"model_name": "m-ratio", "expected_version": cur["version"], "pricing": {"ModelRatio": 3}}]}, droot)
        out = sync()
        expect("ALERT m-ratio pricing was edited outside uplink" in out, "manual edit not detected")
        out = sync()
        expect("WARN  m-ratio not managed since its pricing was edited outside uplink" in out, "no reminder about the edited model")
        expect("HELD  m-ratio" not in out, "edited model still has a held change")
        out = sync("-release", "claude-ratio,m-ratio,fresh-name")
        after = d_pricing(droot, models)
        expect("DONE  claude-ratio released; previous pricing restored" in out, "claude-ratio release message")
        expect(after["claude-ratio"]["version"] == before["claude-ratio"]["version"], "claude-ratio pricing not restored")
        expect("DONE  m-ratio released; pricing was edited outside uplink" in out, "m-ratio release message")
        expect(after["m-ratio"]["configured"] == {"ModelRatio": 3}, "operator's m-ratio pricing was changed")
        expect("DONE  fresh-name released; previous pricing restored" in out, "fresh-name release message")
        expect(after["fresh-name"]["configured"] == {} and after["fresh-name"]["version"] == before["fresh-name"]["version"],
               f"fresh-name not back to no pricing: {after['fresh-name']}")
        out = sync("-ack")
        expect("holds cleared" in out and "HOLD " not in out, "ack did not clear holds")
        expect("TODO  claude-ratio " in out, "released model was taken over again")

        if failures:
            print("SYNC E2E FAIL")
            for f in failures:
                print("  -", f)
            return 1
        print("SYNC E2E OK")
        return 0
    finally:
        for p in procs:
            if p.poll() is None:
                p.send_signal(signal.SIGTERM)
                try:
                    p.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    p.kill()
        mock.shutdown()


if __name__ == "__main__":
    sys.exit(main())
