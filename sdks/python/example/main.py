import json

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


    response = ctx.cortex_api_call(
        method="GET",
        path="/api/v1/catalog/aa00-1/custom-data",
    )

    body = getattr(response, "body", None)
    if response.status_code >= 400:
        ctx.log(f"SetCustomTags error: status={response.status_code} {body}", level="ERROR")
        return

    ctx.log(f"SetCustomTags response: {body}", level="INFO")


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


def run():
    # Connect to the gRPC server
    client = AxonClient(
        scope=globals(),
    )
    client.run()


if __name__ == "__main__":
    run()
