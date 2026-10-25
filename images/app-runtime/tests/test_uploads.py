import http.server
import io
import json
import threading

import pytest

import booth_streamlit as bs
from booth_streamlit import DataAccessOff, DataAccessPaused, files


@pytest.fixture()
def loopback(monkeypatch):
    """A stand-in for the gate's loopback /uploads listener: records requests, answers from a queue."""
    seen = []
    answers = []

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            seen.append({"path": self.path, "body": body, "content_type": self.headers.get("Content-Type"), "user": self.headers.get("X-Booth-App-User")})
            code, payload = answers.pop(0) if answers else (201, {"backend": "local", "path": "finance/inbox/20261015T090000Z-abc123-s.csv", "size": len(body)})
            raw = json.dumps(payload).encode()
            self.send_response(code)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *a):
            pass

    srv = http.server.HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    monkeypatch.setenv("BOOTH_UPLOADS_URL", f"http://127.0.0.1:{srv.server_port}/uploads")
    monkeypatch.setattr(files._time, "sleep", lambda s: None)
    yield seen, answers
    srv.shutdown()


def test_upload_returns_the_stored_path(loopback):
    seen, _ = loopback
    path = files.upload("local", "finance/inbox/", "s.csv", b"a,b\n")
    assert path == "finance/inbox/20261015T090000Z-abc123-s.csv"
    assert seen[0]["path"] == "/uploads/local?prefix=finance%2Finbox%2F&name=s.csv"
    assert seen[0]["body"] == b"a,b\n"
    assert seen[0]["content_type"] == "text/csv"


def test_upload_names_the_viewer_for_the_prompt(loopback, monkeypatch):
    seen, _ = loopback
    monkeypatch.setattr(bs, "user", lambda: bs.User(subject="viewer-7", workspace="acme", role="viewer"))
    files.upload("local", "finance/inbox/", "s.csv", b"x")
    monkeypatch.setattr(bs, "user", lambda: None)
    files.upload("local", "finance/inbox/", "s.csv", b"x")
    assert [s["user"] for s in seen] == ["viewer-7", None]


def test_upload_takes_file_objects(loopback):
    seen, _ = loopback
    files.upload("local", "", "a b.pdf", io.BytesIO(b"%PDF"))
    assert seen[0]["path"] == "/uploads/local?prefix=&name=a+b.pdf"
    assert seen[0]["body"] == b"%PDF"
    assert seen[0]["content_type"] == "application/pdf"
    with pytest.raises(TypeError):
        files.upload("local", "", "x.csv", "not bytes")


def test_refusals_carry_the_code(loopback):
    _, answers = loopback
    answers.append((403, {"error": {"code": "not_granted", "message": "also needs an owner's grant on the Storage page"}}))
    with pytest.raises(files.UploadRefused) as e:
        files.upload("local", "finance/inbox/", "s.csv", b"x")
    assert (e.value.status, e.value.code) == (403, "not_granted")
    assert "Storage page" in e.value.message
    answers.append((403, {"error": "data_access_paused", "reason": "owner gone"}))
    with pytest.raises(DataAccessPaused, match="owner gone"):
        files.upload("local", "finance/inbox/", "s.csv", b"x")


def test_only_the_modules_own_busy_answer_is_retried(loopback):
    seen, answers = loopback
    answers.append((429, {"error": {"code": "too_many_uploads", "message": "at most 4"}}))
    assert files.upload("local", "finance/inbox/", "s.csv", b"x").endswith("-s.csv")
    assert len(seen) == 2
    # A failure after the request reached booth-storage may have stored the file: never retried.
    seen.clear()
    answers.append((502, {"error": {"code": "unavailable", "message": "booth-storage could not be reached"}}))
    with pytest.raises(files.UploadRefused):
        files.upload("local", "finance/inbox/", "s.csv", b"x")
    assert len(seen) == 1


def test_no_uploads_url_says_why(monkeypatch):
    monkeypatch.delenv("BOOTH_UPLOADS_URL", raising=False)
    with pytest.raises(DataAccessOff, match="Write targets"):
        files.upload("local", "", "s.csv", b"x")
