![CI Status](https://github.com/cortexapps/axon/actions/workflows/ci.yml/badge.svg)

# Cortex Axon
Framework and Toolset for Integrating Your Data into Cortex

## Intro

With Axon you can:

* Allow Cortex to access internal services without opening firewall ports. Learn more [here](#accessing-internal-services-with-axon-relay).
* Quickly and easily write custom jobs that push data into Cortex. Learn more [here](#writing-handlers), either on a schedule, or on a webhook.

Axon is composed of:

* An "agent" which runs in a Docker container (`cortex-axon-agent`)
* SDKs for writing custom handlers, published in Python or Go but you can write one in any language that supports GRPC and Protobuf.  To deploy apps with these SDKs you build a new Docker container based off of `cortex-axon-agent` and include your code.


## Accessing internal services with Axon Relay

With Axon you can use your internal Github installation to access your internal services. The Cortex Axon agent will contact the Cortex cloud and establish a secure connection, which will be then used to route requests from there to your internal services.

For details on Relay, see [here](README.relay.md).

### Setting up the Cortex Relay configuration 

1. Go to your Cortex [GitHub settings page](https://app.getcortexapp.com/admin/settings/github) 
2. Choose "Add Configuration", then "Relay".  Choose your alias name, for example `github-relay` or any unused name.
3. Click "Save"
4. On your machine, create a file called `.env` with the following contents, assuming you are using the public Github API, then launch the Docker container

```
CORTEX_API_TOKEN=your_cortex_token
GITHUB_TOKEN=your_github_token
```

5. Launch the Docker container with the following command: 

```
docker run --env-file .env ghcr.io/cortexapps/cortex-axon-agent:latest relay -i github -a github-relay
```

For details on Relay, see [here](README.relay.md).

## Writing Handlers

Axon also makes it very easy to write sophisicated code that can send date to Cortex either on a regular interval, a cron schedule, or after processing a webhook.

### Setting up a Handler Project in Python

Axon is distibuted via a Docker container, so to set up your first application in Python you can run:

```
docker run -v "$(pwd)$:/src" ghcr.io/cortexapps/cortex-axon-agent:latest init --language python --name my-cortex-app 
```

This will:

1. Pull the Docker container for Axon
2. Initialize a new Python project at `/path/for/your/application/my-cortex-app`

```
$ ls -l /path/for/your/application/my-cortex-app
 436 Mar  5 13:35 Dockerfile
 306 Mar  5 13:35 Makefile
2147 Mar  5 13:35 README.md
1769 Mar  5 13:35 main.py
  91 Mar  5 13:35 requirements.txt
```

By default, the app doesn't do anything but you can start it:

```
cd /path/for/your/application/my-cortex-app
docker build -t my-cortex-app:local .
docker run -e "DRYRUN=true" my-cortex-app:local
```

This will build your app into a Docker container and run it, you will see example output every 1 second.  See the `main.py` file in your project for the code that is running, and you can modify it there.

### Hello World Handler Example

Axon supports writing handlers that can be invoked:

* When the agent is run. Use this for jobs where you want to invoke the agent to run as a Kubernetes Job then exit `@cortex_scheduled(run_now=True)`
* On a schedule interval, say every 6 hours `@cortex_scheduled(interval="6h")`
* On a cron schedule `@cortex_scheduled(cron="2 30 * * *")`
* On a webhook `@cortex_webhook(id="my-webhook-unique-id")`

Let's write an example that calls an API and updates custom tags on some entities:

```python
from cortex_axon import cortex_axon_pb2
from cortex_axon.cortex_axon_pb2_grpc import CortexApiStub
from cortex_axon.handler import cortex_scheduled
from cortex_axon.axon_client import AxonClient


@cortex_scheduled(interval="5s")
def my_handler(ctx: HandlerContext):

    my_data = someInternalService.getProperties(type="attributes")

    # here we imagine the key is the service name, for each of those set
    # the custom tag, eg `PUT /api/v1/catalog/custom-data`

    # build our request payload
    values = {}

    for item in my_data:
        service_name = item.get("service_name")
        service_values = []
        values [service_name] = service_values
        for key, value in item.get("properties").items():
            service_values.append({"key": key, "value": value})        

    payload = {
        "values": values
    }

    json_payload = json.dumps(payload)

    response = ctx.cortex_api_call(
        method="PUT",
        path="/api/v1/catalog/custom-data",
        body=json_payload,
    )
```

### Webhook handlers

For webhook handlers, they look like this:

```python
from cortex_axon import cortex_axon_pb2
from cortex_axon.cortex_axon_pb2_grpc import CortexApiStub
from cortex_axon.handler import cortex_scheduled
from cortex_axon.axon_client import AxonClient


@cortex_webhook(id="my-webhook-unique-id")
def my_webhook(ctx: HandlerContext):
  body = ctx.args["body"]
  url = ctx.args["url"]
  content_type = ctx.args["content-type"]

  # handle your webhook here
```

The webhook server can be acccessed on port 8081 of the Axon Agent, so if you were to register this webhook with another system you would use the URL `http://axon-agent:8081/webhook/my-webhook-unique-id`, assuming your Axon agent Kubernetes service was called `axon-agent` and that it exposed port 8081.

### Debugging your app locally

To iterate on code, it's not convenient to build a Docker container every time. To handle this, you can run the Docker container as the agent, then run your code locally.

```
# create a virtual env
python -m venv venv
. venv/bin/activate

# install dependencies
pip install -r requirements.txt

# start the Axon agent (this will be running in the background)
docker run -d -e "DRYRUN=true" -p "50051:50051" -p "80:80" ghcr.io/cortexapps/cortex-axon-agent:latest serve

# Now, run your app under the debugger...
```

See the [calling the Cortex API](#calling-the-cortex-api) below for more information on the API.

### Calling the Cortex API

Since you've put the `CORTEX_API_TOKEN` in your environment, you can call the Cortex REST API in one of two ways:

1. On your handlers you will find a helper on the the `context` object that's passed in, in the case of Python it's `ctx.cortex_api_call`.  This will make a REST call to the Cortex API, for example:

```python
response = ctx.cortex_api_call(
    method="GET",
    path="/api/v1/catalog/entities",
)
```

2. You can call the Cortex API directly at any time at the address `http://localhost/cortex-api`, e.g. `GET http://localhost/cortex-api/api/v1/catalog/entities`. The Agent will automatically add your `CORTEX_API_TOKEN` to the headers of the request, and handle things like rate limiting.


### Running Live against the Cortex API

When you are ready to invoke Cortex APIs, set the `CORTEX_API_TOKEN` enviornment variable and omit `DRYRUN`. For on-premise installs you'll also need to add `CORTEX_API_BASE_URL` which is the DNS name of your cortex instance eg `https://api.cortex.internal`

## Publishing your code

To publish your app into your environment, you'll need to:

1. Build a docker image using the `Dockerfile` in the project.  This may need to be customized to add more files if needed for the scenario.
2. Publish this image into your Docker registry
3. Build a `Deployment` or `Job` Kubernetes manifest that deploys that container into your environment, configuring secrets for passing the `CORTEX_API_TOKEN`

Here you build your docker container:

```
docker build -t my-cortex-app:latest .
```

Now to execute this, you run it as above but simply:

```
export CORTEX_API_TOKEN=your_token
docker run -e "CORTEX_API_TOKEN=$CORTEX_API_TOKEN" my-cortex-app:latest
```

Note this will run everything inside the same container so you do NOT need to run the separate agent container or publish the ports as with debugging above.

You can then deloy this container into your Kubernetes environment