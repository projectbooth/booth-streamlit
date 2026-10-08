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
