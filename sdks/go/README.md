# Cortex Axon SDK for Go

This is the official Cortex Axon SDK for Go. It provides a simple way to interact with and extend your Cortex instance.

## Getting started

To run the Cortex SDK you need to get the Axon Agent via Docker:

```
docker pull ghcr.io/cortexapps/cortex-axon-agent:latest
```

Then to scaffold a Go project:

```
docker run -it --rm -v "$(pwd):/src" ghcr.io/cortexapps/cortex-axon-agent:latest init --language go my-go-axon project
```

This will create a new directory `my-go-axon` with a Go project scaffolded in the current directory.

## Running locally

To run your project, first start the agent Docker container like:

```
docker run -it --rm -p "50051:50051" -p "80:80" -e "DRYRUN=true" ghcr.io/cortexapps/cortex-axon-agent:latest serve
```

This is `DRYRUN` mode that prints what it would have called, to run against the Cortex API remove the `DRYRUN` environment variable and add `-e "CORTEX_API_TOKEN=$CORTEX_API_TOKEN`.  Be sure to export your token first, e.g. `export CORTEX_API_TOKEN=your-token`.


## Adding handlers

To add a handler, open `main.go` and create a function:

```go

// Here we have our example handler that will be called every one second
func myExampleIntervalHandler(client pb.CortexApiClient) interface{} {

	// here you would do some operations that then push data to the cortex api
	//
	// JSON payload to send to the Cortex API is like:
	// {
	// 	"values": {
	// 	  "service-tag": [
	// 		{
	// 		  "key": "k1",
	// 		  "value": "v1"
	// 		},
	// 		{
	// 		  "key": "k2",
	// 		  "value": "v2"
	// 		}
	// 	  ],
	// 	}

	payload := map[string]interface{}{
		"values": map[string]interface{}{
			"my-service": []interface{}{
				map[string]string{
					"key":   "my-custom-key",
					"value": "my-custom-value",
				},
			},
		},
	}

	json, err := json.Marshal(payload)
	if err != nil {
		ctx.Logger().Error("Error marshalling json", zap.Error(err))
		return nil
	}

	ctx.CortexJsonApiCall(context.Background(), "PUT","/api/v1/catalog/custom-data", string(json))

	log.Println("myExampleIntervalHandle called!")
	return nil
}

```

Then register it with the agent:

```go

agentClient := axon.NewAxonAgent()

_, err := agentClient.RegisterHandler(myExampleIntervalHandler,
		axon.WithInvokeOption(pb.HandlerInvokeType_RUN_INTERVAL, "1s"),
	)
agentClient.Run(context.Background())
```

Now start the agent in a separate terminal:
```
make run-agent
```

And run your project:
```
go run main.go
```

This will begin executing your handler every second.






## Failure hooks

`WithFailureHook` runs your own code for every failed invocation, including
ones only the agent sees (such as a webhook dropped because every handler was
busy). Use it to send failed messages somewhere you can replay them from.
Every failure goes through the agent: it sends each one to the hook and does
what the hook returns. This needs an agent that supports failure events.

```go
agent := axon.NewAxonAgent(
	axon.WithFailureHook(func(ctx context.Context, info axon.InvocationInfo, failure *axon.Failure) axon.FailureAction {
		// info.Args holds the original webhook: "body", "content-type", "url"
		deadLetters.Publish(info, failure)
		if failure.Source == pb.FailureSource_RUNTIME && info.Attempt < 3 {
			return axon.FailureActionRetry
		}
		return axon.FailureActionDrop
	}),
)
```

`failure.Reason` is `HANDLER_ERROR`, `TIMEOUT` or `NO_RESOURCES`.
`failure.Source` says whether handler code (`APPLICATION`) or Axon
(`RUNTIME`) failed. A timeout during a Cortex API call counts as `RUNTIME`.
`CallerRejected` means the sender was told the message wasn't accepted (a
webhook got a 503). `FailureActionRetry` has the agent queue the invocation
again as its next attempt, up to 5 attempts; `FailureActionDrop` drops it.
