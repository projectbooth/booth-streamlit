"""A tiny PEP 503 package index for the pip checks (pip-access.sh), run in the cluster so CI never
depends on public PyPI. Standard library only; it runs from the app runtime image.

    python pypi_index.py [port]

Serves, under /simple/:
  booth-it-hello   one pure-Python wheel (booth_it_hello 1.0.0), built in memory at startup
  booth-it-slow    a project page that never finishes: one byte every few seconds, forever. pip's
                   own --timeout is per read, so this never trips it; only the module's
                   whole-install deadline (apps.pip.timeout) stops it.
Anything else is 404, which pip reports as "No matching distribution".
"""

import base64
import hashlib
import http.server
import io
import sys
import time
import zipfile

HELLO_SRC = 'GREETING = "hello from the booth test index"\n'


def wheel(name: str, version: str, module_src: str) -> tuple[str, bytes]:
    dist = name.replace("-", "_")
    info = f"{dist}-{version}.dist-info"
    files = {
        f"{dist}/__init__.py": module_src,
        f"{info}/METADATA": f"Metadata-Version: 2.1\nName: {name}\nVersion: {version}\n",
        f"{info}/WHEEL": "Wheel-Version: 1.0\nGenerator: booth-it\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
    record = []
    for path, text in files.items():
        data = text.encode()
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
        record.append(f"{path},sha256={digest},{len(data)}")
    record.append(f"{info}/RECORD,,")
    files[f"{info}/RECORD"] = "\n".join(record) + "\n"
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        for path, text in files.items():
            z.writestr(zipfile.ZipInfo(path, (2026, 1, 1, 0, 0, 0)), text)
    return f"{dist}-{version}-py3-none-any.whl", buf.getvalue()


HELLO_FILE, HELLO_WHEEL = wheel("booth-it-hello", "1.0.0", HELLO_SRC)
HELLO_SHA = hashlib.sha256(HELLO_WHEEL).hexdigest()


class Index(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def do_GET(self):  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path == "/simple/":
            return self.send(200, b'<a href="booth-it-hello/">booth-it-hello</a>\n<a href="booth-it-slow/">booth-it-slow</a>\n')
        if path == "/simple/booth-it-hello/":
            link = f'<a href="/packages/{HELLO_FILE}#sha256={HELLO_SHA}">{HELLO_FILE}</a>\n'
            return self.send(200, link.encode())
        if path == f"/packages/{HELLO_FILE}":
            return self.send(200, HELLO_WHEEL, "application/octet-stream")
        if path == "/simple/booth-it-slow/":
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            try:
                while True:
                    self.wfile.write(b" ")
                    self.wfile.flush()
                    time.sleep(5)
            except OSError:
                return
        return self.send(404, b"not found\n")

    def send(self, code, body, ctype="text/html"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("pypi-index: " + fmt % args, flush=True)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    print(f"pypi-index: serving {HELLO_FILE} (sha256 {HELLO_SHA}) on :{port}", flush=True)
    http.server.ThreadingHTTPServer(("", port), Index).serve_forever()
