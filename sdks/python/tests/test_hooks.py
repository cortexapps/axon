import contextlib
import queue
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import grpc
import pytest

from cortex_axon import (
    AxonClient,
    FailureAction,
    FailureReason,
    FailureSource,
)
from cortex_axon.handler import CortexWebhook
from generated import (
    common_pb2,
    cortex_api_pb2_grpc,
    cortex_axon_agent_pb2,
    cortex_axon_agent_pb2_grpc,
)

HANDLER = "my_hooked_webhook"


class HookAgent(cortex_axon_agent_pb2_grpc.AxonAgentServicer):
    """Sends a fixed set of invocations and failure events, then holds both
    streams open, recording what the client sends back."""

    def __init__(self, invokes=(), events=(), events_code=None):
        self.invokes = list(invokes)
        self.events = list(events)
        self.events_code = events_code
        self.dispatch_requests = queue.Queue()
        self.events_requests = queue.Queue()
        self.resolutions = queue.Queue()
        self.events_calls = 0
        self.reports = queue.Queue()
        self.stop = threading.Event()

    def RegisterHandler(self, request, context):
        return cortex_axon_agent_pb2.RegisterHandlerResponse(
            error=common_pb2.Error(code="0", message="OK"),
            id=request.dispatch_id,
        )

    def Dispatch(self, request_iterator, context):
        request = next(request_iterator)
        self.dispatch_requests.put(request)
        for invoke in self.invokes:
            yield cortex_axon_agent_pb2.DispatchMessage(
                type=cortex_axon_agent_pb2.DISPATCH_MESSAGE_INVOKE,
                invoke=invoke,
            )
        self.invokes = []
        self.stop.wait(timeout=10)

    def DispatchEvents(self, request_iterator, context):
        self.events_calls += 1
        if self.events_code:
            context.abort(self.events_code, "not here")
        self.events_requests.put(next(request_iterator))

        def read_answers():
            # Ends with an error when the client goes away at teardown.
            with contextlib.suppress(Exception):
                for request in request_iterator:
                    if request.HasField("resolution"):
                        self.resolutions.put(request.resolution)

        threading.Thread(target=read_answers, daemon=True).start()
        events, self.events = self.events, []
        yield from events
        self.stop.wait(timeout=10)

    def ReportInvocation(self, request, context):
        self.reports.put(request)
        return cortex_axon_agent_pb2.ReportInvocationResponse()


class SlowApi(cortex_api_pb2_grpc.CortexApiServicer):
    def __init__(self):
        self.release = threading.Event()

    def Call(self, request, context):
        self.release.wait(timeout=10)
        context.abort(grpc.StatusCode.UNAVAILABLE, "too late")


def invoke(invocation_id="1", timeout_ms=0, deadline_in=None, **args):
    msg = cortex_axon_agent_pb2.DispatchHandlerInvoke(
        invocation_id=invocation_id,
        handler_id=HANDLER,
        handler_name=HANDLER,
        reason=cortex_axon_agent_pb2.WEBHOOK,
        timeout_ms=timeout_ms,
        args=args,
    )
    if deadline_in is not None:
        msg.deadline.FromMicroseconds(int((time.time() + deadline_in) * 1e6))
    return msg


def failure_event(invocation_id, attempt=1, **args):
    msg = invoke(invocation_id, **args)
    msg.attempt = attempt
    return cortex_axon_agent_pb2.DispatchEvent(
        invoke=msg,
        failure=cortex_axon_agent_pb2.InvocationFailure(
            reason=cortex_axon_agent_pb2.NO_RESOURCES,
            source=cortex_axon_agent_pb2.RUNTIME,
            message="dispatch queue is full",
            caller_rejected=True,
        ),
    )


def serve(servicer, add):
    server = grpc.server(ThreadPoolExecutor(max_workers=8))
    port = server.add_insecure_port("127.0.0.1:0")
    add(servicer, server)
    server.start()
    return server, port


class Hooks:
    def __init__(self, action=None):
        self.action = action
        self.failures = queue.Queue()

    def on_failure(self, info, failure):
        self.failures.put((info, failure))
        return self.action


