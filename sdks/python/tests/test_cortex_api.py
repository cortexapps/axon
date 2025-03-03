import pytest
from utils import (
    mock_agent,  # noqa: F401
    mock_cortex_api,  # noqa: F401
    request_to_json,
)

from cortex_axon import AxonClient, CortexHTTPError
from generated import cortex_api_pb2


def mock_call(request, recvd_request=None, response_status = 201):
    if recvd_request is None:
        recvd_request = {}
    recvd_request.update(request_to_json(request))

    return cortex_api_pb2.CallResponse(
        status_code=response_status,
        body=f'{{"message": "201", "path":"{request.path}"}}',
    )

def test_call(mock_agent, mock_cortex_api):  # noqa: F811
    mock_cortex_api.mock.Call.side_effect = lambda request, context: mock_call(request)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[],
        scope=globals(),
    )

    ctx = client._handler_context({})
    resp = ctx.cortex_api_call(method="GET", path="/api/v1/some-api")

    assert resp.status_code == 201
    assert resp.text == '{"message": "201", "path":"/api/v1/some-api"}'


def test_param_passed(mock_agent, mock_cortex_api):  # noqa: F811
    recvd_request = {}
    mock_cortex_api.mock.Call.side_effect = lambda request, context: mock_call(request, recvd_request)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[],
        scope=globals(),
    )

    ctx = client._handler_context({})
    resp = ctx.cortex_api_call(method="GET", path="/api/v1/some-api", params={"X": "valueX", "Y": "white space"})

    assert resp.status_code == 201
    assert resp.text == '{"message": "201", "path":"/api/v1/some-api?X=valueX&Y=white%2Bspace"}'

def test_response_to_json(mock_agent, mock_cortex_api):  # noqa: F811
    mock_cortex_api.mock.Call.side_effect = lambda request, context: mock_call(request)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[],
        scope=globals(),
    )

    ctx = client._handler_context({})
    resp = ctx.cortex_api_call(method="GET", path="/api/v1/some-api")

    assert resp.status_code == 201
    assert resp.ok is True

    # body is plain-text
    assert resp.text == '{"message": "201", "path":"/api/v1/some-api"}'

    #json gives you the dict
    assert resp.json() == {"message": "201", "path":"/api/v1/some-api"}


def test_response_handle_errors(mock_agent, mock_cortex_api):  # noqa: F811
    mock_cortex_api.mock.Call.side_effect = lambda request, context: mock_call(request, response_status=401)

    client = AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[],
        scope=globals(),
    )

    ctx = client._handler_context({})
    resp = ctx.cortex_api_call(method="GET", path="/api/v1/some-api", params={"X": "valueX", "Y": "valueY", "Z": "valueZ"})

    assert resp.status_code == 401
    assert resp.ok is False
    with pytest.raises(CortexHTTPError):
        resp.raise_for_status()
