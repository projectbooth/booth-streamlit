"""Write rows through booth-api from an app (ADR 0117).

An app can add, change or delete rows in a table that booth-api serves, when two things are true:
the app's owner declared the API in the app's Write targets (in Streamlit), and an owner granted
the app on the API's own page (in the API module). The write is made as the app, so anyone who can
open the app can use its forms. There is no credential in your code: requests go to this pod's gate
on loopback, which adds the app's identity and passes them to the module.

    from booth_streamlit import api

    api.insert("household", {"amount": "12.50", "memo": "groceries"})
    api.update("household", 42, {"memo": "groceries and wine"})
    api.update("budget", {"month": "2026-10", "category": "food"}, {"limit": "400"})  # composite key
    api.delete("household", 42)          # only if the owner allowed delete on both sides

``entered_by``: booth-api records the app as the writer, not the person. These helpers put the
signed-in viewer's subject (booth_streamlit.user()) in an ``entered_by`` column unless the row
already sets it; pass ``entered_by="other_column"`` to use another column, or ``entered_by=None``
to record nobody. That attribution is only as trustworthy as the app's code (ADR 0117 point 3).

Values use booth-api's formats: bigint and numeric as strings (Decimal is converted), ISO 8601
dates and times (date and datetime are converted), None for SQL NULL. One row per call, at most
64 KiB.

Errors: InvalidRow (booth-api's per-field problems, in ``.fields``), WriteRefused (anything else
booth-api or the module refused; ``.code`` is booth-api's error code, e.g. ``not_granted`` when an
owner hasn't granted the app on the API page), NotSignedIn, DataAccessPaused, DataAccessOff (the
app declares no write targets, or app writes are off in this install).
"""

from __future__ import annotations

import datetime as _dt
import decimal as _decimal
import json as _json
import os as _os
import time as _time
import urllib.error as _urlerr
import urllib.parse as _urlparse
import urllib.request as _urlreq
import uuid as _uuid
from dataclasses import dataclass, field
from typing import Any, Mapping, Optional, Union

from . import DataAccessOff, DataAccessPaused

__all__ = ["insert", "update", "delete", "Written", "WriteRefused", "InvalidRow", "NotSignedIn"]

_TIMEOUT = 35  # a little over the module's 30-second write timeout
_ATTEMPTS = 3  # one write, retried at most twice with the same idempotency key
_MAX_WAIT = 5.0  # longest Retry-After honoured between attempts

Key = Union[str, int, Mapping[str, Any]]


@dataclass(frozen=True)
class Written:
    """booth-api's answer: the row as stored (every field, as a read returns it) and its version,
    which ``expected_version`` takes to make an update or delete conditional."""

    row: dict
    version: str
    replayed: bool = False  # the same idempotency key was already applied; this is that answer


class WriteRefused(RuntimeError):
    """booth-api or the module refused the write. ``code`` is booth-api's error code."""

    def __init__(self, status: int, code: str, message: str, fields: Optional[list] = None, constraint: Optional[str] = None):
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message
        self.fields = fields or []
        self.constraint = constraint


class InvalidRow(WriteRefused):
    """The row doesn't fit the table: ``fields`` lists each problem as {"field", "problem", ...}."""


class NotSignedIn(RuntimeError):
    """No signed-in viewer to record in ``entered_by``; nothing was written."""


@dataclass
class _Attempt:
    status: int
    body: bytes
    headers: Mapping[str, str] = field(default_factory=dict)


def _base() -> str:
    url = _os.environ.get("BOOTH_WRITES_URL")
    if not url:
        raise DataAccessOff(
            "this app can't write: its owner hasn't declared any write targets (app settings, Write targets), "
            "or app writes are off in this Booth install"
        )
    return url.rstrip("/")


def _json_default(v: Any) -> Any:
    if isinstance(v, _decimal.Decimal):
        return str(v)
    if isinstance(v, (_dt.datetime, _dt.date, _dt.time)):
        return v.isoformat()
    raise TypeError(f"{type(v).__name__} is not a value booth-api accepts")


def _viewer() -> Optional[str]:
    from . import user  # looked up at call time, so tests can stand one in

    u = user()
    return u.subject if u else None


def _with_entered_by(data: Mapping[str, Any], entered_by: Optional[str], who: Optional[str]) -> dict:
    out = dict(data)
    if entered_by is None or entered_by in out:
        return out
    if who is None:
        raise NotSignedIn(f"no signed-in viewer to record in {entered_by!r}; pass entered_by=None to write without one")
    out[entered_by] = who
    return out


