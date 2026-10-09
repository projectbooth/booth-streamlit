import pytest

import booth_streamlit as bs
from booth_streamlit import s3

MINIO_CONF = "[default]\nendpoint_url = http://minio.booth-minio.svc:9000\nregion = us-east-1\ns3 =\n    addressing_style = path\n"


@pytest.fixture()
def files(tmp_path, monkeypatch):
    """The sidecar's two files, as booth-core's credential sidecar (8f0c6b4) writes them, plus the env
    the lifecycle sets in the Streamlit container."""
    cred, conf = tmp_path / "credentials", tmp_path / "credentials.config"
    cred.write_text("[default]\naws_access_key_id = AK\naws_secret_access_key = SK\n")
    conf.write_text(MINIO_CONF)
    monkeypatch.setenv("AWS_SHARED_CREDENTIALS_FILE", str(cred))
    monkeypatch.setenv("AWS_CONFIG_FILE", str(conf))
    monkeypatch.setenv("BOOTH_WAREHOUSE_ROOT", "s3://lake/acme-data/")
    monkeypatch.delenv("AWS_PROFILE", raising=False)
    return cred, conf


def test_location_reads_the_nested_botocore_form(files):
    loc = s3.location()
    assert loc == s3.Location("http://minio.booth-minio.svc:9000", "us-east-1", "path")
    assert (loc.host, loc.scheme) == ("minio.booth-minio.svc:9000", "http")


def test_location_flat_style_and_real_aws(files):
    _, conf = files
    conf.write_text("[default]\nendpoint_url = https://s3.example\naddressing_style = virtual\n")
    assert s3.location().addressing_style == "virtual"
    conf.unlink()  # real AWS: the sidecar writes no config file at all
    assert s3.location() == s3.Location(None, None, None)


def test_no_sidecar_is_data_access_off(monkeypatch):
    for k in ("AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "BOOTH_WAREHOUSE_ROOT"):
        monkeypatch.delenv(k, raising=False)
    with pytest.raises(bs.DataAccessOff, match="no lakehouse access"):
        s3.location()
    with pytest.raises(bs.DataAccessOff):
        bs.warehouse_root()


def test_waiting_for_the_first_lease_reports_a_pause(files, monkeypatch):
    cred, _ = files
    cred.unlink()
    monkeypatch.setattr(s3, "data_status", lambda: {"state": "paused", "reason": "owner gone"})
    with pytest.raises(bs.DataAccessPaused, match="owner gone"):
        s3.location()


def test_no_lease_in_time_is_a_clear_error(files, monkeypatch):
    cred, _ = files
    cred.unlink()
    monkeypatch.setattr(s3, "data_status", lambda: {"state": "ok"})
    with pytest.raises(RuntimeError, match="hasn't written its first lease"):
        s3.location(wait=0.6)


def test_warehouse_root(files):
    assert bs.warehouse_root() == "s3://lake/acme-data"


def test_pyarrow_fs_gets_the_endpoint_and_never_the_keys(files, monkeypatch):
    from pyarrow import fs

    seen = {}
    real = fs.S3FileSystem
    monkeypatch.setattr(fs, "S3FileSystem", lambda **kw: seen.update(kw) or real(**kw))
    got = bs.pyarrow_fs()
    assert isinstance(got, real)  # the real pyarrow in this image accepts the options
    assert seen == {"region": "us-east-1", "endpoint_override": "minio.booth-minio.svc:9000", "scheme": "http"}


class FakeDuck:
    def __init__(self):
        self.sql = []

    def execute(self, q):
        self.sql.append(q)


def test_duckdb_secret_sets_the_location_only(files, monkeypatch):
    monkeypatch.delenv("BOOTH_DUCKDB_EXTENSIONS", raising=False)  # no image copy: DuckDB's own INSTALL
    con = FakeDuck()
    bs.duckdb_secret(con)
    assert con.sql[0] == "INSTALL httpfs; LOAD httpfs; INSTALL aws; LOAD aws;"
    assert con.sql[1] == (
        "CREATE OR REPLACE SECRET booth_s3 (TYPE s3, PROVIDER credential_chain, REFRESH auto, "
        "REGION 'us-east-1', ENDPOINT 'minio.booth-minio.svc:9000', USE_SSL false, URL_STYLE 'path')"
    )
    assert "SK" not in " ".join(con.sql) and "AK" not in " ".join(con.sql)
    with pytest.raises(ValueError):
        bs.duckdb_secret(con, name="x; DROP")


def test_duckdb_secret_loads_the_images_extensions(files, tmp_path, monkeypatch):
    monkeypatch.setenv("BOOTH_DUCKDB_EXTENSIONS", str(tmp_path))
    con = FakeDuck()
    bs.duckdb_secret(con)
    assert con.sql[:2] == [f"SET extension_directory = '{tmp_path}'", "LOAD httpfs; LOAD aws;"]


def test_duckdb_secret_with_real_duckdb(files):
    """Real DuckDB, as the runtime image ships it (its extensions preinstalled, no download): the
    secret exists with the warehouse's endpoint and path style, and no key in it."""
    duckdb = pytest.importorskip("duckdb")
    import os

    if not os.path.isdir(os.environ.get("BOOTH_DUCKDB_EXTENSIONS", "")):
        pytest.skip("not in the runtime image (no preinstalled DuckDB extensions)")
    con = duckdb.connect()
    bs.duckdb_secret(con)
    name, typ, provider, secret = con.execute(
        "SELECT name, type, provider, secret_string FROM duckdb_secrets() WHERE name = 'booth_s3'"
    ).fetchone()
    assert (name, typ, provider) == ("booth_s3", "s3", "credential_chain")
    assert "endpoint=minio.booth-minio.svc:9000" in secret and "url_style=path" in secret and "use_ssl=false" in secret
    # DuckDB 1.5.6 shows the key id and redacts the secret; the keys came from the file, not from us.
    assert "secret=redacted" in secret.split(";") and "SK" not in secret
