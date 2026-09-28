#!/usr/bin/env python3
"""End-to-end test: a real New API instance, a mock model upstream, uplink.

Usage:
  run.py --newapi /path/to/new-api-binary --uplink /path/to/uplink --work /tmp/dir

What it does:
  1. starts a mock upstream that returns exactly the token usage each request asks for
  2. starts New API (SQLite) and configures groups, prices, channels, users and keys
  3. sends requests that exercise ratio, expression (tiered), fixed-price and
     Anthropic-semantics billing, plus an upstream error
  4. runs `uplink export -once`, starts `uplink publish`, runs `uplink check`
  5. checks key scoping and saves every response for schema validation

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
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

MOCK_PORT = 18081
NEWAPI_PORT = 13000
PUBLISH_PORT = 18790
NEWAPI = f"http://127.0.0.1:{NEWAPI_PORT}"
PUBLISH = f"http://127.0.0.1:{PUBLISH_PORT}"
# Everything is on 127.0.0.1: never route through a system proxy.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))
for _k in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"):
    os.environ.pop(_k, None)
os.environ["NO_PROXY"] = os.environ["no_proxy"] = "127.0.0.1,localhost"
ROOT_PASS = "root-pass-" + secrets.token_hex(4)
USER_PASS = "user-pass-" + secrets.token_hex(4)


# ---------------------------------------------------------------- mock upstream

class Mock(BaseHTTPRequestHandler):
    """Usage is chosen by the caller: OpenAI `user` / Anthropic metadata.user_id
    of the form mock:<a>:<b>:... (see below)."""

    def log_message(self, *a):
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        if self.path.endswith("/chat/completions"):
            spec = req.get("user", "")
            if spec == "mock:error":
                return self._send(500, {"error": {"message": "mock upstream failure", "type": "server_error"}})
            # mock:<prompt_tokens incl. cache>:<cached>:<completion>
            _, prompt, cached, completion = spec.split(":")
            prompt, cached, completion = int(prompt), int(cached), int(completion)
            return self._send(200, {
                "id": "chatcmpl-mock", "object": "chat.completion", "created": int(time.time()),
                "model": req.get("model"),
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
                          "prompt_tokens_details": {"cached_tokens": cached}},
            })
        if self.path.endswith("/messages"):
            # mock:<input>:<cache_read>:<cache_write_5m>:<cache_write_1h>:<output>
            spec = (req.get("metadata") or {}).get("user_id", "")
            _, inp, cr, c5, c1, out = spec.split(":")
            inp, cr, c5, c1, out = map(int, (inp, cr, c5, c1, out))
            return self._send(200, {
                "id": "msg_mock", "type": "message", "role": "assistant", "model": req.get("model"),
                "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
                "usage": {"input_tokens": inp, "output_tokens": out, "cache_read_input_tokens": cr,
                          "cache_creation_input_tokens": c5 + c1,
                          "cache_creation": {"ephemeral_5m_input_tokens": c5, "ephemeral_1h_input_tokens": c1}},
            })
        self._send(404, {"error": {"message": "not found"}})


# ---------------------------------------------------------------- HTTP helpers

LAST_HEADERS = {}


def call(method, url, body=None, token=None, headers=None, expect=200):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with OPENER.open(req, timeout=60) as r:
            code, raw, hdrs = r.status, r.read(), r.headers
    except urllib.error.HTTPError as e:
        code, raw, hdrs = e.code, e.read(), e.headers
    LAST_HEADERS.clear()
    LAST_HEADERS.update({k.lower(): v for k, v in hdrs.items()})
    try:
        out = json.loads(raw) if raw else None
    except json.JSONDecodeError:
        out = raw.decode(errors="replace")
    if expect is not None and code != expect:
        raise RuntimeError(f"{method} {url} -> {code}: {out}")
    return code, out


def api(method, path, body=None, token=None, headers=None):
    _, out = call(method, NEWAPI + path, body, token, headers)
    if not isinstance(out, dict) or not out.get("success", False):
        raise RuntimeError(f"{method} {path}: {out}")
    return out.get("data")


def login(username, password):
    return api("POST", "/api/user/login", {"username": username, "password": password})["access_token"]


def wait_http(url, timeout=90):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with OPENER.open(url, timeout=2) as r:
                if r.status == 200:
                    return
        except Exception:
            pass
        time.sleep(0.5)
    raise RuntimeError(f"{url} did not come up")


# ---------------------------------------------------------------- scenario

EXPR = ('len <= 200000 ? tier("standard", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6) '
        ': tier("long_context", p * 6 + c * 22.5 + cr * 0.6 + cc * 7.5 + cc1h * 12)')


def configure(root):
    options = {
        "GroupRatio": json.dumps({"default": 1, "partner": 0.5}),
        "ModelRatio": json.dumps({"m-ratio": 1.25, "m-expr": 1.5, "claude-ratio": 1.5}),
        "CompletionRatio": json.dumps({"m-ratio": 8, "claude-ratio": 5}),
        "CacheRatio": json.dumps({"m-ratio": 0.1, "claude-ratio": 0.1}),
        "ModelPrice": json.dumps({"m-fixed": 0.02}),
        "billing_setting.billing_expr": json.dumps({"m-expr": EXPR}),
        "billing_setting.billing_mode": json.dumps({"m-expr": "tiered_expr"}),
    }
    for k, v in options.items():
        api("PUT", "/api/option/", {"key": k, "value": v}, root)

    api("POST", "/api/channel/", {"mode": "single", "channel": {
        "type": 1, "name": "mock-openai", "key": "sk-mock", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
        "models": "m-ratio,m-expr,m-fixed", "group": "default,partner", "status": 1}}, root)
    api("POST", "/api/channel/", {"mode": "single", "channel": {
        "type": 14, "name": "mock-anthropic", "key": "sk-ant-mock", "base_url": f"http://127.0.0.1:{MOCK_PORT}",
        "models": "claude-ratio", "group": "default,partner", "status": 1}}, root)


def make_user(root, username, group, quota):
    api("POST", "/api/user/", {"username": username, "password": USER_PASS, "display_name": username}, root)
    users = api("GET", f"/api/user/search?keyword={username}", token=root)
    items = users["items"] if isinstance(users, dict) else users
    u = next(x for x in items if x["username"] == username)
    u.update({"group": group, "password": ""})
    api("PUT", "/api/user/", u, root)
    if quota:
        api("POST", "/api/user/manage", {"id": u["id"], "action": "add_quota", "mode": "add", "value": quota}, root)
    return u["id"]


def make_key(user_token, name, remain_quota=0, unlimited=True, model_limits=""):
    api("POST", "/api/token/", {"name": name, "remain_quota": remain_quota, "unlimited_quota": unlimited,
                                "expired_time": -1, "group": "", "model_limits_enabled": bool(model_limits),
                                "model_limits": model_limits}, user_token)
    page = api("GET", "/api/token/?p=1&size=100", token=user_token)
    items = page["items"] if isinstance(page, dict) else page
    tid = next(x for x in items if x["name"] == name)["id"]
    key = api("POST", f"/api/token/{tid}/key", {}, user_token)
    key = key["key"] if isinstance(key, dict) else key
    return tid, key if key.startswith("sk-") else "sk-" + key


SENT_IDS = {}  # key -> request ids New API returned in X-Oneapi-Request-Id for settled calls


def remember(key):
    rid = LAST_HEADERS.get("x-oneapi-request-id")
    if not rid:
        raise RuntimeError("model call returned no X-Oneapi-Request-Id")
    SENT_IDS.setdefault(key, []).append(rid)


def make_pat(username):
    tok = login(username, USER_PASS)
    proof = api("POST", "/api/verify", {"method": "password", "scope": "access_token.generate", "password": USER_PASS}, tok)
    pat = api("POST", "/api/user/token", {}, tok, {"X-Security-Proof": proof["proof_token"]})
    return pat["token"] if isinstance(pat, dict) else pat


def chat(key, model, prompt, cached, completion):
    out = call("POST", NEWAPI + "/v1/chat/completions",
               {"model": model, "messages": [{"role": "user", "content": "hi"}], "user": f"mock:{prompt}:{cached}:{completion}"}, key)
    remember(key)
    return out


def chat_error(key, model):
    return call("POST", NEWAPI + "/v1/chat/completions",
                {"model": model, "messages": [{"role": "user", "content": "hi"}], "user": "mock:error"}, key, expect=None)


def messages(key, model, inp, cr, c5, c1, out):
    res = call("POST", NEWAPI + "/v1/messages",
               {"model": model, "max_tokens": 16, "messages": [{"role": "user", "content": "hi"}],
                "metadata": {"user_id": f"mock:{inp}:{cr}:{c5}:{c1}:{out}"}},
               key, headers={"anthropic-version": "2023-06-01"})
    remember(key)
    return res


def usage_ids(key, since, until, limit=1000):
    ids, cursor = [], None
    while True:
        q = f"/uplink/v0/usage?since={since}&until={until}&limit={limit}" + (f"&cursor={cursor}" if cursor else "")
        _, page = call("GET", PUBLISH + q, token=key)
        ids += [x["request_id"] for x in page["records"]]
        cursor = page["next_cursor"]
        if not cursor:
            return ids


def wait_for(pred, what, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        if pred():
            return
        time.sleep(0.5)
    raise RuntimeError(f"timed out waiting for {what}")


def run_check(uplink, key, save=None):
    args = [uplink, "check", "-base", PUBLISH] + (["-save", str(save)] if save else [])
    r = subprocess.run(args, env=dict(os.environ, UPLINK_KEY=key), capture_output=True, text=True)
    print(r.stdout.strip(), r.stderr.strip())
    return r.returncode == 0, r.stdout


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--newapi", required=True)
    ap.add_argument("--uplink", required=True)
    ap.add_argument("--work", required=True)
    a = ap.parse_args()
    work = Path(a.work)
    work.mkdir(parents=True, exist_ok=True)
    for f in work.glob("*.db*"):
        f.unlink()
    procs = []

    mock = ThreadingHTTPServer(("127.0.0.1", MOCK_PORT), Mock)
    threading.Thread(target=mock.serve_forever, daemon=True).start()

    env = dict(os.environ, PORT=str(NEWAPI_PORT), SQLITE_PATH=str(work / "newapi.db"),
               SESSION_SECRET=secrets.token_hex(16), ERROR_LOG_ENABLED="true", TZ="UTC")
    newapi_log = open(work / "newapi.log", "w")
    procs.append(subprocess.Popen([a.newapi], cwd=work, env=env, stdout=newapi_log, stderr=subprocess.STDOUT))
    try:
        wait_http(NEWAPI + "/api/status")
        api("POST", "/api/setup", {"username": "root", "password": ROOT_PASS, "confirmPassword": ROOT_PASS})
        root = login("root", ROOT_PASS)
        configure(root)

        make_user(root, "probe_partner", "partner", 0)
        make_user(root, "reseller", "partner", 500_000 * 100)
        make_user(root, "retail", "default", 500_000 * 10)
        pat = make_pat("probe_partner")
        reseller = login("reseller", USER_PASS)
        _, key_a = make_key(reseller, "key-a")
        _, key_b = make_key(reseller, "key-b")
        _, key_c = make_key(reseller, "key-c", model_limits="m-ratio")
        tid_d, key_d = make_key(reseller, "key-d")
        _, key_e = make_key(login("retail", USER_PASS), "key-e")   # another group
        print("setup done")

        failures = []
        since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 3600))
        until = lambda: time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 60))

        # Batch 1, before any export. Usage values cover each billing path.
        chat(key_a, "m-ratio", 10432, 9984, 812)          # ratio + cache read
        chat(key_a, "m-ratio", 7, 0, 3)                   # tiny: minimum charge / rounding
        chat(key_a, "m-expr", 50000, 20000, 1000)         # expression, standard tier
        chat(key_a, "m-expr", 200000, 0, 100)             # exactly at the threshold
        chat(key_a, "m-expr", 200001, 100000, 100)        # one over -> long tier
        chat(key_a, "m-fixed", 1000, 0, 1000)             # fixed price per request
        messages(key_a, "claude-ratio", 1200, 30000, 4000, 2000, 500)  # Anthropic semantics, 5m + 1h writes
        chat_error(key_a, "m-ratio")                      # upstream failure -> error log
        chat(key_b, "m-ratio", 100, 0, 10)
        chat(key_e, "m-ratio", 100, 0, 10)                # default group: must not show in partner status

        # Exporter as a live loop, publisher alongside it.
        exp_dir = work / "export"
        exp_dir.mkdir(exist_ok=True)
        (work / "export.yaml").write_text(f"""
