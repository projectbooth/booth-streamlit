"""Install an app's requirements.txt: the ``pip`` init container of its pod (ADR 0107;
docs/design-data-access.md item 5).

    python -m booth_streamlit.pip_install

Runs as the app's uid with a read-only root filesystem. It mounts only the app's source (read only,
for ``requirements.txt``) and the empty ``site`` volume it installs into: no token, no bearer, no
credentials, which is what makes running unvetted packages' install steps here acceptable. The
Streamlit container then mounts ``site`` read only, on PYTHONPATH.

Packages are installed afresh on every pod start; nothing persists.

On failure the owner's message is the container's termination message (``/dev/termination-log``),
which the module reads from the pod status: the tail of pip's output, or, when the app's egress is
closed and pip couldn't reach the index, why. A whole-install deadline (BOOTH_PIP_DEADLINE_SECONDS)
stops a pip that hangs, which ``--timeout`` alone doesn't: that is per network read, and a server
that trickles bytes never trips it.

Environment (set by the module's lifecycle):
    BOOTH_PIP_REQUIREMENTS      the requirements file
    BOOTH_PIP_TARGET            where packages go (the site volume)
    BOOTH_PIP_DEADLINE_SECONDS  the whole install's limit
    BOOTH_PIP_EGRESS_CLOSED     "true" when apps.egress.mode is closed and no package index is reachable
    PIP_INDEX_URL               the operator's package index, if not PyPI (read by pip itself)
    PIP_TRUSTED_HOST            that index's host, when it is plain HTTP (read by pip itself)
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import time

TERMINATION_LOG = "/dev/termination-log"
# Kubernetes keeps at most 4096 bytes of a termination message; leave room for the explanation.
TAIL_BYTES = 2500
TAIL_LINES = 20
PIP_TIMEOUT = "20"  # seconds per network read (pip's own --timeout)
PIP_RETRIES = "2"

CLOSED = (
    "pip install needs internet access, and apps.egress.mode is closed: this app's pod can't reach "
    "a package index. Ask the operator for an index the apps may reach (apps.pip.indexUrl and "
    "apps.pip.egress), or remove the requirements."
)
# What pip prints when it can't connect at all (as opposed to a bad requirement).
NETWORK_MARKERS = (
    "NewConnectionError",
    "ConnectTimeoutError",
    "Failed to establish a new connection",
    "Max retries exceeded",
    "Network is unreachable",
    "Temporary failure in name resolution",
    "Name or service not known",
    "Connection timed out",
    "Read timed out",
)


def tail(output: str) -> str:
    lines = [ln for ln in output.splitlines() if ln.strip()]
    text = "\n".join(lines[-TAIL_LINES:])
    return text[-TAIL_BYTES:]


def message(rc: int | None, output: str, deadline: float, egress_closed: bool) -> str:
    """The owner's explanation of a failed install. rc None means the deadline stopped pip."""
    last = tail(output) or "(pip printed nothing)"
    stopped = f"pip install did not finish within {int(deadline)}s (the module's apps.pip.timeout) and was stopped."
    # With closed egress a blocked connection can also run into the deadline while pip retries.
    if egress_closed and any(m in output for m in NETWORK_MARKERS):
        return f"{CLOSED}\n{stopped if rc is None else ''}\nLast output:\n{last}".replace("\n\n", "\n")
    if rc is None:
        return f"{stopped} Last output:\n{last}"
    return f"pip install failed (exit {rc}):\n{last}"


def mounts() -> list[str]:
    """Mount points other than the system's own, for the log: what this container could read."""
    out = []
    try:
        with open("/proc/self/mounts") as f:
            for line in f:
                point = line.split()[1]
                if point != "/" and not point.startswith(("/proc", "/sys", "/dev")):
                    out.append(point)
    except OSError:
        pass
    return sorted(set(out))


def clear(target: str) -> None:
    """Empty the target: a restarted init container finds the previous attempt's files there."""
    for name in os.listdir(target):
        path = os.path.join(target, name)
        if os.path.isdir(path) and not os.path.islink(path):
            shutil.rmtree(path, ignore_errors=True)
        else:
            try:
                os.remove(path)
            except OSError:
                pass


def run(env=None, termination_log: str = TERMINATION_LOG) -> int:
    env = dict(os.environ if env is None else env)
    req = env.get("BOOTH_PIP_REQUIREMENTS", "/app/requirements.txt")
    target = env.get("BOOTH_PIP_TARGET", "/opt/booth/site")
    deadline = float(env.get("BOOTH_PIP_DEADLINE_SECONDS", "300"))
    egress_closed = env.get("BOOTH_PIP_EGRESS_CLOSED") == "true"

    print("booth pip install: mounts: " + " ".join(mounts()), flush=True)
    clear(target)
    work = os.path.join(target, ".pip-work")  # pip's HOME and TMPDIR: the root filesystem is read only
    os.makedirs(work, exist_ok=True)
    env.update(HOME=work, TMPDIR=work, PIP_NO_CACHE_DIR="1", PIP_DISABLE_PIP_VERSION_CHECK="1", PYTHONDONTWRITEBYTECODE="1")
    cmd = [
        sys.executable, "-m", "pip", "install", "--no-cache-dir", "--no-input", "--progress-bar", "off",
        "--target", target, "--timeout", PIP_TIMEOUT, "--retries", PIP_RETRIES, "-r", req,
    ]
    started = time.monotonic()
    proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace")
    try:
        output, _ = proc.communicate(timeout=deadline)
        rc: int | None = proc.returncode
    except subprocess.TimeoutExpired:
        proc.kill()
        output, _ = proc.communicate()
        rc = None
    shutil.rmtree(work, ignore_errors=True)
    sys.stdout.write(output)
    took = time.monotonic() - started
    if rc == 0:
        print(f"booth pip install: done in {took:.1f}s", flush=True)
        return 0
    msg = message(rc, output, deadline, egress_closed)
    print(f"booth pip install: failed after {took:.1f}s", flush=True)
    try:
        with open(termination_log, "w") as f:
            f.write(msg)
    except OSError as e:
        print(f"booth pip install: could not write the termination message: {e}", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(run())
