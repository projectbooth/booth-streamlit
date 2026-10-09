import http.server
import json
import threading

import pytest

import booth_streamlit as bs
from booth_streamlit import files


@pytest.fixture()
def loopback(monkeypatch):
    """A stand-in for the gate's loopback /files listener: records paths, answers per path."""
    seen = []
    pages = {
        "/files/storage/b?prefix=s%2F&recursive=false": {"entries": [{"path": "s/1"}], "nextCursor": "c2"},
        "/files/storage/b?prefix=s%2F&recursive=false&cursor=c2": {"entries": [{"path": "s/2"}]},
    }

    class H(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            seen.append(self.path)
            if self.path in pages:
                return self._send(200, json.dumps(pages[self.path]).encode())
            if self.path == "/files/storage/b/sales/q%201.csv":
                return self._send(200, b"a,b\n1,2\n")
            if self.path == "/files/datasets/ds1":
                return self._send(200, json.dumps({"id": "ds1", "location": {"backendId": "b", "path": "sales/q 1.csv"}}).encode())
            if self.path == "/files/datasets/ds1/content/sales/q%201.csv":
                return self._send(200, b"dataset bytes")
            if self.path.startswith("/files/storage/paused"):
                return self._send(403, json.dumps({"error": "data_access_paused", "reason": "owner gone"}).encode())
            if self.path.startswith("/files/storage/b/.."):
                return self._send(400, json.dumps({"error": '".." not allowed'}).encode())
            return self._send(404, b'{"error":"not found"}')

        def _send(self, code, body):
            self.send_response(code)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):
            pass

    srv = http.server.HTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    monkeypatch.setenv("BOOTH_FILES_URL", f"http://127.0.0.1:{srv.server_port}/files")
    yield seen
    srv.shutdown()


def test_reads_lists_and_datasets(loopback):
    assert files.read("b", "sales/q 1.csv") == b"a,b\n1,2\n"
    assert [e["path"] for e in files.list("b", prefix="s/")] == ["s/1", "s/2"]
    assert files.dataset("ds1")["location"]["backendId"] == "b"
    assert files.read_dataset("ds1") == b"dataset bytes"  # the location itself
    assert files.read_dataset("ds1", "sales/q 1.csv") == b"dataset bytes"


def test_errors_say_what_happened(loopback):
    with pytest.raises(bs.DataAccessPaused, match="owner gone"):
        files.read("paused", "x")
    with pytest.raises(FileNotFoundError):
        files.read("b", "nope.csv")
    with pytest.raises(ValueError):
        files.read("b", "../secret")
    # ".." goes to the module as written (it refuses it); the helper never normalizes a path.
    assert loopback[-1] == "/files/storage/b/../secret"


def test_no_file_access_without_the_module(monkeypatch):
    monkeypatch.delenv("BOOTH_FILES_URL", raising=False)
    with pytest.raises(bs.DataAccessOff):
        files.read("b", "x")
