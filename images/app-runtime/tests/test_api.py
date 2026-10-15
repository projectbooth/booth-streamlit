import datetime
import decimal
import http.server
import json
import threading

import pytest

import booth_streamlit as bs
from booth_streamlit import api


@pytest.fixture()
def loopback(monkeypatch):
    """A stand-in for the gate's loopback /writes listener: records requests and answers from a
    queue of (status, body, headers), the last one repeating."""
    seen = []
    answers = [(201, {"data": {"id": "1", "amount": "12.50"}, "version": "v1"}, {})]

    class H(http.server.BaseHTTPRequestHandler):
        def _handle(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n) if n else b""
            seen.append({"method": self.command, "path": self.path, "headers": dict(self.headers), "body": json.loads(body) if body else None})
            status, payload, hdrs = answers.pop(0) if len(answers) > 1 else answers[0]
            raw = json.dumps(payload).encode()
            self.send_response(status)
            for k, v in hdrs.items():
                self.send_header(k, v)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        do_POST = do_PATCH = do_DELETE = _handle  # noqa: N815

        def log_message(self, *a):
            pass

    srv = http.server.HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    monkeypatch.setenv("BOOTH_WRITES_URL", f"http://127.0.0.1:{srv.server_port}/writes")
    monkeypatch.setattr(bs, "user", lambda: bs.User(subject="viewer-7", workspace="acme", role="viewer"))
    monkeypatch.setattr(api, "_MAX_WAIT", 0.01)
    yield seen, answers
    srv.shutdown()


def test_insert_records_the_viewer_and_sends_one_idempotency_key(loopback):
    seen, _ = loopback
    w = api.insert("household", {"amount": decimal.Decimal("12.50"), "on": datetime.date(2026, 10, 15), "memo": None})
    assert w.row == {"id": "1", "amount": "12.50"} and w.version == "v1" and not w.replayed
    req = seen[0]
    assert req["method"] == "POST" and req["path"] == "/writes/household/rows"
    assert req["body"] == {"data": {"amount": "12.50", "on": "2026-10-15", "memo": None, "entered_by": "viewer-7"}}
    assert req["headers"]["X-Booth-App-User"] == "viewer-7"
    assert len(req["headers"]["Idempotency-Key"]) == 36
    assert "Authorization" not in req["headers"]  # the gate adds the app's identity, never app code


def test_entered_by_column_choice_and_opt_out(loopback):
    seen, _ = loopback
    api.insert("household", {"amount": "1", "entered_by": "someone-else"})
    api.insert("household", {"amount": "1"}, entered_by="added_by")
    api.insert("household", {"amount": "1"}, entered_by=None)
    assert [r["body"]["data"] for r in seen] == [
        {"amount": "1", "entered_by": "someone-else"},
        {"amount": "1", "added_by": "viewer-7"},
        {"amount": "1"},
    ]


def test_no_viewer_is_not_an_anonymous_row(loopback, monkeypatch):
    seen, _ = loopback
    monkeypatch.setattr(bs, "user", lambda: None)
    with pytest.raises(api.NotSignedIn):
        api.insert("household", {"amount": "1"})
    assert seen == []
    api.insert("household", {"amount": "1"}, entered_by=None)  # an explicit opt-out still writes
    assert "X-Booth-App-User" not in seen[0]["headers"]


def test_update_and_delete_key_forms(loopback):
    seen, answers = loopback
    answers[:] = [(200, {"data": {"id": "a b"}, "version": "v2"}, {"Idempotent-Replayed": "true"})]
    w = api.update("household", "a b", {"memo": "x"}, expected_version="v1")
    assert w.version == "v2" and w.replayed
    assert seen[0]["method"] == "PATCH" and seen[0]["path"] == "/writes/household/rows/a%20b"
    assert seen[0]["headers"]["If-Match"] == '"v1"'
    api.update("budget", {"month": "2026-10", "category": "food"}, {"limit": "400"}, entered_by=None)
    assert seen[1]["path"] == "/writes/budget/rows?key%5Bmonth%5D=2026-10&key%5Bcategory%5D=food"
    api.delete("household", 42)
    assert seen[2]["method"] == "DELETE" and seen[2]["path"] == "/writes/household/rows/42" and seen[2]["body"] is None
    with pytest.raises(ValueError):
        api.delete("household", "a/b")  # use the {field: value} form
    with pytest.raises(ValueError):
        api.insert("../x", {})


def test_invalid_row_carries_the_fields(loopback):
    _, answers = loopback
    fields = [{"field": "amount", "problem": "required"}]
    answers[:] = [(422, {"error": {"code": "validation", "message": "row is invalid", "fields": fields}}, {})]
    with pytest.raises(api.InvalidRow) as e:
        api.insert("household", {})
    assert e.value.fields == fields and e.value.status == 422 and e.value.code == "validation"


def test_refusals(loopback):
    _, answers = loopback
    msg = "This app has no write grant on API \"household\". A write also needs an owner's grant on the API page: ..."
    answers[:] = [(403, {"error": {"code": "not_granted", "message": msg}}, {})]
    with pytest.raises(api.WriteRefused) as e:
        api.insert("household", {})
    assert e.value.code == "not_granted" and "also needs an owner's grant on the API page" in str(e.value)
    assert not isinstance(e.value, api.InvalidRow)

    answers[:] = [(403, {"error": {"code": "not_a_write_target", "message": "not declared"}}, {})]
    with pytest.raises(api.WriteRefused) as e:
        api.delete("household", 1)
    assert e.value.code == "not_a_write_target"

    answers[:] = [(403, {"error": "data_access_paused", "reason": "the owner left"}, {})]
    with pytest.raises(bs.DataAccessPaused, match="the owner left"):
        api.insert("household", {})

    answers[:] = [(409, {"error": {"code": "conflict", "message": "duplicate", "constraint": "household_pkey"}}, {})]
    with pytest.raises(api.WriteRefused) as e:
        api.insert("household", {})
    assert e.value.constraint == "household_pkey" and e.value.status == 409


def test_retries_reuse_the_idempotency_key(loopback):
    seen, answers = loopback
    answers[:] = [
        (429, {"error": {"code": "rate_limited", "message": "slow down"}}, {"Retry-After": "1"}),
        (503, {"error": {"code": "unavailable", "message": "no lease"}}, {}),
        (201, {"data": {"id": "9"}, "version": "v9"}, {}),
    ]
    assert api.insert("household", {}).row == {"id": "9"}
    keys = {r["headers"]["Idempotency-Key"] for r in seen}
    assert len(seen) == 3 and len(keys) == 1


def test_gives_up_after_three_attempts(loopback):
    seen, answers = loopback
    answers[:] = [(429, {"error": {"code": "rate_limited", "message": "slow down"}}, {"Retry-After": "0"})]
    with pytest.raises(api.WriteRefused) as e:
        api.insert("household", {})
    assert e.value.code == "rate_limited" and len(seen) == 3


def test_validation_is_not_retried(loopback):
    seen, answers = loopback
    answers[:] = [(422, {"error": {"code": "constraint", "message": "fk"}}, {})]
    with pytest.raises(api.WriteRefused):
        api.insert("household", {})
    assert len(seen) == 1


def test_no_writes_url_says_why(monkeypatch):
    monkeypatch.delenv("BOOTH_WRITES_URL", raising=False)
    monkeypatch.setattr(bs, "user", lambda: bs.User(subject="v", workspace="acme", role="viewer"))
    with pytest.raises(bs.DataAccessOff, match="Write targets"):
        api.insert("household", {"a": 1})