@pytest.fixture
def run_client():
    servers = []
    agents = []
    clients = []

    def start(agent, handler, hooks=None, api=None, max_concurrent_handlers=4):
        server, port = serve(agent, cortex_axon_agent_pb2_grpc.add_AxonAgentServicer_to_server)
        servers.append(server)
        agents.append(agent)
        api_port = port
        if api:
            api_server, api_port = serve(api, cortex_api_pb2_grpc.add_CortexApiServicer_to_server)
            servers.append(api_server)

        webhook = CortexWebhook(id="hook")
        webhook.set_func(handler)
        client = AxonClient(
            agent_host="127.0.0.1",
            agent_port=port,
            cortex_host="127.0.0.1",
            cortex_port=api_port,
            handlers=[webhook],
            max_concurrent_handlers=max_concurrent_handlers,
            on_failure=hooks.on_failure if hooks else None,
        )
        threading.Thread(target=client.run, daemon=True).start()
        clients.append(client)
        return client

    yield start
    for agent in agents:
        agent.stop.set()
    for server in servers:
        server.stop(grace=None)
    # Drain handler and hook threads so none is still printing when the
    # interpreter exits.
    for client in clients:
        client._stop_failure_events()
        client._executor.shutdown(wait=True, cancel_futures=True)
        if client._hook_executor:
            client._hook_executor.shutdown(wait=True, cancel_futures=True)
        if client._events_thread:
            client._events_thread.join(timeout=5)


def reported_failure(agent):
    report = agent.reports.get(timeout=5)
    return report.failure if report.HasField("failure") else None


def test_success_reports_no_failure(run_client):
    agent = HookAgent(invokes=[invoke(body="payload")])

    def my_hooked_webhook(ctx):
        return "ok"

    run_client(agent, my_hooked_webhook, Hooks())

    assert reported_failure(agent) is None


def test_handler_error_is_reported_not_hooked_locally(run_client):
    hooks = Hooks(action=FailureAction.RETRY)
    agent = HookAgent(invokes=[invoke(body="payload")])

    def my_hooked_webhook(ctx):
        raise ValueError("ohnoes")

    run_client(agent, my_hooked_webhook, hooks)

    failure = reported_failure(agent)
    assert failure.reason == FailureReason.HANDLER_ERROR
    assert failure.source == FailureSource.APPLICATION
    assert failure.message == "ohnoes"
    # The agent sends it back as an event; the SDK doesn't run the hook itself.
    with pytest.raises(queue.Empty):
        hooks.failures.get(timeout=0.5)


def test_api_timeout_is_reported_as_runtime(run_client):
    api = SlowApi()
    agent = HookAgent(invokes=[invoke(timeout_ms=200)])

    def my_hooked_webhook(ctx):
        ctx.cortex_api_call("/api/v1/slow", "GET")

    try:
        run_client(agent, my_hooked_webhook, Hooks(), api=api)
        failure = reported_failure(agent)
    finally:
        api.release.set()
    assert failure.reason == FailureReason.TIMEOUT
    assert failure.source == FailureSource.RUNTIME


def test_slow_handler_is_reported_as_application_timeout(run_client):
    agent = HookAgent(invokes=[invoke(timeout_ms=200)])
    finished = threading.Event()

    def my_hooked_webhook(ctx):
        time.sleep(0.6)
        finished.set()

    run_client(agent, my_hooked_webhook, Hooks())

    report = agent.reports.get(timeout=5)
    assert report.error.code == "timeout"
    assert report.failure.reason == FailureReason.TIMEOUT
    assert report.failure.source == FailureSource.APPLICATION

    # The late finish is dropped: no second report.
    assert finished.wait(timeout=5)
    time.sleep(0.2)
    assert agent.reports.empty()


def test_expired_invocation_is_not_started(run_client):
    """The agent has already reported an invocation past its deadline, so the
    SDK must not run it: a failure hook may be replaying it."""
    ran = queue.Queue()
    agent = HookAgent(invokes=[
        invoke("stale", timeout_ms=10000, deadline_in=0.5),
        invoke("fresh", timeout_ms=10000, deadline_in=60),
    ])

    def my_hooked_webhook(ctx):
        ran.put(ctx.args)

    run_client(agent, my_hooked_webhook, Hooks())

    assert agent.reports.get(timeout=5).handler_invoke.invocation_id == "fresh"
    time.sleep(0.5)
    assert agent.reports.empty()
    assert ran.qsize() == 1