newapi:
  base_url: {NEWAPI}
  db_path: {work / 'newapi.db'}
  probes:
    - group: partner
      access_token_env: UPLINK_PROBE_PARTNER
currency: CNY
price_per_usd: "1"
export_db: {exp_dir / 'export.db'}
interval: 2s
""")
        exp_log = open(work / "export.log", "w")
        exporter = subprocess.Popen([a.uplink, "export", "-config", str(work / "export.yaml")],
                                    env=dict(os.environ, UPLINK_PROBE_PARTNER=pat), stdout=exp_log, stderr=subprocess.STDOUT)
        procs.append(exporter)
        wait_for(lambda: (exp_dir / "export.db").exists(), "first export")
        time.sleep(3)

        def publish_config(name, stale):
            (work / name).write_text(f"""
listen: 127.0.0.1:{PUBLISH_PORT}
export_db: {exp_dir / 'export.db'}
api_base: {NEWAPI}/v1
contact: support@example.com
summary_timezone: UTC
stale_after: {stale}
""")
            return work / name

        pub_log = open(work / "publish.log", "w")
        publisher = subprocess.Popen([a.uplink, "publish", "-config", str(publish_config("publish.yaml", "10s"))],
                                     stdout=pub_log, stderr=subprocess.STDOUT)
        procs.append(publisher)
        wait_http(PUBLISH + "/.well-known/uplink.json")

        print("---- check 1 (key A, batch 1)")
        ok, _ = run_check(a.uplink, key_a)
        if not ok:
            failures.append("check 1 failed")

        # Batch 2 while the exporter keeps running: incremental export, no duplicates or gaps.
        chat(key_a, "m-ratio", 3000, 1000, 200)
        chat(key_a, "m-expr", 250000, 50000, 300)
        messages(key_a, "claude-ratio", 500, 0, 0, 3000, 100)
        chat(key_c, "m-ratio", 100, 0, 10)
        wait_for(lambda: len(usage_ids(key_a, since, until())) == 10, "batch 2 in usage", timeout=30)

        saved = work / "responses"
        print("---- check 2 (key A, batches 1+2)")
        ok, out = run_check(a.uplink, key_a, saved)
        if not ok or "records: 10, matched: 10" not in out:
            failures.append("check 2 failed")

        # request_id must be exactly the X-Oneapi-Request-Id each caller received.
        ids_a = usage_ids(key_a, since, until())
        if sorted(ids_a) != sorted(SENT_IDS[key_a]):
            failures.append(f"request ids differ from response headers: published={sorted(ids_a)} sent={sorted(SENT_IDS[key_a])}")
        if len(ids_a) != len(set(ids_a)):
            failures.append("duplicate records across export cycles")

        # Key scoping and pagination.
        ids_b = usage_ids(key_b, since, until())
        if len(ids_b) != 1 or set(ids_a) & set(ids_b):
            failures.append(f"key scoping: A={len(ids_a)} B={len(ids_b)}")
        if usage_ids(key_a, since, until(), limit=3) != ids_a:
            failures.append("pagination does not reproduce the unpaged order")

        # Model limits: key C sees only m-ratio.
        _, cat_c = call("GET", PUBLISH + "/uplink/v0/catalog", token=key_c)
        if [m["id"] for m in cat_c["models"]] != ["m-ratio"]:
            failures.append(f"model limits: {[m['id'] for m in cat_c['models']]}")

        # Status covers only the key's group: partner m-ratio = A 2+1 error + A 1 + B 1 + C 1 = 6; retail excluded.
        _, st = call("GET", PUBLISH + "/uplink/v0/status", token=key_a)
        mr = next(m for m in st["models"] if m["id"] == "m-ratio")
        if mr["sample_size"] != 6 or mr["success_rate"] != "0.833":
            failures.append(f"status scoping: {mr}")

        # Disabled key -> 401 after the next export.
        call("GET", PUBLISH + "/uplink/v0/account", token=key_d)
        api("PUT", "/api/token/?status_only=true", {"id": tid_d, "status": 2}, reseller)
        wait_for(lambda: call("GET", PUBLISH + "/uplink/v0/account", token=key_d, expect=None)[0] == 401, "disabled key refused")

        # Errors, redaction, ETag.
        code, body = call("GET", PUBLISH + "/uplink/v0/catalog", token="sk-" + "x" * 48, expect=None)
        if code != 401 or body["error"]["code"] != "invalid_api_key":
            failures.append(f"unknown key: {code} {body}")
        code, body = call("GET", PUBLISH + "/uplink/v0/usage?since=2026-01-01T00:00:00Z&until=2026-03-01T00:00:00Z", token=key_a, expect=None)
        if code != 400 or body["error"]["code"] != "range_too_large":
            failures.append(f"range check: {code} {body}")
        code, _ = call("GET", PUBLISH + "/uplink/v0/catalog?key=" + key_a, expect=None)
        if code != 401:
            failures.append("key in query string was accepted")
        _, cat = call("GET", PUBLISH + "/uplink/v0/catalog", token=key_a)
        etag = LAST_HEADERS.get("etag")
        blob = json.dumps(cat)
        for leak in ("partner", "mock-openai", "mock-anthropic", "127.0.0.1:18081"):
            if leak in blob:
                failures.append(f"catalog leaks {leak!r}")
        code, _ = call("GET", PUBLISH + "/uplink/v0/catalog", token=key_a, headers={"If-None-Match": etag}, expect=None)
        if code != 304:
            failures.append(f"If-None-Match: {code}")
        for name, path in [("usage_summary", f"/uplink/v0/usage/summary?since={since}&until={until()}"),
                           ("error", "/uplink/v0/usage?since=bad&until=bad")]:
            _, body = call("GET", PUBLISH + path, token=key_a, expect=None)
            (saved / f"{name}.json").write_text(json.dumps(body))

        # Exporter stops -> publisher refuses stale data.
        exporter.send_signal(signal.SIGTERM)
        exporter.wait(timeout=10)
        wait_for(lambda: call("GET", PUBLISH + "/uplink/v0/account", token=key_a, expect=None)[0] == 503, "stale data refused", timeout=30)
        print("stale export refused with 503")

        # Publisher on a read-only directory (as with a read-only mount).
        publisher.send_signal(signal.SIGTERM)
        publisher.wait(timeout=10)
        os.chmod(exp_dir / "export.db", 0o444)
        os.chmod(exp_dir, 0o555)
        try:
            publisher = subprocess.Popen([a.uplink, "publish", "-config", str(publish_config("publish-ro.yaml", "1h"))],
                                         stdout=pub_log, stderr=subprocess.STDOUT)
            procs.append(publisher)
            wait_http(PUBLISH + "/.well-known/uplink.json")
            print("---- check 3 (read-only export directory)")
            ok, out = run_check(a.uplink, key_a)
            if not ok or "records: 10, matched: 10" not in out:
                failures.append("check on read-only directory failed")
            if any(p.name != "export.db" for p in exp_dir.iterdir()):
                failures.append(f"publisher created files: {[p.name for p in exp_dir.iterdir()]}")
        finally:
            os.chmod(exp_dir, 0o755)
            os.chmod(exp_dir / "export.db", 0o644)

        print("responses saved to", saved)
        if failures:
            print("E2E FAIL")
            for f in failures:
                print("  -", f)
            return 1
        print("E2E OK")
        return 0
    finally:
        for p in procs:
            if p.poll() is not None:
                continue
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
        mock.shutdown()


if __name__ == "__main__":
    sys.exit(main())
