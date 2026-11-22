import json

from cortex_axon import FailureAction
from cortex_axon.axon_client import AxonClient, HandlerContext
from cortex_axon.handler import cortex_handler, cortex_scheduled


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


@cortex_scheduled(cron="* * * * *", run_now=False)
def my_cron_handler(context: HandlerContext):
    context.log("Cron handler called!")


@cortex_handler()
def my_invoke_handler(context: HandlerContext) -> str:
    context.log("Invoke handler called!")
    result = {
        "status": "success",
    }
    return json.dumps(result)


def on_failure(info, failure):
    # Runs for every failed invocation, including ones only the agent sees.
    print(
        f"FAILED {info.handler_name} (id={info.invocation_id}, attempt={info.attempt}) "
        f"reason={failure.reason.name} source={failure.source.name}: {failure.message}"
    )
    # A real hook would save info.args somewhere it can be replayed from.
    if info.attempt < 3:
        return FailureAction.RETRY
    return FailureAction.DROP


def run():
    # Connect to the gRPC server
    client = AxonClient(
        scope=globals(),
        on_failure=on_failure,
    )
    client.run()


if __name__ == "__main__":
    run()
