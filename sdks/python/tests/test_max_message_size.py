import grpc
import pytest
from utils import (
    mock_agent,  # noqa: F401
    mock_cortex_api,  # noqa: F401
)

from cortex_axon import AxonClient
from cortex_axon.axon_client import (
    DEFAULT_MAX_RECEIVE_MESSAGE_SIZE,
    MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR,
)
from generated import cortex_api_pb2

OVERSIZE_BODY_LENGTH = DEFAULT_MAX_RECEIVE_MESSAGE_SIZE + 16 * 1024


def mock_oversize_call(request, context):
    return cortex_api_pb2.CallResponse(
        status_code=200,
        body="x" * OVERSIZE_BODY_LENGTH,
    )


def make_client(mock_agent, mock_cortex_api, **kwargs):  # noqa: F811
    return AxonClient(
        agent_host="localhost",
        agent_port=mock_agent.port,
        cortex_host="localhost",
        cortex_port=mock_cortex_api.port,
        handlers=[],
        scope=globals(),
        **kwargs,
    )


def test_oversize_response_rejected_by_default(mock_agent, mock_cortex_api):  # noqa: F811
    mock_cortex_api.mock.Call.side_effect = mock_oversize_call

    ctx = make_client(mock_agent, mock_cortex_api)._handler_context({})

    with pytest.raises(grpc.RpcError) as err:
        ctx.cortex_api_call(method="GET", path="/api/v1/catalog/descriptors")

    assert err.value.code() == grpc.StatusCode.RESOURCE_EXHAUSTED


def test_oversize_response_accepted_with_argument(mock_agent, mock_cortex_api):  # noqa: F811
    mock_cortex_api.mock.Call.side_effect = mock_oversize_call

    ctx = make_client(
        mock_agent, mock_cortex_api, max_receive_message_size=16 * 1024 * 1024
    )._handler_context({})

    resp = ctx.cortex_api_call(method="GET", path="/api/v1/catalog/descriptors")

    assert resp.status_code == 200
    assert len(resp.text) == OVERSIZE_BODY_LENGTH


def test_oversize_response_accepted_with_env_var(monkeypatch, mock_agent, mock_cortex_api):  # noqa: F811
    monkeypatch.setenv(MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR, str(16 * 1024 * 1024))
    mock_cortex_api.mock.Call.side_effect = mock_oversize_call

    ctx = make_client(mock_agent, mock_cortex_api)._handler_context({})

    resp = ctx.cortex_api_call(method="GET", path="/api/v1/catalog/descriptors")

    assert resp.status_code == 200
    assert len(resp.text) == OVERSIZE_BODY_LENGTH


def test_argument_overrides_env_var(monkeypatch, mock_agent, mock_cortex_api):  # noqa: F811
    monkeypatch.setenv(MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR, str(16 * 1024 * 1024))
    mock_cortex_api.mock.Call.side_effect = mock_oversize_call

    ctx = make_client(
        mock_agent, mock_cortex_api, max_receive_message_size=DEFAULT_MAX_RECEIVE_MESSAGE_SIZE
    )._handler_context({})

    with pytest.raises(grpc.RpcError) as err:
        ctx.cortex_api_call(method="GET", path="/api/v1/catalog/descriptors")

    assert err.value.code() == grpc.StatusCode.RESOURCE_EXHAUSTED


def test_unlimited_env_var(monkeypatch, mock_agent, mock_cortex_api):  # noqa: F811
    monkeypatch.setenv(MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR, "-1")
    mock_cortex_api.mock.Call.side_effect = mock_oversize_call

    ctx = make_client(mock_agent, mock_cortex_api)._handler_context({})

    resp = ctx.cortex_api_call(method="GET", path="/api/v1/catalog/descriptors")

    assert resp.status_code == 200
    assert len(resp.text) == OVERSIZE_BODY_LENGTH


def test_invalid_env_var_is_reported(monkeypatch, mock_agent, mock_cortex_api):  # noqa: F811
    monkeypatch.setenv(MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR, "8MB")

    with pytest.raises(ValueError, match=MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR):
        make_client(mock_agent, mock_cortex_api)
