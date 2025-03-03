package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/cortexapps/axon-go"
	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"go.uber.org/zap"
)

func main() {

	// create our agent client and register a handler
	agentClient := axon.NewAxonAgent()

	// this handler will be invoked every 1 second
	_, err := agentClient.RegisterHandler(myExampleIntervalHandler,
		axon.WithTimeout(time.Minute),
		axon.WithInvokeOption(
			pb.HandlerInvokeType_RUN_INTERVAL, "1s",
		),
	)

	_, err = agentClient.RegisterHandler(myExampleWebhooklHandler,
		axon.WithInvokeOption(
			pb.HandlerInvokeType_WEBHOOK, "my-webhook-id",
		),
	)

	if err != nil {
		log.Fatalf("Error registering handler: %v", err)
	}

	// Start the run process.  This will block and stream invocations
	ctx := context.Background()
	agentClient.Run(ctx)

}

// Here we have our example handler that will be called every one second
func myExampleIntervalHandler(ctx axon.HandlerContext) interface{} {

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

	_, err = ctx.CortexJsonApiCall("PUT", "/api/v1/catalog/custom-data", string(json))
	if err != nil {
		ctx.Logger().Error("Error calling cortex api", zap.Error(err))
	}

	ctx.Logger().Info("Success! myExampleIntervalHandler handler called!")
	return nil
}

// Here we have our example handler that will be called every one second
func myExampleWebhooklHandler(ctx axon.HandlerContext) interface{} {

	body := ctx.Args()["body"]
	contentType := ctx.Args()["content-type"]

	ctx.Logger().Info("Hello from myExampleIntervalHandler!", zap.String("body", body), zap.String("content-type", contentType))
	return nil
}
