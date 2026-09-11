import queue
import threading
from concurrent.futures import ThreadPoolExecutor

import grpc
from utils import (
    mock_agent,  # noqa: F401
    mock_cortex_api,  # noqa: F401
    request_to_json,
)

from cortex_axon import AxonClient
from cortex_axon.handler import CortexHandler, CortexScheduled, CortexWebhook
from generated import common_pb2, cortex_axon_agent_pb2, cortex_axon_agent_pb2_grpc


def register_handler(request, assert_queue):
    assert_queue.put(request_to_json(request))
    return cortex_axon_agent_pb2.RegisterHandlerResponse(
        error=common_pb2.Error(code="0", message="OK"),
        id=request.dispatch_id
    )


def dispatch_server(request_iterator):
    request = next(request_iterator)
    while True:
        yield cortex_axon_agent_pb2.DispatchResponse(
            invocation_id="1",
            dispatch_id=request.dispatch_id,
            handler_id="my_handler",
            handler_name="my_handler",
        )


def test_register(mock_agent, mock_cortex_api):  # noqa: F811
    assert_queue = queue.Queue()

    def my_handler():
        print("my_handler")
        nonlocal assert_queue
        assert_queue.put(True)

    scheduled = CortexScheduled(
        interval="5s",
        cron=None,
        run_now=True
    )

    scheduled.set_func(my_handler)

    mock_agent.mock.RegisterHandler.side_effect = lambda request, context: register_handler(
        request, assert_queue)
    mock_agent.mock.Dispatch.side_effect = lambda self, request_iterator, context: dispatch_server(
        request_iterator)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[scheduled],
        scope=globals()
    )

    client_thread = threading.Thread(
        target=lambda c: c.run(),
        args=([client])
    )

    client_thread.daemon = True
    client_thread.start()

    server_registered_handler = assert_queue.get(timeout=5)

    del server_registered_handler['dispatch_id']
    assert server_registered_handler == {
        "handler_name": "my_handler",
        "options":  [{'invoke': {'type': cortex_axon_agent_pb2.RUN_INTERVAL, 'value': '5s'}}, {'invoke': {'type': cortex_axon_agent_pb2.RUN_NOW}}]
    }


def test_register_webhook(mock_agent, mock_cortex_api):  # noqa: F811
    assert_queue = queue.Queue()

    def my_webhook():
        print("my_handler")
        nonlocal assert_queue
        assert_queue.put(True)

    scheduled = CortexWebhook(
        id="my-webhook",
    )

    scheduled.set_func(my_webhook)

    mock_agent.mock.RegisterHandler.side_effect = lambda request, context: register_handler(
        request, assert_queue)
    mock_agent.mock.Dispatch.side_effect = lambda self, request_iterator, context: dispatch_server(
        request_iterator)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[scheduled],
        scope=globals()
    )

    client_thread = threading.Thread(
        target=lambda c: c.run(),
        args=([client])
    )

    client_thread.daemon = True
    client_thread.start()

    server_registered_handler = assert_queue.get(timeout=5)

    del server_registered_handler['dispatch_id']
    assert server_registered_handler == {
        "handler_name": "my_webhook",
        "options":  [{'invoke': {'type': cortex_axon_agent_pb2.WEBHOOK, 'value': 'my-webhook'}}]
    }


def test_register_handler(mock_agent, mock_cortex_api):  # noqa: F811
    assert_queue = queue.Queue()

    def my_invoke_handler():
        print("my_handler")
        nonlocal assert_queue
        assert_queue.put(True)

    handler = CortexHandler()

    handler.set_func(my_invoke_handler)

    mock_agent.mock.RegisterHandler.side_effect = lambda request, context: register_handler(
        request, assert_queue)
    mock_agent.mock.Dispatch.side_effect = lambda self, request_iterator, context: dispatch_server(
        request_iterator)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[handler],
        scope=globals()
    )

    client_thread = threading.Thread(
        target=lambda c: c.run(),
        args=([client])
    )

    client_thread.daemon = True
    client_thread.start()

    server_registered_handler = assert_queue.get(timeout=5)

    del server_registered_handler['dispatch_id']
    assert server_registered_handler == {
        "handler_name": "my_invoke_handler",
        "options":  [{'invoke': {}}] # this is empty because INVOKE is default
    }


class ConcurrencyAgent(cortex_axon_agent_pb2_grpc.AxonAgentServicer):
    """A real servicer: MagicMock cannot serve a streaming RPC."""

    def __init__(self, handler_name, invoke_count, keep_stream_open):
        self.handler_name = handler_name
        self.invoke_count = invoke_count
        self.keep_stream_open = keep_stream_open

    def RegisterHandler(self, request, context):
        return cortex_axon_agent_pb2.RegisterHandlerResponse(
            error=common_pb2.Error(code="0", message="OK"),
            id=request.dispatch_id,
        )

    def Dispatch(self, request_iterator, context):
        request = next(request_iterator)
        for i in range(self.invoke_count):
            yield cortex_axon_agent_pb2.DispatchMessage(
                type=cortex_axon_agent_pb2.DISPATCH_MESSAGE_INVOKE,
                invoke=cortex_axon_agent_pb2.DispatchHandlerInvoke(
                    invocation_id=str(i),
                    dispatch_id=request.dispatch_id,
                    handler_id=self.handler_name,
                    handler_name=self.handler_name,
                ),
            )
        self.keep_stream_open.wait(timeout=10)

    def ReportInvocation(self, request, context):
        return cortex_axon_agent_pb2.ReportInvocationResponse()


def test_handlers_run_concurrently():
    """A slow handler must not hold back the invocations behind it.

    Handlers used to run inline on the dispatch stream loop, so invocation
    two was not read until invocation one returned. Under webhook load that
    let the agent time every queued invocation out while the client worked
    through them one at a time.
    """
    handler_count = 4
    # handler_count workers plus this thread: the barrier only trips if every
    # handler is inside the call at the same moment.
    all_started = threading.Barrier(handler_count + 1)
    release = threading.Event()

    def my_concurrent_webhook(ctx):
        all_started.wait(timeout=10)
        release.wait(timeout=10)

    webhook = CortexWebhook(id="my-webhook")
    webhook.set_func(my_concurrent_webhook)

    server = grpc.server(ThreadPoolExecutor(max_workers=handler_count + 2))
    port = server.add_insecure_port("127.0.0.1:0")
    cortex_axon_agent_pb2_grpc.add_AxonAgentServicer_to_server(
        ConcurrencyAgent("my_concurrent_webhook", handler_count, release),
        server,
    )
    server.start()

    try:
        client = AxonClient(
            agent_host="127.0.0.1",
            agent_port=port,
            handlers=[webhook],
            scope=globals(),
            max_concurrent_handlers=handler_count,
        )

        client_thread = threading.Thread(target=client.run, daemon=True)
        client_thread.start()

        try:
            all_started.wait(timeout=10)
        except threading.BrokenBarrierError:
            raise AssertionError(
                "handlers did not run concurrently: fewer than "
                f"{handler_count} were in flight at once"
            ) from None
    finally:
        release.set()
        server.stop(grace=None)
