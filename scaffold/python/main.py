from cortex_axon.handler import cortex_scheduled, cortex_webhook
from cortex_axon.axon_client import AxonClient

@cortex_webhook(id="my-webhook-1")
def my_webhook_handler(context):
    context.log("Success! Webhook handler called!")
    body = context.args["body"]
    context.log(f"Webhook body: {body}")

@cortex_scheduled(interval="1s", run_now=True)
def my_handler(context):

    # Example of calling Cortex API PUT /api/v1/catalog/custom-data,
    # format is like
    #
    #  {
    #   "values": {
    #     "my-service": [
    #       {
    #         "key": "exampleKey1",
    #         "value": "exampleValue1"
    #       },
    #       {
    #         "key": "exampleKey2",
    #         "value": "exampleValue2"
    #       }
    #     ]
    #   }


    # payload = {
    #     "values": {
    #         "my-service": [
    #             {
    #                 "key": "exampleKey1",
    #                 "value": "exampleValue1",
    #             },
    #                             {
    #                 "key": "exampleKey2",
    #                 "value": "exampleValue2",
    #             },
    #         ]
    #     }
    # }

    # json_payload = json.dumps(payload)

    # response = ctx.cortex_api_call(
    #         method="PUT",
    #         path="/api/v1/catalog/custom-data",
    #         body=json_payload,
    # )

    # if response.status_code >= 400:
    #     ctx.log(f"SetCustomTags error: {response.body}", level="ERROR")
    #     exit(1)

    # ctx.log("CortexApi PUT custom-data called successfully!")
    context.log("Success! Handler called!")


def run():
    print("Starting {{.ProjectName}}")
    # Connect to the gRPC server
    client = AxonClient(scope=globals())
    client.run()


if __name__ == '__main__':
    run()
