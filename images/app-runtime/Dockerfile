# syntax=docker/dockerfile:1
# The per-app runtime: Streamlit plus the booth_streamlit helper. One container of this image runs
# one user's app (design note (a)); the module backend never runs Streamlit itself.
#
# Streamlit binds 127.0.0.1 in the pod (STREAMLIT_SERVER_ADDRESS, set by the lifecycle); the gate
# container, which runs from the backend's image, is the pod's only way in. Per-app packages (an
# app's requirements.txt) are installed by an init container from this same image
# (booth_streamlit.pip_install) into a volume on PYTHONPATH.
FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    # Streamlit writes ~/.streamlit; the root filesystem is read-only in the pod, /tmp is not.
    HOME=/tmp \
    STREAMLIT_SERVER_HEADLESS=true \
    STREAMLIT_BROWSER_GATHER_USAGE_STATS=false \
    STREAMLIT_SERVER_PORT=8501 \
    # No file watcher: code changes arrive as a new pod, never by editing files in place.
    STREAMLIT_SERVER_FILE_WATCHER_TYPE=none

# A real passwd entry for the uid apps run as. libpq (psycopg, psql) asks the OS for the current
# user's name when DATABASE_URL names no user, and fails with "local user with ID 65532 does not
# exist" without one; the integration test found this. The credential sidecar connects as whatever
# its lease names, so the name itself doesn't matter.
RUN groupadd --gid 65532 booth && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin booth

COPY images/app-runtime/requirements.txt /opt/booth/requirements.txt
RUN pip install -r /opt/booth/requirements.txt
# DuckDB's httpfs and aws extensions, matching the pinned DuckDB, into a directory that is part of
# the image (HOME is /tmp at runtime, an empty volume). booth_streamlit.duckdb_secret() loads them
# from here, so reading the lakehouse needs no download at runtime. They come from DuckDB's
# extension repository at build time, pinned by sha256 (install_duckdb_extensions.py).
ENV BOOTH_DUCKDB_EXTENSIONS=/opt/booth/duckdb
COPY images/app-runtime/install_duckdb_extensions.py /tmp/install_duckdb_extensions.py
RUN python /tmp/install_duckdb_extensions.py /opt/booth/duckdb \
 && rm /tmp/install_duckdb_extensions.py \
 && chmod -R a+rX /opt/booth/duckdb
COPY images/app-runtime/booth_streamlit /opt/booth/lib/booth_streamlit
ENV PYTHONPATH=/opt/booth/lib

# No [auth] section anywhere in the image's Streamlit config, so st.login has no provider: identity
# comes only from the headers the module's proxy sets (design note (b)).
USER 65532:65532
WORKDIR /app
EXPOSE 8501
# The app's source is mounted at /app/app.py; STREAMLIT_SERVER_BASE_URL_PATH (apps/<id>) is set
# per app by whoever starts the container.
ENTRYPOINT ["streamlit", "run", "/app/app.py"]
