import json

from cortex_axon.axon_client import AxonClient, HandlerContext
from cortex_axon.handler import cortex_scheduled


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


def run():
    # Connect to the gRPC server
    client = AxonClient(
        scope=globals(),
    )
    client.run()


if __name__ == "__main__":
    run()
