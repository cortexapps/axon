import json
import queue
import traceback
from datetime import datetime
from time import sleep
from typing import Any, Optional
from urllib.parse import quote_plus

import grpc

from generated import (
    common_pb2,
    cortex_api_pb2,
    cortex_api_pb2_grpc,
    cortex_axon_agent_pb2,
    cortex_axon_agent_pb2_grpc,
)

from .agentversion import AGENT_VERSION
from .handler import (
    CortexAnnotation,
    find_annotated_methods,
)


# TODO Add docstrings
class CortexRequestException(IOError):
    """There was an ambiguous exception that occurred while handling your
    request.
    """

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)


class CortexHTTPError(CortexRequestException):
    """An HTTP error occurred."""


class CortexResponse:
    """
    A class to represent the response from a Cortex API call.

    Attributes:
    -----------
    status_code : int
        The HTTP status code of the response.
    text : str
        The body of the response. In plaintext
    headers : Dict[str, str]
        The headers of the response.
    """

    def __init__(self, status_code: int, text: str, headers: dict[str, str]):
        """
        Constructs all the necessary attributes for the CortexResponse object.

        Parameters:
        -----------
        status_code : int
            The HTTP status code of the response.
        body : str
            The body of the response.
        headers : Dict[str, str]
            The headers of the response.
        """
        self.status_code = status_code
        self.text = text
        self.headers = headers

    def json(self) -> Any:
        """
        Returns the JSON representation of the response body.

        Returns:
        --------
        Any
            The JSON representation of the response body.
        """
        return json.loads(self.text)

    def raise_for_status(self):
        """Raises :class:`HTTPError`, if one occurred."""

        http_error_msg = ''
        if 400 <= self.status_code < 500:
            http_error_msg = f'{self.status_code} Client Error: {self.text}'

        elif 500 <= self.status_code < 600:
            http_error_msg = f'{self.status_code} Server Error: {self.text}'

        if http_error_msg:
            raise CortexHTTPError(http_error_msg)

    @property
    def ok(self):
        """Returns True if :attr:`status_code` is less than 400, False if not.

        This attribute checks if the status code of the response is between
        400 and 600 to see if there was a client error or a server error. If
        the status code is between 200 and 400, this will return True. This
        is **not** a check to see if the response code is ``200 OK``.
        """
        try:
            self.raise_for_status()
        except CortexHTTPError:
            return False
        return True


def _client_message_iterator(initial_message: cortex_axon_agent_pb2.DispatchRequest):
    blocking_queue = queue.Queue()
    blocking_queue.put(initial_message)
    while True:
        message = blocking_queue.get()
        yield message


