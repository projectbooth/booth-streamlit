from booth_streamlit import User, user_from_headers


def test_reads_the_verified_headers_case_insensitively():
    got = user_from_headers({"x-booth-user": "3f1c", "X-Booth-Workspace": "acme", "X-BOOTH-ROLE": "editor"})
    assert got == User(subject="3f1c", workspace="acme", role="editor")


def test_none_outside_booth_or_on_incomplete_headers():
    assert user_from_headers({}) is None
    assert user_from_headers({"X-Booth-User": "u", "X-Booth-Workspace": "acme"}) is None
    assert user_from_headers({"X-Booth-User": "u", "X-Booth-Workspace": "acme", "X-Booth-Role": "admin"}) is None


def test_database_url_explains_why_it_cannot_work(monkeypatch):
    import http.server
    import json
    import threading

    import pytest

    import booth_streamlit as bs

    monkeypatch.delenv("DATABASE_URL", raising=False)
    monkeypatch.delenv("BOOTH_DATA_STATUS_URL", raising=False)
    with pytest.raises(bs.DataAccessOff):
        bs.database_url()
    assert bs.data_status() == {"state": "off"}

    state = {"state": "paused", "reason": "owner gone"}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            body = json.dumps(state).encode()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        monkeypatch.setenv("BOOTH_DATA_STATUS_URL", f"http://127.0.0.1:{srv.server_port}/_booth/data/status")
        monkeypatch.setenv("DATABASE_URL", "postgresql://localhost:5432/bdb_ws_x")
        with pytest.raises(bs.DataAccessPaused, match="owner gone"):
            bs.database_url()
        state.update(state="ok", reason="")
        assert bs.database_url() == "postgresql://localhost:5432/bdb_ws_x"
    finally:
        srv.shutdown()



def test_the_app_uid_has_a_passwd_entry():
    """libpq needs it to connect with a URL that names no user (DATABASE_URL); runs in the image."""
    import os

    if not hasattr(os, "getuid") or os.getuid() != 65532:  # only meaningful inside the runtime image
        return
    import pwd

    assert pwd.getpwuid(65532).pw_name == "booth"
