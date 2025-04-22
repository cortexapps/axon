import functools
import inspect
from abc import abstractmethod

from generated import (
    cortex_axon_agent_pb2,
)


class CortexAnnotation:
    def __init__(self, timeout_ms: int = 0):
        self.func = None
        self.name = None
        self.timeout = timeout_ms

    def set_func(self, func):
        if self.func and self.func != func:
            raise ValueError("Cannot set func twice")
        self.name = func.__name__
        self.func = func

    def __call__(self, *args, **kwargs):
        if not self.func:
            raise ValueError("Handler not initialized")
        return self.func(*args, **kwargs)

    def __get__(self, instance, owner):
        return self.func.__get__(instance, owner)

    @abstractmethod
    def get_handler_options(self) -> list[cortex_axon_agent_pb2.HandlerOption]:
        pass


class CortexScheduled(CortexAnnotation):
    def __init__(
        self,
        # interval is a string that represents a time duration, eg "5s" or "3h"
        interval: str = None,
        cron: str = None,  # cron is a string that represents a cron expression, eg "* * * * *"
        # run_now is a boolean that determines if the handler should be run immediately
        run_now: bool = True,
        # timeout_ms is an integer that represents the timeout in milliseconds
        timeout_ms: int = 0,
    ):
        super().__init__(timeout_ms)
        self.interval = interval
        self.cron = cron
        self.run_now = run_now

    def get_handler_options(self) -> list[cortex_axon_agent_pb2.HandlerOption]:
        options = []
        if self.interval:
            options.append(
                cortex_axon_agent_pb2.HandlerOption(
                    invoke=cortex_axon_agent_pb2.HandlerInvokeOption(
                        type=cortex_axon_agent_pb2.RUN_INTERVAL, value=self.interval
                    )
                )
            )
        if self.cron:
            options.append(
                cortex_axon_agent_pb2.HandlerOption(
                    invoke=cortex_axon_agent_pb2.HandlerInvokeOption(
                        type=cortex_axon_agent_pb2.CRON_SCHEDULE, value=self.cron
                    )
                )
            )
        if self.run_now:
            options.append(
                cortex_axon_agent_pb2.HandlerOption(
                    invoke=cortex_axon_agent_pb2.HandlerInvokeOption(
                        type=cortex_axon_agent_pb2.RUN_NOW,
                    )
                )
            )
        return options


class CortexWebhook(CortexAnnotation):
    def __init__(self, id: str, timeout_ms: int = 0):
        super().__init__(timeout_ms)
        self.id = id

    def get_handler_options(self) -> list[cortex_axon_agent_pb2.HandlerOption]:
        return [
            cortex_axon_agent_pb2.HandlerOption(
                invoke=cortex_axon_agent_pb2.HandlerInvokeOption(
                    type=cortex_axon_agent_pb2.WEBHOOK, value=self.id
                )
            )
        ]


class CortexHandler(CortexAnnotation):
    def __init__(self, timeout_ms: int = 0):
        super().__init__(timeout_ms)

    def get_handler_options(self) -> list[cortex_axon_agent_pb2.HandlerOption]:
        return [
            cortex_axon_agent_pb2.HandlerOption(
                invoke=cortex_axon_agent_pb2.HandlerInvokeOption(
                    type=cortex_axon_agent_pb2.INVOKE
                )
            )
        ]


def cortex_handler(*a, **k):
    handler = CortexHandler()
    name = cortex_handler.__name__

    def real_decorator(f):
        @functools.wraps(f)
        def wrapper(*args, **kwargs):
            retval = f(*args, **kwargs)
            return retval

        existing = getattr(f, name, None)
        if existing:
            raise ValueError(f'Attribute type ${name} already exists')
        setattr(wrapper, name, handler)
        return wrapper

    return real_decorator


def cortex_scheduled(*a, **k):

    interval = k.get("interval")
    cron = k.get("cron")
    run_now = k.get("run_now", True)
    name = cortex_scheduled.__name__

    handler = CortexScheduled(
        interval=interval, cron=cron, run_now=run_now
    )

    def real_decorator(f):
        @functools.wraps(f)
        def wrapper(*args, **kwargs):
            retval = f(*args, **kwargs)
            return retval

        existing = getattr(f, name, None)
        if existing:
            raise ValueError(f'Attribute type ${name} already exists')
        setattr(wrapper, name, handler)
        return wrapper

    return real_decorator


def cortex_webhook(*a, **k):
    id = k.get("id")
    handler = CortexWebhook(id=id)
    name = cortex_webhook.__name__

    def real_decorator(f):
        @functools.wraps(f)
        def wrapper(*args, **kwargs):
            retval = f(*args, **kwargs)
            return retval

        existing = getattr(f, name, None)
        if existing:
            raise ValueError(f'Attribute type ${name} already exists')
        setattr(wrapper, name, handler)
        return wrapper

    return real_decorator

handler_types =[cortex_scheduled, cortex_webhook, cortex_handler] 

def find_annotated_methods(scope: dict = None):
    handlers = []
    for _, func in scope.items():
        if not inspect.isfunction(func):
            continue
    
        for type in handler_types:
            h = getattr(func, type.__name__, None)
            if isinstance(h, CortexAnnotation):
                h.set_func(func)
                handlers.append(h)
                break
        
    return handlers
