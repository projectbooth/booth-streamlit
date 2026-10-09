"""Read booth-storage objects and catalog file datasets from an app (ADR 0107).

Everything is read as the app's owner, capped at viewer, read only, in the app's own workspace.
There is nothing to configure and no credential in your code: requests go to this pod's gate on
loopback, which adds the app's identity and passes them to the module.

    from booth_streamlit import files

    files.read("my-backend", "sales/q1.csv")          # bytes
    files.list("my-backend", prefix="sales/")          # [{"path": ..., "size": ...}, ...]
    files.dataset("<dataset id>")                       # {"name", "format", "location", ...}
    files.read_dataset("<dataset id>", "sales/q1.csv")  # a file within the dataset's location

Errors: DataAccessPaused (the owner lost access; the message says why), FileNotFoundError (no such
object, backend or dataset in this workspace), PermissionError, ValueError (a path the module
refuses, e.g. ".."), RuntimeError for anything else.
"""

from __future__ import annotations

import json as _json
import os as _os
import urllib.error as _urlerr
import urllib.parse as _urlparse
import urllib.request as _urlreq
from typing import Iterator, Optional

from . import DataAccessOff, DataAccessPaused

__all__ = ["read", "open", "list", "dataset", "read_dataset"]

_TIMEOUT = 330  # a little over the module's 5-minute object timeout


def _base() -> str:
    url = _os.environ.get("BOOTH_FILES_URL")
    if not url:
        raise DataAccessOff("this app has no file access (the module's data access is not enabled)")
    return url.rstrip("/")


def _quote_path(path: str) -> str:
    # Each segment escaped on its own; "/" stays a separator. The module refuses "..", "." and
    # empty segments, so there is nothing to normalize here.
    return "/".join(_urlparse.quote(seg, safe="") for seg in path.split("/"))


def _request(path: str, query: Optional[dict] = None):
    url = _base() + path
    if query:
        url += "?" + _urlparse.urlencode(query)
    try:
        return _urlreq.urlopen(_urlreq.Request(url, method="GET"), timeout=_TIMEOUT)  # noqa: S310 - fixed loopback URL
    except _urlerr.HTTPError as err:
        body = err.read(4096)
        try:
            payload = _json.loads(body)
        except ValueError:
            payload = {"error": body.decode(errors="replace")}
        msg = payload.get("reason") or payload.get("error") or f"HTTP {err.code}"
        if err.code == 403 and payload.get("error") == "data_access_paused":
            raise DataAccessPaused(msg) from None
        if err.code == 404:
            raise FileNotFoundError(path) from None
        if err.code == 403:
            raise PermissionError(msg) from None
        if err.code == 400:
            raise ValueError(msg) from None
        raise RuntimeError(f"file proxy answered {err.code}: {msg}") from None


def open(backend_id: str, path: str):  # noqa: A001 - mirrors the builtin on purpose
    """A readable binary stream of one object; close it when done."""
    return _request(f"/storage/{_urlparse.quote(backend_id, safe='')}/{_quote_path(path)}")


def read(backend_id: str, path: str) -> bytes:
    """One object's bytes (at most 512 MiB by default)."""
    with open(backend_id, path) as resp:
        return resp.read()


def list(backend_id: str, prefix: str = "", recursive: bool = False) -> "list[dict]":  # noqa: A001
    """Every entry under prefix, following the module's pages."""
    return [e for e in _entries(backend_id, prefix, recursive)]


def _entries(backend_id: str, prefix: str, recursive: bool) -> Iterator[dict]:
    cursor = ""
    while True:
        q = {"prefix": prefix, "recursive": "true" if recursive else "false"}
        if cursor:
            q["cursor"] = cursor
        with _request(f"/storage/{_urlparse.quote(backend_id, safe='')}", q) as resp:
            page = _json.load(resp)
        yield from page.get("entries") or []
        cursor = page.get("nextCursor") or ""
        if not cursor:
            return


def dataset(dataset_id: str) -> dict:
    """A catalog dataset in this workspace: id, name, description, format, location."""
    with _request(f"/datasets/{_urlparse.quote(dataset_id, safe='')}") as resp:
        return _json.load(resp)


def read_dataset(dataset_id: str, path: Optional[str] = None) -> bytes:
    """A file of a "file" dataset: its location itself when that is one object, or path, which
    must be the location or under it."""
    if path is None:
        path = dataset(dataset_id)["location"]["path"].strip("/")
    with _request(f"/datasets/{_urlparse.quote(dataset_id, safe='')}/content/{_quote_path(path)}") as resp:
        return resp.read()
