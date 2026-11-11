"""The SDK against a real agent, triggered through the agent's webhook
endpoint. The agent is built from ../../agent with `go`, or taken from
AXON_AGENT_BIN; without either these tests are skipped."""

import collections
import os
import queue
import shutil
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from cortex_axon import AxonClient, FailureAction, FailureReason, FailureSource
from cortex_axon.handler import CortexWebhook

AGENT_DIR = Path(__file__).resolve().parents[3] / "agent"

# AxonClient.run has no way to stop, so a test's client keeps retrying after
# its agent is killed, and may print into pytest's closed capture.
pytestmark = pytest.mark.filterwarnings("ignore::pytest.PytestUnhandledThreadExceptionWarning")


@pytest.fixture(scope="session")
def agent_bin(tmp_path_factory):
    if os.environ.get("AXON_AGENT_BIN"):
        return os.environ["AXON_AGENT_BIN"]
    if not shutil.which("go"):
        pytest.skip("needs go to build the agent, or AXON_AGENT_BIN")
    out = tmp_path_factory.mktemp("agent") / "axon-agent"
    subprocess.run(["go", "build", "-o", str(out), "."], cwd=AGENT_DIR, check=True)
    return str(out)


def free_ports(n):
    """Return n distinct free ports, holding each open until all are picked."""
    socks = [socket.socket() for _ in range(n)]
    try:
        for s in socks:
            s.bind(("127.0.0.1", 0))
        return [s.getsockname()[1] for s in socks]
    finally:
        for s in socks:
            s.close()


