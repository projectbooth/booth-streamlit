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

Uploads (ADR 0117): an app whose owner declared a Storage folder in its Write targets, and granted it
on Storage's Admin page (App uploads), can add files there. Create-only: a file is never replaced,
deleted or listed through this call.

    path = files.upload("local", "finance/inbox/", "statement.csv", uploaded_file.getvalue())

It returns the stored path, {folder}{UTC time}-{6 random}-{name}, so two people uploading
statement.csv don't collide. Errors: UploadRefused (status, code, message: not_an_upload_target,
not_granted, extension_not_allowed, too_large, ...), DataAccessOff, DataAccessPaused.
"""

from __future__ import annotations

import json as _json
import mimetypes as _mimetypes
import os as _os
import time as _time
import urllib.error as _urlerr
import urllib.parse as _urlparse
import urllib.request as _urlreq
from typing import Any, Iterator, Optional

from . import DataAccessOff, DataAccessPaused

__all__ = ["read", "open", "list", "dataset", "read_dataset", "upload", "UploadRefused"]

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


class UploadRefused(Exception):
    """The upload was refused; nothing was stored. ``code`` says why (not_an_upload_target: the
    folder isn't declared in the app's Write targets; not_granted: no owner's grant on Storage's
    Admin page; extension_not_allowed, too_large, ...) and ``message`` says it in words."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(f"{message} ({status} {code})" if code else f"{message} ({status})")
        self.status = status
        self.code = code
        self.message = message


_UPLOAD_ATTEMPTS = 3


def _uploads_base() -> str:
    url = _os.environ.get("BOOTH_UPLOADS_URL")
    if not url:
        raise DataAccessOff(
            "this app can't upload: its owner hasn't declared any Storage folders (app settings, Write targets), "
            "or app uploads are off in this Booth install"
        )
    return url.rstrip("/")


def _bytes_of(data: Any) -> bytes:
    if isinstance(data, (bytes, bytearray, memoryview)):
        return bytes(data)
    if hasattr(data, "getvalue"):  # Streamlit's UploadedFile, io.BytesIO
        return bytes(data.getvalue())
    if hasattr(data, "read"):
        return bytes(data.read())
    raise TypeError(f"upload takes bytes or a binary file object, not {type(data).__name__}")


def upload(backend_id: str, folder: str, name: str, data: Any, content_type: Optional[str] = None) -> str:
    """Add one file to a declared Storage folder; returns its stored path.

    ``folder`` is the folder as declared ("" for the backend's root); ``name`` is one file name, no
    "/". ``data`` is bytes or a binary file object (such as st.file_uploader's result).
    """
    body = _bytes_of(data)
    url = _uploads_base() + "/" + _urlparse.quote(backend_id, safe="") + "?" + _urlparse.urlencode({"prefix": folder, "name": name})
    # The built-in table, not the platform's (a Windows registry calls .csv vnd.ms-excel).
    ct = content_type or _mimetypes.MimeTypes().guess_type(name)[0] or "application/octet-stream"
    for attempt in range(_UPLOAD_ATTEMPTS):
        req = _urlreq.Request(url, data=body, method="POST", headers={"Content-Type": ct})
        try:
            with _urlreq.urlopen(req, timeout=_TIMEOUT) as resp:  # noqa: S310 - fixed loopback URL from the pod spec
                return _json.load(resp)["path"]
        except _urlerr.HTTPError as err:
            status, raw = err.code, err.read(64 << 10)
        code, message = _upload_error(status, raw)
        if status == 403 and code == "data_access_paused":
            raise DataAccessPaused(message)
        # Only the module's own "too many at once" is retried: nothing reached booth-storage. Any
        # other answer may have stored the file, and a retry would store it twice.
        if status == 429 and code == "too_many_uploads" and attempt < _UPLOAD_ATTEMPTS - 1:
            _time.sleep(0.5 * 2**attempt)
            continue
        raise UploadRefused(status, code, message)
    raise AssertionError("unreachable")


def _upload_error(status: int, raw: bytes) -> "tuple[str, str]":
    try:
        payload = _json.loads(raw)
    except ValueError:
        return "", raw.decode(errors="replace") or f"HTTP {status}"
    err = payload.get("error") if isinstance(payload, dict) else None
    if isinstance(err, dict):
        return err.get("code", ""), err.get("message", "") or f"HTTP {status}"
    if isinstance(err, str):  # the internal port's own shape (data_access_paused, unauthorized)
        return err, payload.get("reason") or err
    return "", f"HTTP {status}"
