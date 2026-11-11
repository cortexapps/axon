# Cortex Axon SDK for Python

This is the official Cortex Axon SDK for Python. It provides a simple way to interact with and extend your Cortex instance.

## Getting started

To run the Cortex SDK you need to get the Axon Agent via Docker:

```
docker pull ghcr.io/cortexapps/cortex-axon-agent:latest
```

Then to scaffold a Python project:

```
docker run -it --rm -v "$(pwd):/src" ghcr.io/cortexapps/cortex-axon-agent:latest init --language python my-python-axon project
```

This will create a new directory `my-python-axon` with a Go project scaffolded in the current directory.

## Running locally

To run your project, first start the agent Docker container like:

```
docker run -it --rm -p "50051:50051" -p "80:80" -e "DRYRUN=true" ghcr.io/cortexapps/cortex-axon-agent:latest serve
```

This is `DRYRUN` mode that prints what it would have called, to run against the Cortex API remove the `DRYRUN` environment variable and add `-e "CORTEX_API_TOKEN=$CORTEX_API_TOKEN`.  Be sure to export your token first, e.g. `export CORTEX_API_TOKEN=your-token`.


## Adding handlers

To add a handler, open `main.py` and create a function:

```python

@cortex_scheduled(interval="5s")
def my_handler(ctx: HandlerContext):

    payload = {
        "values": {
            "my-service": [
                {
                    "key": "exampleKey1",
                    "value": "exampleValue1",
                },
                {
                    "key": "exampleKey2",
                    "value": "exampleValue2",
                },
            ]
        }
    }

    json_payload = json.dumps(payload)

    response = ctx.cortex_api_call(
        method="PUT",
        path="/api/v1/catalog/custom-data",
        body=json_payload,
    )

    if response.status_code >= 400:
        ctx.log(f"SetCustomTags error: {response.body}", level="ERROR")
        exit(1)

    ctx.log("CortexApi PUT custom-data called successfully!")

@cortex_webhook(id="my-webhook-1")
def my_webhook_handler(context: HandlerContext):
    context.log("Success! Webhook handler called!")
    body = context.args["body"]
    context.log(f"Webhook body: {body}")

```

Now start the agent in a separate terminal:
```
make run-agent
```

And run your project:
```
python main.py
```

This will begin executing your handler every 5 seconds. 
To invoke the webhook handler, you can just send a POST request to `http://localhost:80/webhook/my-webhook-1` with a body.





## Handler concurrency

Handlers run on a thread pool, so a slow handler does not delay the ones
behind it. Eight run at once by default. Once every worker is busy, the SDK
stops reading new invocations until one finishes.

Write handlers to be thread safe, because two invocations of the same handler
can run at the same time. To change the limit:

```python
client = AxonClient(scope=globals(), max_concurrent_handlers=16)
```

Set it to `1` for the old behaviour, where handlers run one at a time.

## Failure hooks

Pass `on_failure` to `AxonClient` to run your own code whenever an
invocation fails, for example to send the failed message somewhere you can
replay it from. Every failure goes through the agent, wherever it happened:
a handler that raised in this process, or a webhook the agent couldn't hand
to this process because every handler was busy. The agent sends each one to
your hook and does what the hook returns. Failures arrive on their own
stream, so they get through even while every handler slot is taken, and a
failure your hook hadn't answered when the connection dropped is sent again.
This needs an agent that supports failure events.

```python
from cortex_axon import AxonClient, FailureAction, FailureSource
from cortex_axon.handler import cortex_webhook


@cortex_webhook(id="my-webhook-1")
def my_webhook_handler(context):
    process(context.args["body"])


def on_failure(info, failure):
    # info.args holds the original webhook: "body", "content-type", "url"
    dead_letters.publish({
        "handler": info.handler_name,
        "invocation_id": info.invocation_id,
        "attempt": info.attempt,
        "reason": failure.reason.name,       # HANDLER_ERROR, TIMEOUT, NO_RESOURCES
        "source": failure.source.name,       # APPLICATION or RUNTIME
        "caller_rejected": failure.caller_rejected,
        "message": info.args,
    })
    # Optionally have the agent run it again, as a new attempt.
    if failure.source == FailureSource.RUNTIME and info.attempt < 3:
        return FailureAction.RETRY


client = AxonClient(scope=globals(), on_failure=on_failure)
client.run()
```

| Reason | When | Source |
|---|---|---|
| `HANDLER_ERROR` | The handler raised | `APPLICATION` |
| `TIMEOUT` | The handler ran past its timeout | `RUNTIME` if a Cortex API call was in flight, the invocation waited for a free handler, or the agent never got a result; otherwise `APPLICATION` |
| `NO_RESOURCES` | The agent's queue was full, or the message expired waiting for a free handler | `RUNTIME` |

`caller_rejected` is true when the sender was told the message wasn't
accepted (a webhook got a 503), so the sender may also retry it.

Returning `FailureAction.RETRY` has the agent queue the invocation again,
with `attempt` incremented; it stops after 5 attempts. Returning
`FailureAction.DROP`, or nothing, drops it. If the hook raises, the
invocation is dropped.

The hook runs for every handler type, not only webhooks. Python can't stop a
handler that overruns its timeout: at the deadline the SDK reports the
`TIMEOUT` and runs the hook, and whatever the handler does after that is
dropped.
