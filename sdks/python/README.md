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




