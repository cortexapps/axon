from dataclasses import dataclass, field
from enum import Enum, IntEnum
from typing import Any, Callable, Optional

from generated import cortex_axon_agent_pb2


class FailureReason(IntEnum):
    UNSPECIFIED = cortex_axon_agent_pb2.FAILURE_REASON_UNSPECIFIED
    # The handler raised.
    HANDLER_ERROR = cortex_axon_agent_pb2.HANDLER_ERROR
    # The invocation did not finish within its timeout.
    TIMEOUT = cortex_axon_agent_pb2.TIMEOUT
    # The invocation never ran: the agent's queue was full, or it expired
    # waiting for a free handler.
    NO_RESOURCES = cortex_axon_agent_pb2.NO_RESOURCES


class FailureSource(IntEnum):
    UNSPECIFIED = cortex_axon_agent_pb2.FAILURE_SOURCE_UNSPECIFIED
    # Handler code failed.
    APPLICATION = cortex_axon_agent_pb2.APPLICATION
    # Axon failed: the agent, the SDK, or a Cortex API call.
    RUNTIME = cortex_axon_agent_pb2.RUNTIME


class PostAction(Enum):
    # Leave the failure as it is.
    IGNORE = "ignore"
    # Queue the invocation again as its next attempt.
    RETRY = "retry"


@dataclass(frozen=True)
class InvocationInfo:
    """Identifies one invocation. For a webhook, args holds the inbound
    message: "body", "content-type" and "url"."""

    invocation_id: str
    handler_name: str
    invoke_type: int
    args: dict[str, str]
    # 1 for the first delivery, counting up with each retry.
    attempt: int
    invoke: Any = field(repr=False, compare=False)

    @classmethod
    def from_proto(cls, invoke: cortex_axon_agent_pb2.DispatchHandlerInvoke) -> "InvocationInfo":
        return cls(
            invocation_id=invoke.invocation_id,
            handler_name=invoke.handler_name,
            invoke_type=invoke.reason,
            args=dict(invoke.args),
            attempt=max(invoke.attempt, 1),
            invoke=invoke,
        )


@dataclass(frozen=True)
class Failure:
    reason: FailureReason
    source: FailureSource
    message: str
    # Whoever sent the invocation was told it was not accepted (a webhook
    # sender got a 503), so it may arrive again.
    caller_rejected: bool = False

    @classmethod
    def from_proto(cls, failure: cortex_axon_agent_pb2.InvocationFailure) -> "Failure":
        return cls(
            reason=FailureReason(failure.reason),
            source=FailureSource(failure.source),
            message=failure.message,
            caller_rejected=failure.caller_rejected,
        )


# Runs before each handler.
PreDispatchHook = Callable[[InvocationInfo], None]

# Runs after each invocation with failure=None on success, and for every
# failure the agent detects on its own, such as an invocation that never
# reached this process. Returning None means PostAction.IGNORE; the return
# value only matters for a failure.
PostDispatchHook = Callable[[InvocationInfo, Optional[Failure]], Optional[PostAction]]
