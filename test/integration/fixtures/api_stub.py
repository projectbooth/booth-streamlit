"""A stand-in for booth-api's app write route and revoke-principal (ADR 0117), for real-core tests.

Registered with the real booth-core as module "api" (deploy-realcore.sh), so every request reaches
it through core's gateway, which has already verified the caller's token and stripped
/modules/api. It implements only what booth-streamlit calls, as booth-api's contract
(docs/contracts/app-writes.md on its app-writes branch) describes it:

    POST|PATCH|DELETE /app/v1/{slug}/rows[/{key}]    an app's write, with the app's workload token
    POST /api/write-grants/revoke-principal          booth-streamlit's revoke-only token
                                                    (subject streamlit:<ws>:grants), ADR 0117 item 1a

and records every request with the claims its token carried, for the test to read. The grants are
set by the test on its own port, never through core:

    POST /stub/grant {"principal", "api", "operations"}    POST /stub/fail-revoke {"on": bool}
    GET  /stub/log                                         GET  /stub/grants

It does not verify signatures: core's gateway did, before forwarding. booth-api's own verification
is booth-api's test.
"""

import base64
import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
grants = {}  # principal -> {api slug: [operations]}
log = []
state = {"fail_revoke": False, "n": 0}

ROW = re.compile(r"^/app/v1/([a-z0-9-]+)/rows(?:/([^/?]+))?(?:\?.*)?$")
OPS = {"POST": "insert", "PATCH": "update", "DELETE": "delete"}


def claims(auth):
    if not auth.startswith("Bearer "):
        return {}
    try:
        payload = auth[7:].split(".")[1]
        return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    except (IndexError, ValueError):
        return {}


class H(BaseHTTPRequestHandler):
    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n)) if n else None

    def _send(self, code, payload):
        raw = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _err(self, code, err, msg):
        self._send(code, {"error": {"code": err, "message": msg}})

    def do_GET(self):  # noqa: N802
        if self.path == "/healthz":
            return self._send(200, {"status": "ok"})
        if self.path == "/stub/log":
            with lock:
                return self._send(200, log)
        if self.path == "/stub/grants":
            with lock:
                return self._send(200, grants)
        return self._err(405, "invalid_request", "no reads here")

    def do_POST(self):  # noqa: N802
        self._handle("POST")

    def do_PATCH(self):  # noqa: N802
        self._handle("PATCH")

    def do_DELETE(self):  # noqa: N802
        self._handle("DELETE")

    def _handle(self, method):
        body = self._body()
        if self.path == "/stub/grant":
            with lock:
                grants.setdefault(body["principal"], {})[body["api"]] = body.get("operations") or ["insert"]
            return self._send(200, grants)
        if self.path == "/stub/fail-revoke":
            state["fail_revoke"] = bool(body.get("on"))
            return self._send(200, state)

        c = claims(self.headers.get("Authorization", ""))
        ws = self.headers.get("X-Workspace", "")
        entry = {
            "method": method, "path": self.path, "sub": c.get("sub"), "booth_module": c.get("booth_module"),
            "workspace": ws, "appUser": self.headers.get("X-Booth-App-User"),
            "idempotencyKey": self.headers.get("Idempotency-Key"), "body": body,
        }
        with lock:
            log.append(entry)

        if self.path == "/api/write-grants/revoke-principal" and method == "POST":
            # Item 1a: only streamlit's own revoke token, only for its workspace's app principals.
            principal = (body or {}).get("principal", "")
            if c.get("sub") != f"streamlit:{ws}:grants" or c.get("booth_module") != "streamlit":
                return self._send(401, {"error": "unauthenticated"})
            if not principal.startswith(f"streamlit:{ws}:") or principal.endswith((":grants", ":sources")):
                return self._send(400, {"error": "not an app principal in this workspace"})
            if state["fail_revoke"]:
                return self._send(503, {"error": "booth-api is unavailable (stub)"})
            with lock:
                gone = grants.pop(principal, {})
            return self._send(200, {"revoked": len(gone), "items": []})

        m = ROW.match(self.path)
        if not m:
            return self._err(404, "not_found", "no such route")
        sub = c.get("sub") or ""
        if c.get("booth_module") != "streamlit" or not sub.startswith(f"streamlit:{ws}:"):
            return self._err(403, "not_an_app", "not a booth-streamlit app's token")
        op = OPS[method]
        with lock:
            allowed = grants.get(sub, {}).get(m.group(1))
            if allowed is None:
                return self._err(403, "not_granted", "no live grant for this app on this API")
            if op not in allowed:
                return self._err(403, "operation_not_allowed", f"{op} is not granted")
            state["n"] += 1
            n = state["n"]
        row = dict((body or {}).get("data") or {})
        row.setdefault("id", str(n))
        return self._send(201 if op == "insert" else 200, {"data": row, "version": f"v{n}"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), H).serve_forever()
