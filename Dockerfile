# syntax=docker/dockerfile:1
# The module backend image: the backend, the built UI it serves, and the per-app gate binary.
# Streamlit is not in this image; it runs only in the per-app containers (images/app-runtime).

FROM node:20-bookworm-slim AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-streamlit ./cmd/streamlit
# The gate that runs in every app pod in front of Streamlit (internal/gate). It ships in this image
# so the app pod's only code besides Streamlit is the module's own, pinned to the same version.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-streamlit-gate ./cmd/gate

# Distroless static + nonroot (uid/gid 65532): no shell, no package manager; the chart pins the
# same uid.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/booth-streamlit /booth-streamlit
COPY --from=build /out/booth-streamlit-gate /booth-streamlit-gate
COPY --from=web /web/dist /web
ENV BOOTH_WEB_DIR=/web
USER nonroot:nonroot
ENTRYPOINT ["/booth-streamlit"]
