import json
import os
import queue
import threading
import traceback
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from time import monotonic, sleep, time
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
from .hooks import (
    Failure,
    FailureAction,
    FailureHook,
    FailureReason,
    FailureSource,
    InvocationInfo,
)

DEFAULT_MAX_CONCURRENT_HANDLERS = 8
RECONNECT_WAIT_SECONDS = 5
# How far ahead of the agent's deadline a handler must finish, so its report
# reaches the agent before the agent gives up on it.
DEADLINE_MARGIN_SECONDS = 1.0


def _invoke_timeout(invoke, now: float):
    """How long a handler may run, in seconds (None for no limit): its own
    timeout, but no later than the agent's deadline. capped means the agent's
    deadline is the tighter one, i.e. the invocation waited before it got
    here. expired means it is already too late to start."""
    timeout = invoke.timeout_ms / 1000 if invoke.timeout_ms > 0 else None
    if not invoke.HasField("deadline"):
        return timeout, False, False
    remaining = invoke.deadline.ToMicroseconds() / 1e6 - now - DEADLINE_MARGIN_SECONDS
    if remaining <= 0:
        return None, False, True
    if timeout is None or remaining < timeout:
        return remaining, True, False
    return timeout, False, False


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


DEFAULT_MAX_RECEIVE_MESSAGE_SIZE = 4 * 1024 * 1024
MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR = "AXON_GRPC_MAX_RECEIVE_MESSAGE_SIZE"