class RealAgent:
    def __init__(self, agent_bin, tmp_path):
        self.grpc_port, self.webhook_port, http_port = free_ports(3)
        env = dict(
            os.environ,
            DRYRUN="true",
            PORT=str(self.grpc_port),
            HTTP_PORT=str(http_port),
            WEBHOOK_PORT=str(self.webhook_port),
            HANDLER_HISTORY_PATH=str(tmp_path / "history"),
        )
        # Stays open for the agent process's lifetime; closed in stop().
        self.log = open(tmp_path / "agent.log", "wb")  # noqa: SIM115
        self.proc = subprocess.Popen(
            [agent_bin, "serve"], env=env, stdout=self.log, stderr=subprocess.STDOUT
        )
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                socket.create_connection(("127.0.0.1", self.grpc_port), timeout=0.2).close()
                return
            except OSError:
                time.sleep(0.05)
        raise AssertionError("agent never started listening")

    def post(self, webhook_id, body):
        """Sends body once the webhook is live; returns the HTTP status."""
        url = f"http://127.0.0.1:{self.webhook_port}/webhook/{webhook_id}"
        deadline = time.monotonic() + 10
        while True:
            req = urllib.request.Request(url, data=body.encode(), method="POST",
                                         headers={"Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(req) as r:
                    return r.status
            except urllib.error.HTTPError as e:
                # 404 until the SDK has registered and started dispatching.
                if e.code != 404 or time.monotonic() > deadline:
                    return e.code
            time.sleep(0.05)

    def stop(self):
        self.proc.kill()
        self.proc.wait()
        self.log.close()


@pytest.fixture
def real_agent(agent_bin, tmp_path):
    agent = RealAgent(agent_bin, tmp_path)
    yield agent
    agent.stop()


class Recorder:
    """Records each run of the handler and each call of the failure hook,
    which always asks for a retry."""

    def __init__(self):
        self.lock = threading.Lock()
        self.runs = collections.Counter()
        self.succeeded = collections.Counter()
        self.failures = []
        self.failure_seen = queue.Queue()
        self.success_seen = queue.Queue()

    def ran(self, ctx):
        with self.lock:
            self.runs[ctx.args["body"]] += 1
            return self.runs[ctx.args["body"]]

    def ok(self, ctx):
        with self.lock:
            self.succeeded[ctx.args["body"]] += 1
        self.success_seen.put(ctx.args["body"])

    def on_failure(self, info, failure):
        with self.lock:
            self.failures.append((info, failure))
        self.failure_seen.put((info, failure))
        return FailureAction.RETRY


def run_client(agent, recorder, webhook_id, func, timeout_ms=0, slots=4):
    webhook = CortexWebhook(id=webhook_id, timeout_ms=timeout_ms)
    webhook.set_func(func)
    client = AxonClient(
        agent_host="127.0.0.1",
        agent_port=agent.grpc_port,
        handlers=[webhook],
        max_concurrent_handlers=slots,
        on_failure=recorder.on_failure,
    )
    threading.Thread(target=client.run, daemon=True).start()
    return client


def test_handler_error_is_hooked_and_retried(real_agent):
    rec = Recorder()

    def e2e_error(ctx):
        if rec.ran(ctx) == 1:
            raise ValueError("first attempt fails")
        rec.ok(ctx)

    run_client(real_agent, rec, "e2e-error", e2e_error)
    assert real_agent.post("e2e-error", '{"id": 42}') == 200

    info, failure = rec.failure_seen.get(timeout=15)
    assert failure.reason == FailureReason.HANDLER_ERROR
    assert failure.source == FailureSource.APPLICATION
    assert failure.message == "first attempt fails"
    assert info.args["body"] == '{"id": 42}'
    assert info.attempt == 1

    # The retry carries the original message, and succeeds.
    assert rec.success_seen.get(timeout=15) == '{"id": 42}'
    with pytest.raises(queue.Empty):
        rec.failure_seen.get(timeout=1)
    assert rec.runs['{"id": 42}'] == 2


def test_timeout_is_hooked_and_retried(real_agent):
    rec = Recorder()

    def e2e_timeout(ctx):
        if rec.ran(ctx) == 1:
            time.sleep(3)
            return
        rec.ok(ctx)

    run_client(real_agent, rec, "e2e-timeout", e2e_timeout, timeout_ms=500)
    assert real_agent.post("e2e-timeout", '{"id": 7}') == 200

    info, failure = rec.failure_seen.get(timeout=15)
    assert failure.reason == FailureReason.TIMEOUT
    assert failure.source == FailureSource.APPLICATION
    assert info.args["body"] == '{"id": 7}'

    assert rec.success_seen.get(timeout=15) == '{"id": 7}'

    # The SDK reported the timeout itself, so the agent must not report it
    # again once its grace period is over, and the late finish is dropped.
    with pytest.raises(queue.Empty):
        rec.failure_seen.get(timeout=7)
    assert rec.succeeded['{"id": 7}'] == 1


def test_saturated_invocations_are_hooked_retried_and_run_once(real_agent):
    """One handler slot, stuck. The invocations queued behind it expire; the
    agent reports them, the hook retries them, and each message ends up
    succeeding exactly once. None of the expired originals runs late."""
    rec = Recorder()
    release = threading.Event()
    bodies = [f"m{i}" for i in range(5)]

    def e2e_busy(ctx):
        rec.ran(ctx)
        if not release.is_set():
            release.wait(30)
            return
        rec.ok(ctx)

    original_hook = rec.on_failure

    def on_failure(info, failure):
        if failure.source == FailureSource.RUNTIME:
            # The agent has given up on the queued ones: unstick the slot.
            release.set()
        return original_hook(info, failure)

    rec.on_failure = on_failure
    run_client(real_agent, rec, "e2e-busy", e2e_busy, timeout_ms=1000, slots=1)

    assert real_agent.post("e2e-busy", bodies[0]) == 200
    for body in bodies[1:]:
        assert real_agent.post("e2e-busy", body) == 200

    succeeded = {rec.success_seen.get(timeout=30) for _ in bodies}
    assert succeeded == set(bodies)
    with pytest.raises(queue.Empty):
        rec.success_seen.get(timeout=2)

    with rec.lock:
        assert all(rec.succeeded[b] == 1 for b in bodies), rec.succeeded
        # Each failure is reported once per invocation.
        ids = [info.invocation_id for info, _ in rec.failures]
        assert len(ids) == len(set(ids)), "an invocation was reported twice"
        # Every message failed at least once, and the queued ones were
        # reported by the agent as Axon's fault.
        failed = collections.Counter(info.args["body"] for info, _ in rec.failures)
        assert set(failed) == set(bodies)
        runtime = {info.args["body"] for info, f in rec.failures if f.source == FailureSource.RUNTIME}
        assert set(bodies[1:]) <= runtime
        # The expired originals never ran: each queued message ran once, as
        # its retry.
        assert all(rec.runs[b] == 1 for b in bodies[1:]), rec.runs
