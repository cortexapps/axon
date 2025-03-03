# Cortex Axon Python Example

## Description

This example demonstrates how to use the Cortex Axon Python API to query weather data from the OpenWeatherMap API.

It does two things:

1. Creates a hierarchy of domains that represent continents and countries, and a set of cities parented by each country. It does this using the `PATCH /api/v1/open-api` API.
2. For each of those cities, it fetches current weather data hourly and updates custom data on the city domain with the weather data. It does this using the `PUT /api/v1/catalog/custom-data` API.

## Setup

First you need to run the agent in dry run mode

```
docker run -e "DRYRUN=true" -p "50051:50051" ghcr.io/cortexapps/cortex-axon-agent:latest
```

To run the sample in `DRYRUN`

```bash
python3 -m venv venv
source venv/bin/activate
pip3 install -r requirements.txt
python3 main.py
```

This will run your app and print out what it would send to the cortex app.

### Running against Cortex

To run it for real, you need to add your cortex token:

```
docker run -e "CORTEX_API_TOKEN=$CORTEX_API_TOKEN" -p "50051:50051" ghcr.io/cortexapps/cortex-axon-agent:latest
```

This will make live updates to your Cortex instance specified by the token.