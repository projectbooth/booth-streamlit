import subprocess

import pytest

from booth_streamlit import pip_install as pi


class FakePip:
    """Stands in for the pip process: records the command and env, answers with output and an exit
    code, or hangs (raises TimeoutExpired, as communicate() does at the deadline)."""

    def __init__(self, rc=0, output="", hang=False):
        self.rc, self.output, self.hang = rc, output, hang
        self.cmd = self.env = None
        self.killed = False

    def __call__(self, cmd, env=None, **kw):
        self.cmd, self.env = cmd, env
        return self

    def communicate(self, timeout=None):
        if self.hang and not self.killed:
            raise subprocess.TimeoutExpired(self.cmd, timeout)
        return self.output, None

    def kill(self):
        self.killed = True

    @property
    def returncode(self):
        return self.rc


@pytest.fixture()
def site(tmp_path):
    target = tmp_path / "site"
    target.mkdir()
    (target / "left-from-last-attempt").mkdir()
    (target / "stale.py").write_text("x")
    return target


def env(site, **extra):
    e = {"BOOTH_PIP_REQUIREMENTS": "/app/requirements.txt", "BOOTH_PIP_TARGET": str(site), "BOOTH_PIP_DEADLINE_SECONDS": "300"}
    e.update(extra)
    return e


def test_success_installs_into_the_target_with_pips_limits(site, tmp_path, monkeypatch):
    fake = FakePip(0, "Successfully installed booth-it-hello-1.0.0\n")
    monkeypatch.setattr(pi.subprocess, "Popen", fake)
    log = tmp_path / "term"
    assert pi.run(env(site, PIP_INDEX_URL="http://pypi.test/simple"), termination_log=str(log)) == 0
    assert fake.cmd[1:] == [
        "-m", "pip", "install", "--no-cache-dir", "--no-input", "--progress-bar", "off",
        "--target", str(site), "--timeout", "20", "--retries", "2", "-r", "/app/requirements.txt",
    ]
    assert fake.env["PIP_INDEX_URL"] == "http://pypi.test/simple"  # the operator's index, via pip's own env
    assert fake.env["HOME"].startswith(str(site)) and fake.env["TMPDIR"].startswith(str(site))
    assert sorted(p.name for p in site.iterdir()) == []  # previous attempt and pip's work dir are gone
    assert not log.exists()


def test_failure_message_is_the_tail_of_pips_output(site, tmp_path, monkeypatch):
    out = "".join(f"line {i}\n" for i in range(50)) + "ERROR: No matching distribution found for nope==9\n"
    monkeypatch.setattr(pi.subprocess, "Popen", FakePip(1, out))
    log = tmp_path / "term"
    assert pi.run(env(site), termination_log=str(log)) == 1
    msg = log.read_text()
    assert msg.startswith("pip install failed (exit 1):\n")
    assert msg.endswith("ERROR: No matching distribution found for nope==9")
    assert "line 29" not in msg and "line 31" in msg  # the last 20 non-empty lines
    assert len(msg.encode()) < 4096


def test_closed_egress_says_so_only_for_network_failures(site, tmp_path, monkeypatch):
    net = "WARNING: Retrying (Retry(total=1)) after connection broken by 'ConnectTimeoutError(...)'\nERROR: Could not find a version\n"
    log = tmp_path / "term"
    monkeypatch.setattr(pi.subprocess, "Popen", FakePip(1, net))
    pi.run(env(site, BOOTH_PIP_EGRESS_CLOSED="true"), termination_log=str(log))
    assert log.read_text().startswith("pip install needs internet access, and apps.egress.mode is closed")
    # The same failure with open egress is just pip's output.
    pi.run(env(site), termination_log=str(log))
    assert log.read_text().startswith("pip install failed (exit 1)")
    # A bad requirement with closed egress isn't blamed on the network.
    monkeypatch.setattr(pi.subprocess, "Popen", FakePip(1, "ERROR: Invalid requirement: '==='\n"))
    pi.run(env(site, BOOTH_PIP_EGRESS_CLOSED="true"), termination_log=str(log))
    assert log.read_text().startswith("pip install failed (exit 1)")


def test_a_hung_pip_is_stopped_at_the_deadline(site, tmp_path, monkeypatch):
    fake = FakePip(hang=True, output="Collecting booth-it-slow\n")
    monkeypatch.setattr(pi.subprocess, "Popen", fake)
    log = tmp_path / "term"
    assert pi.run(env(site, BOOTH_PIP_DEADLINE_SECONDS="60"), termination_log=str(log)) == 1
    assert fake.killed
    assert log.read_text().startswith("pip install did not finish within 60s (the module's apps.pip.timeout) and was stopped.")
    assert "Collecting booth-it-slow" in log.read_text()


def test_closed_egress_explained_even_when_the_deadline_stops_pip(site, tmp_path, monkeypatch):
    out = "WARNING: Retrying (Retry(total=1)) after connection broken by 'ConnectTimeoutError(...)'\n"
    monkeypatch.setattr(pi.subprocess, "Popen", FakePip(hang=True, output=out))
    log = tmp_path / "term"
    pi.run(env(site, BOOTH_PIP_EGRESS_CLOSED="true", BOOTH_PIP_DEADLINE_SECONDS="60"), termination_log=str(log))
    msg = log.read_text()
    assert msg.startswith("pip install needs internet access, and apps.egress.mode is closed")
    assert "did not finish within 60s" in msg and "ConnectTimeoutError" in msg


def test_a_real_pip_hang_is_killed(site, tmp_path, monkeypatch):
    # Not a fake: a real child process that never ends, stopped by the real deadline.
    real = subprocess.Popen
    monkeypatch.setattr(pi.subprocess, "Popen", lambda cmd, **kw: real([cmd[0], "-c", "import time; print('Collecting x', flush=True); time.sleep(600)"], **kw))
    log = tmp_path / "term"
    assert pi.run(env(site, BOOTH_PIP_DEADLINE_SECONDS="2"), termination_log=str(log)) == 1
    assert "did not finish within 2s" in log.read_text()
