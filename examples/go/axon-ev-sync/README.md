# Cortex Axon Go Example

## Description

This example demonstrates how to use the Cortex Axon Go API to publish custom data into Cortex.

This sample uses Washington State DOT data for electric vehicles as the data source (see details [here](https://data.wa.gov/Transportation/Electric-Vehicle-Population-Data/f6w7-q2d2/data_preview)), and publishes that into Cortex like:

```
Vehicle-Make (domain)
  - Vehicle-Model (domain)
      - Vehicle (entity type "vehicle")
```

Each vehicle has custom data like it's type (eg "BEV" or "PHEV"), its registration zip code, etc.

## Setup

To try running this, you can run the agent in "dry run" mode, which will output all of the calls it would make to Cortex.

```
docker run -e "DRYRUN=true" -p "50051:50051" ghcr.io/cortexapps/cortex-axon-agent:latest
```

To run the sample in `DRYRUN`

```bash
cd examples/go/axon-ev-sync
go mod tidy
go run main.go
```

This will run your app and print out what it would send to the cortex app to the console of the running Docker container (e.g. NOT the app.)

### Running against Cortex

To run it for real, you need to add your cortex token to the Docker container and restart it.

```
docker run -e "CORTEX_API_TOKEN=$CORTEX_API_TOKEN" -p "50051:50051" ghcr.io/cortexapps/cortex-axon-agent:latest
```

Run the app again. This will make live updates to your Cortex instance specified by the token.