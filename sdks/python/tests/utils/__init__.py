from concurrent import futures
from unittest import mock

import grpc
import pytest
from google.protobuf.json_format import MessageToDict

from generated import cortex_api_pb2_grpc, cortex_axon_agent_pb2_grpc


class MockGrpcServer:
    port: int

    # The server is held here because grpc stops a server that becomes
    # unreachable, which ends the test that is using it.
    def __init__(self, port: int, server: grpc.Server):
        self.port = port
        self.server = server
        self.mock = mock.MagicMock()


def _start(add_servicer_to_server) -> MockGrpcServer:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
    port = server.add_insecure_port('[::]:0')
    mock_grpc_server = MockGrpcServer(port, server)
    add_servicer_to_server(mock_grpc_server.mock, server)

    server.start()

    return mock_grpc_server


@pytest.fixture
def mock_agent() -> MockGrpcServer:
    mock_grpc_server = _start(
        cortex_axon_agent_pb2_grpc.add_AxonAgentServicer_to_server
    )
    yield mock_grpc_server
    mock_grpc_server.server.stop(None)


@pytest.fixture
def mock_cortex_api() -> MockGrpcServer:
    mock_grpc_server = _start(cortex_api_pb2_grpc.add_CortexApiServicer_to_server)
    yield mock_grpc_server
    mock_grpc_server.server.stop(None)

def request_to_json(request):
    return MessageToDict(request, preserving_proto_field_name=True, use_integers_for_enums=True)
