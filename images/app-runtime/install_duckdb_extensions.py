"""Install DuckDB's httpfs and aws extensions into the image, pinned by sha256 (image build only).

    python install_duckdb_extensions.py /opt/booth/duckdb

The files come from DuckDB's extension repository at the URL for the pinned DuckDB version and
this machine's platform. Each download must match the sha256 recorded here, or the build fails: a
changed upstream file is a decision to make, not something to pick up silently. DuckDB then checks
each extension's own signature when it installs and loads it.

To bump DuckDB: change requirements.txt, then record the new version's hashes here (the build's
error message prints what it got).
"""

import gzip
import hashlib
import os
import platform
import sys
import tempfile
import urllib.request

import duckdb

VERSION = "v1.5.6"
# sha256 of the .duckdb_extension.gz files, fetched 2026-10-09.
PINNED = {
    "linux_amd64": {
        "httpfs": "19e6906934a845487c96f9c94beee250c71e32bb9be260eb27ad96939a1df5f0",
        "aws": "9d9bbd40e131f5e1314d6a7a82584b680f64a38fe771d4048275d48aa405852e",
    },
    "linux_arm64": {
        "httpfs": "b18472a0e85cbf15ca4ebbe335173f87b62a074cd0f26072d4c756205c6239d0",
        "aws": "7be5627a03495959bc033755fe4184db6cbc1d391c16790d3dad06fc14c79606",
    },
}


def main(target: str) -> None:
    if duckdb.__version__ != VERSION.lstrip("v"):
        sys.exit(f"DuckDB is {duckdb.__version__}, but the pinned extensions are for {VERSION}: record the new hashes")
    plat = {"x86_64": "linux_amd64", "aarch64": "linux_arm64"}.get(platform.machine())
    if plat not in PINNED:
        sys.exit(f"no pinned DuckDB extensions for {platform.machine()}")
    con = duckdb.connect()
    con.execute(f"SET extension_directory = '{target}'")
    with tempfile.TemporaryDirectory() as tmp:
        for name, want in PINNED[plat].items():
            url = f"https://extensions.duckdb.org/{VERSION}/{plat}/{name}.duckdb_extension.gz"
            # extensions.duckdb.org answers Python's default User-Agent with 403 (CI run 37976978455).
            req = urllib.request.Request(url, headers={"User-Agent": "booth-streamlit-image-build (+https://github.com/projectbooth/booth-streamlit)"})
            with urllib.request.urlopen(req, timeout=120) as resp:  # noqa: S310 - fixed https URL
                data = resp.read()
            got = hashlib.sha256(data).hexdigest()
            if got != want:
                sys.exit(f"{url}: sha256 {got}, pinned {want}")
            path = os.path.join(tmp, f"{name}.duckdb_extension")
            with open(path, "wb") as f:
                f.write(gzip.decompress(data))
            con.execute(f"INSTALL '{path}'")
            con.execute(f"LOAD {name}")
            print(f"installed {name} {VERSION} {plat} (sha256 {got})")


if __name__ == "__main__":
    main(sys.argv[1])