def _target(api: str, key: Optional[Key]) -> str:
    if not api or "/" in api:
        raise ValueError(f"{api!r} is not an API slug (as shown on the API page)")
    path = f"/{_urlparse.quote(api, safe='')}/rows"
    if key is None:
        return path
    if isinstance(key, Mapping):
        if not key:
            raise ValueError("a composite key needs at least one field")
        return path + "?" + _urlparse.urlencode({f"key[{k}]": str(v) for k, v in key.items()})
    s = str(key)
    if not s or "/" in s or s in (".", ".."):
        raise ValueError(f"key {s!r} can't go in a path; pass it as {{'<key field>': {s!r}}}")
    return path + "/" + _urlparse.quote(s, safe="")


def _send(method: str, path: str, body: Optional[dict], headers: dict) -> _Attempt:
    data = None if body is None else _json.dumps({"data": body}, default=_json_default).encode()
    req = _urlreq.Request(_base() + path, data=data, method=method, headers=headers)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with _urlreq.urlopen(req, timeout=_TIMEOUT) as resp:  # noqa: S310 - fixed loopback URL from the pod spec
            return _Attempt(resp.status, resp.read(), dict(resp.headers))
    except _urlerr.HTTPError as err:
        return _Attempt(err.code, err.read(64 << 10), dict(err.headers or {}))


def _retry_after(a: _Attempt, attempt: int) -> Optional[float]:
    """How long to wait before retrying, or None if this answer isn't worth retrying."""
    code = _error(a)[0] if a.status >= 400 else ""
    if a.status == 429 or a.status == 503 or (a.status == 409 and code == "idempotency_in_progress"):
        try:
            return min(float(a.headers.get("Retry-After", "")), _MAX_WAIT)
        except ValueError:
            return min(0.5 * 2**attempt, _MAX_WAIT)
    return None


def _error(a: _Attempt) -> tuple:
    try:
        payload = _json.loads(a.body)
    except ValueError:
        return "", a.body.decode(errors="replace") or f"HTTP {a.status}", None, None
    err = payload.get("error") if isinstance(payload, dict) else None
    if isinstance(err, dict):  # booth-api's shape, which the module's own refusals use too
        return err.get("code", ""), err.get("message", ""), err.get("fields"), err.get("constraint")
    if isinstance(err, str):  # the module's internal-port shape (data_access_paused, unauthorized)
        return err, payload.get("reason") or err, None, None
    return "", f"HTTP {a.status}", None, None


def _write(method: str, api: str, key: Optional[Key], body: Optional[dict], expected_version: Optional[str], who: Optional[str], idempotency_key: Optional[str]) -> Written:
    path = _target(api, key)
    headers = {"Idempotency-Key": idempotency_key or str(_uuid.uuid4())}
    if who:
        headers["X-Booth-App-User"] = who
    if expected_version:
        headers["If-Match"] = f'"{expected_version}"'
    for attempt in range(_ATTEMPTS):
        try:
            a = _send(method, path, body, headers)
        except OSError:
            if attempt == _ATTEMPTS - 1:
                raise
            _time.sleep(min(0.5 * 2**attempt, _MAX_WAIT))
            continue
        if a.status < 300:
            out = _json.loads(a.body)
            replayed = str(a.headers.get("Idempotent-Replayed", "")).lower() == "true"
            return Written(row=out.get("data") or {}, version=out.get("version", ""), replayed=replayed)
        wait = _retry_after(a, attempt)
        if wait is None or attempt == _ATTEMPTS - 1:
            break
        _time.sleep(wait)
    code, message, fields, constraint = _error(a)
    if a.status == 403 and code == "data_access_paused":
        raise DataAccessPaused(message)
    if a.status == 422 and fields:
        raise InvalidRow(a.status, code, message or "the row doesn't fit the table", fields, constraint)
    raise WriteRefused(a.status, code, message or f"booth-api answered {a.status}", fields, constraint)


def insert(api: str, row: Mapping[str, Any], *, entered_by: Optional[str] = "entered_by", idempotency_key: Optional[str] = None) -> Written:
    """Add one row to the API's table. Needs "insert" in the app's Write targets and the grant."""
    who = _viewer()
    return _write("POST", api, None, _with_entered_by(row, entered_by, who), None, who, idempotency_key)


def update(
    api: str,
    key: Key,
    changes: Mapping[str, Any],
    *,
    expected_version: Optional[str] = None,
    entered_by: Optional[str] = "entered_by",
    idempotency_key: Optional[str] = None,
) -> Written:
    """Change one row, found by its primary key (a value, or {field: value} for a composite key).
    With expected_version (a Written.version), only if the row hasn't changed since."""
    who = _viewer()
    return _write("PATCH", api, key, _with_entered_by(changes, entered_by, who), expected_version, who, idempotency_key)


def delete(api: str, key: Key, *, expected_version: Optional[str] = None, idempotency_key: Optional[str] = None) -> Written:
    """Delete one row; returns it as it was. Only when the owner allowed delete in the app's Write
    targets and on the API page (it is off by default on both)."""
    return _write("DELETE", api, key, None, expected_version, _viewer(), idempotency_key)