def _resolve_max_receive_message_size(max_receive_message_size: Optional[int]) -> int:
    if max_receive_message_size is not None:
        return max_receive_message_size

    value = os.environ.get(MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR)
    if not value:
        return DEFAULT_MAX_RECEIVE_MESSAGE_SIZE

    try:
        return int(value)
    except ValueError:
        raise ValueError(
            f"{MAX_RECEIVE_MESSAGE_SIZE_ENV_VAR} must be a whole number of bytes, or -1 for no limit, but is {value!r}"
        ) from None


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
        max_receive_message_size: Optional[int] = None,
        max_concurrent_handlers: int = DEFAULT_MAX_CONCURRENT_HANDLERS,
        on_failure: Optional[FailureHook] = None,
    ):
        if max_concurrent_handlers < 1:
            raise ValueError("max_concurrent_handlers must be at least 1")

        self.id = str(datetime.now().timestamp())
        self.agent_hostport = f"{agent_host}:{agent_port}"

        cortex_api_host = cortex_host or agent_host
        cortex_api_port = cortex_port or agent_port
        self.cortex_hostport = f"{cortex_api_host}:{cortex_api_port}"

        # gRPC caps received messages at 4MB, which large Cortex API responses
        # can exceed. Sends stay at the gRPC default of no limit.
        self.grpc_channel_options = [
            (
                "grpc.max_receive_message_length",
                _resolve_max_receive_message_size(max_receive_message_size),
            ),
        ]

        self.cortex_channel = grpc.insecure_channel(
            self.cortex_hostport, options=self.grpc_channel_options
        )
        self.cortex_stub = cortex_api_pb2_grpc.CortexApiStub(
            self.cortex_channel)

        self.agent_channel = None
        self.agent_stub = None

        if not scope and not handlers:
            raise ValueError("One of scope (globals()) or handlers required")

        self.handlers = handlers or find_annotated_methods(scope)

        # Handlers run on this pool rather than inline on the dispatch stream,
        # so a slow one does not hold back every invocation behind it. The
        # semaphore stops reading once every worker is busy, which pushes
        # backpressure onto the agent instead of queueing without limit here.
        self._executor = ThreadPoolExecutor(
            max_workers=max_concurrent_handlers,
            thread_name_prefix="axon-handler",
        )
        self._handler_slots = threading.BoundedSemaphore(max_concurrent_handlers)

        self.on_failure = on_failure
        # Failure events have their own thread and pool, apart from the
        # handler slots: they arrive exactly when every handler is busy, and
        # must never wait for one to free up.
        self._hook_executor = None
        self._events_thread = None
        self._events_stop = threading.Event()
        self._events_call = None
        if on_failure:
            self._hook_executor = ThreadPoolExecutor(
                max_workers=2, thread_name_prefix="axon-hook"
            )

    def _get_stub(self):
        if not self.agent_stub:
            self.agent_channel = grpc.insecure_channel(
                self.agent_hostport, options=self.grpc_channel_options
            )
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

    def _run_failure_hook(self, info: InvocationInfo, failure: Failure, stub):
        if not self.on_failure:
            return
        try:
            action = self.on_failure(info, failure)
        except Exception:
            print(f"Error in on_failure hook for {info.handler_name} (id={info.invocation_id})")
            traceback.print_exc()
            return
        if action == FailureAction.RETRY:
            self._retry_invocation(info, stub)

    def _retry_invocation(self, info: InvocationInfo, stub):
        try:
            response = stub.RetryInvocation(
                cortex_axon_agent_pb2.RetryInvocationRequest(invoke=info.invoke)
            )
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.UNIMPLEMENTED:
                print("Agent does not support retries, upgrade the agent")
            else:
                print(f"Error retrying invocation {info.invocation_id}: {e}")
            return
        if response.error and response.error.code:
            print(f"Agent rejected retry of {info.invocation_id}: {response.error.message}")
            return
        print(
            f"Retrying {info.handler_name} (id={info.invocation_id}) "
            f"as id={response.invocation_id} attempt={info.attempt + 1}"
        )

    def _start_failure_events(self):
        if not self.on_failure or self._events_thread:
            return
        self._events_thread = threading.Thread(
            target=self._run_failure_events, name="axon-failure-events", daemon=True
        )
        self._events_thread.start()

    def _stop_failure_events(self):
        self._events_stop.set()
        call = self._events_call
        if call:
            call.cancel()

    def _run_failure_events(self):
        # A channel of its own, so the dispatch loop resetting its channel
        # after an error doesn't cut this stream too.
        channel = grpc.insecure_channel(
            self.agent_hostport, options=self.grpc_channel_options
        )
        stub = cortex_axon_agent_pb2_grpc.AxonAgentStub(channel)
        try:
            while not self._events_stop.is_set():
                try:
                    self._events_call = stub.DispatchEvents(
                        cortex_axon_agent_pb2.DispatchRequest(
                            dispatch_id=self.id,
                            client_version=AGENT_VERSION,
                            failure_events=True,
                        )
                    )
                    for event in self._events_call:
                        if not event.HasField("invoke"):
                            continue
                        self._hook_executor.submit(
                            self._run_failure_hook,
                            InvocationInfo.from_proto(event.invoke),
                            Failure.from_proto(event.failure),
                            stub,
                        )
                except grpc.RpcError as e:
                    if self._events_stop.is_set():
                        return
                    if e.code() == grpc.StatusCode.UNIMPLEMENTED:
                        print("Agent does not send failure events, upgrade the agent to receive them")
                        return
                    print(f"Failure event stream ended ({e.code()}), reconnecting in {RECONNECT_WAIT_SECONDS} seconds...")
                self._events_stop.wait(RECONNECT_WAIT_SECONDS)
        finally:
            channel.close()

    def _handler_context(self, args, timeout_ms: float = 0):
        return HandlerContext(self.cortex_stub, args, timeout_ms)

    def _submit_handler(self, handler_to_invoke, invoke):
        # Blocks once every worker is busy. That is deliberate: the agent then
        # sees its own queue fill rather than this process growing without
        # bound, and the dispatch stream stays readable up to that point.
        self._handler_slots.acquire()
        _, _, expired = _invoke_timeout(invoke, time())
        if expired:
            # The agent has already reported this one as failed; running it
            # now would duplicate work a failure hook may be replaying.
            self._handler_slots.release()
            print(
                f"Dispatch handler SKIPPED {invoke.handler_name} (id={invoke.invocation_id}): "
                "expired while waiting for a free handler"
            )
            return
        stub = self.agent_stub
        try:
            future = self._executor.submit(
                self._invoke_handler, handler_to_invoke, invoke, stub
            )
        except BaseException:
            self._handler_slots.release()
            raise
        future.add_done_callback(lambda _: self._handler_slots.release())

    def _invoke_handler(self, handler_to_invoke, invoke, stub):
        print(
            f"Dispatch handler START {invoke.handler_name} (id={invoke.invocation_id}) reason={invoke.reason}"
        )
        handler_err = None
        handler_result = None
        failure = None
        result = None
        info = InvocationInfo.from_proto(invoke)
        now = datetime.now()
        timeout, capped, _ = _invoke_timeout(invoke, time())
        ctx = self._handler_context(invoke.args, timeout * 1000 if timeout else 0)
        duration_ms = 0

        # A Python thread can't be stopped, so at the deadline the timer
        # reports the timeout itself and the handler's eventual result is
        # dropped. Whichever finishes first claims the invocation.
        finish_lock = threading.Lock()
        finished = False

        def claim():
            nonlocal finished
            with finish_lock:
                if finished:
                    return False
                finished = True
                return True

        def on_timeout():
            if not claim():
                return
            source = FailureSource.APPLICATION
            message = f"handler did not finish within {invoke.timeout_ms}ms"
            if ctx.blocked_on_api():
                source = FailureSource.RUNTIME
                message += " while waiting on a Cortex API call"
            elif capped:
                source = FailureSource.RUNTIME
                message = "invocation waited for a free handler and ran out of time"
            print(f"Dispatch handler TIMEOUT {invoke.handler_name} (id={invoke.invocation_id}): {message}")
            self._report(stub, cortex_axon_agent_pb2.ReportInvocationRequest(
                handler_invoke=invoke,
                start_client_timestamp=now,
                duration_ms=invoke.timeout_ms,
                error=common_pb2.Error(code="timeout", message=message),
                logs=list(ctx.logs),
            ))
            self._run_failure_hook(
                info, Failure(reason=FailureReason.TIMEOUT, source=source, message=message), stub
            )

        timer = None
        if timeout:
            timer = threading.Timer(timeout, on_timeout)
            timer.daemon = True
            timer.start()

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
            if ctx.blocked_on_api():
                failure = Failure(
                    reason=FailureReason.TIMEOUT,
                    source=FailureSource.RUNTIME,
                    message=f"Cortex API call did not finish within the {invoke.timeout_ms}ms timeout: {e}",
                )
            else:
                failure = Failure(
                    reason=FailureReason.HANDLER_ERROR,
                    source=FailureSource.APPLICATION,
                    message=str(e),
                )

        if timer:
            timer.cancel()
        if not claim():
            print(f"Dispatch handler {invoke.handler_name} (id={invoke.invocation_id}) finished after its timeout, result dropped")
            return

        invoke_info = cortex_axon_agent_pb2.ReportInvocationRequest(
            handler_invoke=invoke,
            start_client_timestamp=now,
            duration_ms=duration_ms,
            result=handler_result,
            error=handler_err,
            logs=ctx.logs,
        )

        self._report(stub, invoke_info)

        # After the report, so a slow hook can't make the agent decide this
        # invocation was never answered and report the failure a second time.
        if failure is not None:
            self._run_failure_hook(info, failure, stub)

    def _report(self, stub, invoke_info):
        # A failure here must not escape the worker: the dispatch loop is no
        # longer in the call path, so an exception would be swallowed by the
        # future and the invocation would look like it vanished.
        try:
            report_response = stub.ReportInvocation(invoke_info)
            if report_response.error and report_response.error.code:
                print(
                    f"Error reporting invocation: {report_response.error}"
                )
        except Exception as e:
            print(
                f"Error reporting invocation {invoke_info.handler_invoke.invocation_id}: {e}"
            )
            traceback.print_exc()

    def run(self):
        self._start_failure_events()
        try:
            self._run_dispatch()
        finally:
            self._stop_failure_events()

    def _run_dispatch(self):
        registered = False
        while True:
            stub = self._get_stub()
            try:
                if not registered:
                    self._register_handlers()
                    registered = True

                initial_message = cortex_axon_agent_pb2.DispatchRequest(
                    dispatch_id=self.id,
                    client_version=AGENT_VERSION,
                    failure_events=self.on_failure is not None,
                )
                for response in stub.Dispatch(
                    _client_message_iterator(initial_message)
                ):

                    if response.type == cortex_axon_agent_pb2.DISPATCH_MESSAGE_INVOKE:
                        invoke = response.invoke
                        handler_to_invoke = self._find_handler(
                            invoke.handler_name)
                        if not handler_to_invoke:
                            print(f"Unknown handler: {response.handler_name}")
                            continue
                        self._submit_handler(handler_to_invoke, invoke)
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

    def __init__(self, api: cortex_api_pb2_grpc.CortexApiStub, args: dict, timeout_ms: int = 0):
        self.api = api
        self.args = args
        self.logs = []
        self.obj = {}
        # Without a deadline, a Cortex API call that never returns holds its
        # worker after the agent has given up on the invocation, and once
        # every worker is held the client stops taking new invocations.
        self.deadline = monotonic() + timeout_ms / 1000 if timeout_ms > 0 else None
        # Whether a Cortex API call was running, or ran out of time, when the
        # deadline hit, which makes the timeout Axon's fault rather than the
        # handler's.
        self._api_in_flight = 0
        self._api_timed_out = False

    def blocked_on_api(self) -> bool:
        return self._api_in_flight > 0 or self._api_timed_out

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
        timeout = None
        if self.deadline is not None:
            timeout = max(self.deadline - monotonic(), 0)
        self._api_in_flight += 1
        try:
            response = self.api.Call(request, timeout=timeout)
        except grpc.RpcError as e:
            if e.code() == grpc.StatusCode.DEADLINE_EXCEEDED:
                # Set before _api_in_flight drops, so a timeout check never
                # sees neither.
                self._api_timed_out = True
            raise
        finally:
            self._api_in_flight -= 1
        return CortexResponse(response.status_code, response.body, response.headers)
