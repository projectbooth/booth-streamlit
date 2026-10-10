"""A TCP connect probe for egress.sh, run as user code in an app's Streamlit container (and, as the
positive control, in an unrestricted pod).

    python netprobe.py '{"targets": {"name": ["host", port], ...}, "dns": "example.com", "timeout": 5}'

Prints one JSON object, name -> result:
  "open"                          the connection was accepted
  "closed:TimeoutError"           no answer at all: the packets were dropped (what a NetworkPolicy
                                  denial looks like under Calico)
  "closed:ConnectionRefusedError" something answered with a reset: nothing listening, or a reject.
                                  Never counted as "blocked": it only says nothing accepted.
  "closed:<other>"                anything else (unreachable network, DNS failure, ...)
and "dns" -> the resolved address, or "failed:<error>".
"""

import json
import socket
import sys

spec = json.loads(sys.argv[1])
timeout = float(spec.get("timeout", 5))
out = {}
for name, (host, port) in spec.get("targets", {}).items():
    try:
        with socket.create_connection((host, int(port)), timeout=timeout):
            out[name] = "open"
    except TimeoutError:
        out[name] = "closed:TimeoutError"
    except OSError as e:
        out[name] = "closed:" + type(e).__name__
if spec.get("dns"):
    try:
        out["dns"] = socket.gethostbyname(spec["dns"])
    except OSError as e:
        out["dns"] = "failed:" + type(e).__name__
print(json.dumps(out))
