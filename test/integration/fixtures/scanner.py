"""Runs as user code in an app's Streamlit container (kubectl exec, same filesystem, environment,
uid and process namespace an app's own code has) and looks for anything that would let user code act
as the app's owner (ADR 0107; docs/design-data-access.md item 2):

  - any workload JWT from booth-core (iss = the workload issuer, or sub = streamlit:...), anywhere it
    can read: every file, every /proc/<pid>/environ and cmdline, every HTTP answer from every port
    listening on this pod's loopback;
  - the app's gate bearer (compared by sha256: the harness passes the hash, never the bearer);
  - a Kubernetes service-account token.

Control: before scanning it plants a decoy workload-shaped JWT in /tmp, and must find it. A scanner
that finds nothing because it is broken fails the test instead of passing it.

    python - <workload issuer> <sha256 of the app's bearer> < scanner.py
Prints one JSON object.
"""

import base64
import hashlib
import json
import os
import re
import socket
import sys

ISSUER, BEARER_SHA = sys.argv[1], sys.argv[2]
JWT = re.compile(rb"eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*")
HEX64 = re.compile(rb"(?<![0-9a-f])[0-9a-f]{64}(?![0-9a-f])")
DECOY = "/tmp/booth-scanner-decoy"
SKIP = ("/proc", "/sys", "/dev")


def b64(d):
    return base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()


with open(DECOY, "w") as f:
    f.write(b64({"alg": "none"}) + "." + b64({"iss": ISSUER, "sub": "streamlit:decoy:scanner"}) + ".x")

found = {"decoy": False, "workload_jwt": [], "bearer": [], "sa_token": False}
counts = {"files": 0, "proc_entries": 0, "ports": []}


def inspect(data, where):
    for m in JWT.findall(data):
        try:
            payload = m.split(b".")[1]
            claims = json.loads(base64.urlsafe_b64decode(payload + b"=" * (-len(payload) % 4)))
        except Exception:
            continue
        if claims.get("iss") == ISSUER or str(claims.get("sub", "")).startswith("streamlit:"):
            if where == DECOY:
                found["decoy"] = True
            else:
                found["workload_jwt"].append(where)
    for m in HEX64.findall(data):
        if hashlib.sha256(m).hexdigest() == BEARER_SHA:
            found["bearer"].append(where)


for root, dirs, files in os.walk("/", followlinks=False):
    if root.startswith(SKIP):
        dirs[:] = []
        continue
    for name in files:
        path = os.path.join(root, name)
        try:
            if os.path.islink(path) or not os.path.isfile(path):
                continue
            with open(path, "rb") as f:
                data = f.read(4 << 20)
        except OSError:
            continue
        counts["files"] += 1
        inspect(data, path)

for pid in os.listdir("/proc"):
    if not pid.isdigit():
        continue
    for leaf in ("environ", "cmdline"):
        try:
            with open(f"/proc/{pid}/{leaf}", "rb") as f:
                inspect(f.read(), f"/proc/{pid}/{leaf}")
            counts["proc_entries"] += 1
        except OSError:
            pass

found["sa_token"] = os.path.exists("/var/run/secrets/kubernetes.io/serviceaccount/token")

listening = set()
for table in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        with open(table) as f:
            next(f)
            for line in f:
                cols = line.split()
                if cols[3] == "0A":  # LISTEN
                    listening.add(int(cols[1].split(":")[1], 16))
    except OSError:
        pass
for port in sorted(listening):
    counts["ports"].append(port)
    for path in ("/", "/healthz", "/_booth/data/status", "/internal/token", "/token"):
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=2) as s:
                s.sendall(f"GET {path} HTTP/1.0\r\nHost: localhost\r\n\r\n".encode())
                s.settimeout(2)
                chunks = []
                while True:
                    c = s.recv(65536)
                    if not c:
                        break
                    chunks.append(c)
                    if sum(map(len, chunks)) > 1 << 20:
                        break
                inspect(b"".join(chunks), f"127.0.0.1:{port}{path}")
        except OSError:
            pass

os.remove(DECOY)
print(json.dumps({"found": found, "counts": counts}))