class AxonClient:
    def __init__(
        self,
        agent_host: str = "localhost",
        agent_port: int = 50051,
        cortex_host: str = None,
        cortex_port: int = None,
        handlers: Optional[list[CortexAnnotation]] = None,
        scope=None,
    ):
        self.id = str(datetime.now().timestamp())
        self.agent_hostport = f"{agent_host}:{agent_port}"

        cortex_api_host = cortex_host or agent_host
        cortex_api_port = cortex_port or agent_port
        self.cortex_hostport = f"{cortex_api_host}:{cortex_api_port}"

        self.cortex_channel = grpc.insecure_channel(self.cortex_hostport)
        self.cortex_stub = cortex_api_pb2_grpc.CortexApiStub(
            self.cortex_channel)

        self.agent_channel = None
        self.agent_stub = None

        if not scope and not handlers:
            raise ValueError("One of scope (globals()) or handlers required")

        self.handlers = handlers or find_annotated_methods(scope)

    def _get_stub(self):
        if not self.agent_stub:
            self.agent_channel = grpc.insecure_channel(self.agent_hostport)
            self.agent_stub = cortex_axon_agent_pb2_grpc.AxonAgentStub(
                self.agent_channel
            )
        return self.agent_stub

    def _clear_stub(self):
        if self.agent_stub:
            self.agent_channel.close()
            self.agent_stub = None

    def _register_handlers(self):
        for h in self.handlers:
            options = h.get_handler_options()

            print(f"Registering handler: {h.name}")
            self.agent_stub.RegisterHandler(
                cortex_axon_agent_pb2.RegisterHandlerRequest(
                    dispatch_id=self.id,
                    handler_name=h.name,
                    timeout_ms=h.timeout,
                    options=options,
                )
            )

    def _find_handler(self, name):
        for h in self.handlers:
            if h.name == name:
                return h
        return None

    def _handler_context(self, args):
        return HandlerContext(self.cortex_stub, args)

    def run(self):
        registered = False
        while True:
            stub = self._get_stub()
            try:
                if not registered:
                    self._register_handlers()
                    registered = True

                initial_message = cortex_axon_agent_pb2.DispatchRequest(
                    dispatch_id=self.id,
                    client_version=AGENT_VERSION
                )
                for response in stub.Dispatch(
                    _client_message_iterator(initial_message)
                ):

                    if response.type == cortex_axon_agent_pb2.DISPATCH_MESSAGE_INVOKE:
                        invoke = response.invoke
                        args = invoke.args
                        handler_to_invoke = self._find_handler(
                            invoke.handler_name)
                        if not handler_to_invoke:
                            print(f"Unknown handler: {response.handler_name}")
                            continue
                        print(
                            f"Dispatch handler START {invoke.handler_name} (id={invoke.invocation_id}) reason={invoke.reason}"
                        )
                        handler_err = None
                        handler_result = None
                        result = None
                        now = datetime.now()
                        ctx = self._handler_context(args)
                        duration_ms = 0

                        try:
                            result = handler_to_invoke(ctx)
                            duration_ms = int(
                                (datetime.now() - now).total_seconds() * 1000
                            )
                            if result:
                                handler_result = cortex_axon_agent_pb2.InvokeResult(
                                    value=str(result)
                                )
                            print(f"Dispatch handler SUCCESS {invoke.handler_name} (id={invoke.invocation_id}) in {round((datetime.now() - now).total_seconds() * 1000, 1)}ms. Returned result={result is not None}")
                        except Exception as e:
                            print(f"Dispatch handler ERROR {invoke.handler_name} (id={invoke.invocation_id}) in {round((datetime.now() - now).total_seconds() * 1000, 1)}ms. Error={e}")
                            traceback.print_exc()
                            handler_err = common_pb2.Error(
                                code="unexpected", message="Error calling handler: " + str(e)
                            )

                        invoke_info = cortex_axon_agent_pb2.ReportInvocationRequest(
                            handler_invoke=invoke,
                            start_client_timestamp=now,
                            duration_ms=duration_ms,
                            result=handler_result,
                            error=handler_err,
                            logs=ctx.logs,
                        )
                        report_response = self.agent_stub.ReportInvocation(
                            invoke_info)

                        if report_response.error and report_response.error.code:
                            print(
                                f"Error reporting invocation: {report_response.error}"
                            )
                    elif (
                        response.type == cortex_axon_agent_pb2.DISPATCH_MESSAGE_WORK_COMPLETED
                    ):
                        print("Work completed, shutting down...")
                        return
                    else:
                        print(f"Unknown message type: {response.type}")

                print("Response stream disconnected... retrying in 5 seconds...")
                sleep(5)
            except Exception as e:
                print("Error calling axon server, retrying in 5 seconds...")
                print(e)
                traceback.print_exc()
                self._clear_stub()
                registered = False
                sleep(5)
                continue


class HandlerContext:

    api: cortex_api_pb2_grpc.CortexApiStub
    logs: list[cortex_axon_agent_pb2.Log]
    args: dict
    obj: dict[str, Any]

    def __init__(self, api: cortex_api_pb2_grpc.CortexApiStub, args: dict):
        self.api = api
        self.args = args
        self.logs = []
        self.obj = {}

    def log(self, message: str, level="INFO"):
        """
        Log a message with a given level.
        :param message:
        :param level:
        """
        self.logs.append(
            cortex_axon_agent_pb2.Log(
                timestamp=datetime.now(), level=level, message=message
            )
        )
        print(f"{level}: {message}")

    def cortex_api_call(
        self, path: str, method: str, body: str = None, content_type: str = "application/json", params: dict[str, str] = None
    ) -> CortexResponse:
        if params:
            encoded_params = {k: quote_plus(v) for k, v in params.items()}
            query_string = "&".join(f"{k}={v}" for k, v in encoded_params.items())
            path = f"{path}?{query_string}"

        request = cortex_api_pb2.CallRequest(
            method=method,
            path=path,
            body=body,
            content_type=content_type,
        )
        response = self.api.Call(request)
        return CortexResponse(response.status_code, response.body, response.headers)
