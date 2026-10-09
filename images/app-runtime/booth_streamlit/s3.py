"""Read the workspace's lakehouse warehouse from an app (ADR 0107; ADR 0095's s3 credential sidecar).

When the workspace has a warehouse, the app's pod runs booth-core's credential sidecar in s3 mode.
It keeps two standard AWS files fresh: AWS_SHARED_CREDENTIALS_FILE (short-lived keys, read only,
renewed before they expire) and AWS_CONFIG_FILE (the object store's endpoint, region and addressing
style). Any boto3-based tool reads both unassisted. pyarrow and DuckDB read the keys but not the
endpoint (ADR 0095 Finding 3), so these helpers hand them the location:

    import booth_streamlit as b, pyarrow.parquet as pq
    fs = b.pyarrow_fs()
    root = b.warehouse_root()                    # "s3://bucket/prefix"
    table = pq.read_table(root[len("s3://"):] + "/sales/q1.parquet", filesystem=fs)

    import duckdb
    con = duckdb.connect()
    b.duckdb_secret(con)
    con.sql(f"SELECT * FROM '{root}/sales/q1.parquet'")

Only the location is passed explicitly; the keys stay with each engine's own AWS credential chain,
which reads the sidecar's file, so a renewal reaches them without any code here. The same helpers as
booth-notebooks' ``booth.s3``.

What the keys can do: read every object under the warehouse's prefix, and nothing else. Your code
can read the keys file (that is how the sidecar's s3 mode works); they expire with their lease.
"""

from __future__ import annotations

import configparser
import os
import time
import urllib.parse
from dataclasses import dataclass
from typing import Optional

from . import DataAccessOff, DataAccessPaused, data_status

__all__ = ["Location", "location", "warehouse_root", "pyarrow_fs", "duckdb_secret"]

NOT_ENABLED = (
    "this app has no lakehouse access: this workspace had no lakehouse warehouse when the app "
    "started, or the module's dataAccess.lakehouse is off. A warehouse created while the app runs "
    "reaches it at its next start."
)


@dataclass(frozen=True)
class Location:
    """Where the warehouse's object store is. ``endpoint_url`` is None for real AWS S3."""

    endpoint_url: Optional[str]
    region: Optional[str]
    addressing_style: Optional[str]  # "path" | "virtual" | None (not stated: the engine's default)

    @property
    def scheme(self) -> Optional[str]:
        return urllib.parse.urlsplit(self.endpoint_url).scheme if self.endpoint_url else None

    @property
    def host(self) -> Optional[str]:
        """``host[:port]``, the form pyarrow and DuckDB take."""
        return urllib.parse.urlsplit(self.endpoint_url).netloc if self.endpoint_url else None


def warehouse_root(env=None) -> str:
    """The warehouse's location, ``s3://bucket/prefix``."""
    env = os.environ if env is None else env
    root = env.get("BOOTH_WAREHOUSE_ROOT", "")
    if not root:
        raise DataAccessOff(NOT_ENABLED)
    return root.rstrip("/")


def _nested(value: str) -> dict:
    """botocore's nested form: ``s3 =\\n    addressing_style = path``."""
    out = {}
    for line in value.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip()
    return out


def _wait_for_lease(cred_path: str, wait: float) -> None:
    deadline = time.monotonic() + wait
    while not os.path.exists(cred_path):
        status = data_status()
        if status.get("state") == "paused":
            raise DataAccessPaused(status.get("reason") or "data access is paused")
        if time.monotonic() >= deadline:
            raise RuntimeError(
                "lakehouse credentials aren't ready: the credential sidecar hasn't written its first "
                "lease. If this persists, the platform refused the app a lease."
            )
        time.sleep(0.5)


def location(env=None, wait: float = 30.0) -> Location:
    """Read the sidecar's config file, waiting up to ``wait`` seconds for its first lease. Raises
    DataAccessOff when the app has no s3 sidecar and DataAccessPaused when its owner lost access."""
    env = os.environ if env is None else env
    conf_path, cred_path = env.get("AWS_CONFIG_FILE", ""), env.get("AWS_SHARED_CREDENTIALS_FILE", "")
    if not conf_path or not cred_path:
        raise DataAccessOff(NOT_ENABLED)
    _wait_for_lease(cred_path, wait)
    parser = configparser.ConfigParser(interpolation=None)
    try:
        parser.read(conf_path, encoding="utf-8")
    except configparser.Error as e:
        raise RuntimeError(f"the credential sidecar's config file is unreadable: {e.__class__.__name__}") from None
    profile = env.get("AWS_PROFILE", "") or "default"
    section = profile if profile == "default" else f"profile {profile}"
    if not parser.has_section(section):
        return Location(None, None, None)  # real AWS S3: the sidecar writes no config at all
    sec = parser[section]
    style = sec.get("addressing_style") or _nested(sec.get("s3", "")).get("addressing_style")
    return Location(sec.get("endpoint_url") or None, sec.get("region") or None, style or None)


def pyarrow_fs(**kwargs):
    """A ``pyarrow.fs.S3FileSystem`` pointed at the warehouse's object store. Keys come from the
    sidecar's file through the AWS SDK's own chain. Extra keyword arguments pass through (and win).
    Paths on it are ``bucket/key``: ``warehouse_root()`` without its ``s3://``."""
    from pyarrow import fs

    loc = location()
    opts = {}
    if loc.region:
        opts["region"] = loc.region
    if loc.endpoint_url:
        opts["endpoint_override"] = loc.host
        opts["scheme"] = loc.scheme
    if loc.addressing_style == "virtual":
        opts["force_virtual_addressing"] = True  # pyarrow's default with an endpoint override is path style
    opts.update(kwargs)
    return fs.S3FileSystem(**opts)


def _sql_str(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _load_extensions(con) -> None:
    """httpfs and aws: the image's own copies when there are some (BOOTH_DUCKDB_EXTENSIONS, no
    download), else DuckDB's usual INSTALL, which downloads them and needs internet egress."""
    ext = os.environ.get("BOOTH_DUCKDB_EXTENSIONS", "")
    if ext and os.path.isdir(ext):
        con.execute(f"SET extension_directory = {_sql_str(ext)}")
        con.execute("LOAD httpfs; LOAD aws;")
    else:
        con.execute("INSTALL httpfs; LOAD httpfs; INSTALL aws; LOAD aws;")


def duckdb_secret(con, name: str = "booth_s3") -> None:
    """Create (or replace) a DuckDB S3 secret on ``con`` for the warehouse's object store, after
    loading ``httpfs`` and ``aws`` (the image's copies; this sets the connection's
    extension_directory to them). The secret uses DuckDB's AWS credential chain with ``REFRESH
    auto``, so renewed keys are picked up; only the location is set here, and no key ever appears
    in SQL text."""
    if not name.replace("_", "").isalnum():
        raise ValueError("secret name must be letters, digits and underscores")
    loc = location()
    _load_extensions(con)
    parts = ["TYPE s3", "PROVIDER credential_chain", "REFRESH auto"]
    if loc.region:
        parts.append(f"REGION {_sql_str(loc.region)}")
    if loc.endpoint_url:
        parts.append(f"ENDPOINT {_sql_str(loc.host)}")
        parts.append(f"USE_SSL {'true' if loc.scheme == 'https' else 'false'}")
    if loc.addressing_style in ("path", "virtual"):
        parts.append(f"URL_STYLE {_sql_str('path' if loc.addressing_style == 'path' else 'vhost')}")
    con.execute(f"CREATE OR REPLACE SECRET {name} ({', '.join(parts)})")
