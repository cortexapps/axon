import queue
import threading

from utils import (
    mock_agent,  # noqa: F401
    mock_cortex_api,  # noqa: F401
    request_to_json,
)

from cortex_axon import AxonClient
from cortex_axon.handler import CortexHandler, CortexScheduled, CortexWebhook
from generated import common_pb2, cortex_axon_agent_pb2


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