def test_timeout_capped_by_agent_deadline_is_runtime(run_client):
    # Its own timeout is long, but the agent gives up in 1.5s: it waited.
    agent = HookAgent(invokes=[invoke(timeout_ms=60000, deadline_in=1.5)])

    def my_hooked_webhook(ctx):
        time.sleep(2)

    run_client(agent, my_hooked_webhook, Hooks())

    failure = reported_failure(agent)
    assert failure.reason == FailureReason.TIMEOUT
    assert failure.source == FailureSource.RUNTIME


def test_failure_event_runs_hook_and_answers(run_client):
    hooks = Hooks(action=FailureAction.RETRY)
    agent = HookAgent(events=[failure_event("lost-1", attempt=2, body="payload")])

    run_client(agent, lambda ctx: None, hooks)

    first = agent.events_requests.get(timeout=5)
    assert first.dispatch_id
    assert not first.HasField("resolution")

    info, failure = hooks.failures.get(timeout=5)
    assert info.invocation_id == "lost-1"
    assert info.args == {"body": "payload"}
    assert info.attempt == 2
    assert failure.reason == FailureReason.NO_RESOURCES
    assert failure.source == FailureSource.RUNTIME
    assert failure.caller_rejected

    answer = agent.resolutions.get(timeout=5)
    assert answer.invocation_id == "lost-1"
    assert answer.action == cortex_axon_agent_pb2.FAILURE_ACTION_RETRY


def test_hook_returning_nothing_drops(run_client):
    agent = HookAgent(events=[failure_event("lost-1")])
    run_client(agent, lambda ctx: None, Hooks(action=None))

    assert agent.resolutions.get(timeout=5).action == cortex_axon_agent_pb2.FAILURE_ACTION_DROP


def test_hook_errors_drop_and_are_contained(run_client):
    agent = HookAgent(events=[failure_event("lost-1"), failure_event("lost-2")])

    class Broken(Hooks):
        def on_failure(self, info, failure):
            raise RuntimeError("hook bug")

    run_client(agent, lambda ctx: None, Broken())

    answers = [agent.resolutions.get(timeout=5) for _ in range(2)]
    assert {a.invocation_id for a in answers} == {"lost-1", "lost-2"}
    assert all(a.action == cortex_axon_agent_pb2.FAILURE_ACTION_DROP for a in answers)


def test_dispatch_request_advertises_failure_events(run_client):
    with_hooks = HookAgent()
    run_client(with_hooks, lambda ctx: None, Hooks())
    assert with_hooks.dispatch_requests.get(timeout=5).failure_events

    without_hooks = HookAgent()
    run_client(without_hooks, lambda ctx: None)
    assert not without_hooks.dispatch_requests.get(timeout=5).failure_events
    assert without_hooks.events_calls == 0


def test_failure_events_reach_hook_while_handlers_are_saturated(run_client):
    """The agent reports lost invocations exactly when every handler slot is
    busy. Those events must not wait behind the slots."""
    slots = 2
    release = threading.Event()
    started = threading.Barrier(slots + 1)
    events = [failure_event(f"lost-{i}", body=f"msg-{i}") for i in range(20)]
    # More invocations than slots, so the dispatch reader blocks.
    agent = HookAgent(invokes=[invoke(str(i)) for i in range(slots + 2)], events=events)
    hooks = Hooks(action=FailureAction.RETRY)

    def my_hooked_webhook(ctx):
        with contextlib.suppress(threading.BrokenBarrierError):
            started.wait(timeout=5)
        release.wait(timeout=10)

    try:
        run_client(agent, my_hooked_webhook, hooks, max_concurrent_handlers=slots)
        started.wait(timeout=5)

        seen = [hooks.failures.get(timeout=5) for _ in events]
        bodies = sorted(info.args["body"] for info, _ in seen)
        assert bodies == sorted(f"msg-{i}" for i in range(20))
        answered = {agent.resolutions.get(timeout=5).invocation_id for _ in events}
        assert answered == {f"lost-{i}" for i in range(20)}
    finally:
        release.set()
        started.abort()


def test_older_agent_without_failure_events(run_client):
    ran = queue.Queue()
    agent = HookAgent(invokes=[invoke()], events_code=grpc.StatusCode.UNIMPLEMENTED)

    def my_hooked_webhook(ctx):
        ran.put(True)

    client = run_client(agent, my_hooked_webhook, Hooks())
    ran.get(timeout=5)
    client._events_thread.join(timeout=5)
    assert not client._events_thread.is_alive()
    assert agent.events_calls == 1
