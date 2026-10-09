"""Helpers for Streamlit apps running in Booth.

``user()`` returns who is viewing the app. The module's proxy verifies booth-core's signed identity
assertion before any request reaches the app, then sets plain headers from the verified result
(design note (b)). Streamlit exposes the headers of the request that opened the session, so the
identity is fixed for the life of one viewer's browser tab.

There is no login inside an app, and no way to get one: identity comes only from Booth.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping, Optional

__all__ = ["User", "user", "user_from_headers"]

HEADER_USER = "X-Booth-User"
HEADER_WORKSPACE = "X-Booth-Workspace"
HEADER_ROLE = "X-Booth-Role"


@dataclass(frozen=True)
class User:
    """The viewer: their identity-provider subject, and their role in the app's workspace."""

    subject: str
    workspace: str
    role: str  # "viewer", "editor" or "owner"


def user_from_headers(headers: Mapping[str, str]) -> Optional[User]:
    """Builds a User from request headers, or None if they don't carry one.

    Header names are matched case-insensitively, as HTTP requires.
    """
    lower = {k.lower(): v for k, v in headers.items()}
    subject = lower.get(HEADER_USER.lower(), "")
    workspace = lower.get(HEADER_WORKSPACE.lower(), "")
    role = lower.get(HEADER_ROLE.lower(), "")
    if not subject or not workspace or role not in ("viewer", "editor", "owner"):
        return None
    return User(subject=subject, workspace=workspace, role=role)


def user() -> Optional[User]:
    """The current viewer, or None when the app runs outside Booth (e.g. ``streamlit run`` locally)."""
    import streamlit as st  # imported here so user_from_headers is testable without Streamlit

    return user_from_headers(dict(st.context.headers))


# --- Data access (ADR 0104/0107) --------------------------------------------------------------
#
# An app reads data as its owner, capped at viewer. The module wires it in for you: DATABASE_URL
# points at a credential proxy on this pod's loopback, so any Postgres client works with no password
# in your code. These helpers only add a clear answer when it can't work.

import json as _json
import os as _os
import urllib.request as _urlreq

__all__ += ["DataAccessPaused", "DataAccessOff", "data_status", "database_url"]


class DataAccessPaused(RuntimeError):
    """The app's owner can't currently lend it their access: they lost their role in this workspace,
    or haven't signed in for 7 days. The app runs; its data doesn't, until they sign in again or a
    workspace owner takes ownership of the app."""


class DataAccessOff(RuntimeError):
    """This Booth install doesn't give apps data access (the module's dataAccess.enabled is off)."""


def data_status(timeout: float = 2.0) -> dict:
    """{"state": "ok" | "paused" | "waiting", "reason": ...} from this pod's gate (loopback only)."""
    url = _os.environ.get("BOOTH_DATA_STATUS_URL")
    if not url:
        return {"state": "off"}
    try:
        with _urlreq.urlopen(url, timeout=timeout) as resp:  # noqa: S310 - fixed loopback URL from the pod spec
            return _json.load(resp)
    except OSError as exc:
        return {"state": "unknown", "reason": str(exc)}


def database_url() -> str:
    """DATABASE_URL, or a clear exception saying why there is none or why it won't work right now."""
    url = _os.environ.get("DATABASE_URL")
    if not url:
        raise DataAccessOff("this app has no database access (booth-database or the module's data access is not enabled)")
    status = data_status()
    if status.get("state") == "paused":
        raise DataAccessPaused(status.get("reason") or "data access is paused")
    return url


# The lakehouse warehouse (booth_streamlit/s3.py): imported last, it uses the names above.
from .s3 import duckdb_secret, pyarrow_fs, warehouse_root  # noqa: E402

__all__ += ["duckdb_secret", "pyarrow_fs", "warehouse_root"]
