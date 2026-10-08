# syntax=docker/dockerfile:1
# The per-app runtime: Streamlit plus the booth_streamlit helper. One container of this image runs
# one user's app (design note (a)); the module backend never runs Streamlit itself.
#
# Step 1 scope: enough to prove the URL and websocket path through core's iframe proxy. The data
# libraries, the gate container and per-app pip installs (ADR 0104) arrive with the lifecycle.
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

COPY images/app-runtime/requirements.txt /opt/booth/requirements.txt
RUN pip install -r /opt/booth/requirements.txt
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
