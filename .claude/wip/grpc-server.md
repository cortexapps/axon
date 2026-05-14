# Standadlone GRPC Server

Goal: replace snyk-broker with a native-Go implemented Grpc streaming interface for tunneling HTTP traffic.

## Current architecture

This project currently communicates with the Cortex background by hosting an instance of the snyk-broker component.  This connects to another snyk-broker in the Cortex infrastructure via a websocket.  It's only functions are to:

* Initiate a two-way websocket tunnel by reaching out to the server
* Maintain the health of this tunnel
* Serve as a dumb pipe for HTTP traffic from the server side to these agent instances

So the flow is:

- Client [Axon] initializes `snyk-broker`
- `snyk-broker` contacts the server side (e.g. https://relay.cortex.io) and establises a websocket tunnel
- Server side then can dispatch HTTP calls that come through the tunnel then are executed by Axon inside the customer's network
- Responses are then played back through the tunnel.

On the server side, snyk-broker interfaces with an HTTP server called the `BROKER_SERVER` which it communicates a set of operations to:

- server-connected / deleted: a server-side snyk-broker instance is registering itself with the broker server
- client-connected / deleted: a client-side instance has regsered with the server-side snyk-broker and this information is sent to the broker-serve.  this includes a BROKER_TOKEN which can be used to route traffic back to a specific client instance

On the server side, the BROKER_SERVER also supports a dispatching operation like:

`GET http://broker-server:8080/broker/$token/some/path`

The broker server then takes the token and determines which snyk-broker server instance owns that connection. It then envelopes the HTTP request, and that is sent over a websocket to the client side. The client side then compares the path and method to it's accept.json file and uses that information to either reject the call (no matching config) or rewrite it to a local call, then tunnel the response back.

### Problems with this

- There is a lot of code in snyk-broker we don't care about.  We only want the HTTP tunneling, none of the other stuff
- Snyk-broker is written in node which complicates development and installation
- The node websocket stack is complicated and we've seen very fragile.  The semantics between server and client have been difficult to get right and there are a lot of failure modes that have been difficult to anticipate.

## New plan

Rather than take on this complexity, I'd like to instead move to an exactly compatable system that is written in Go and GRPC with the following high level architecture:

- build a set of protobuf service files that define the flow between client and server
- in the axon project add a new root folder called server that implements the server side
- in the axon /agent folder we implement a new client to talk to this server. based on a flag we will instantiate either that or the existing snyk relay_instance_manager.  ideally this is a very abstract interface so most of axon has no idea which we have injected
- this should be designed for durability of connection
  - one of the message types is "heartbeat" and both sides regularly send the other a heartbeat to validate a working tunnel
  - when a side can't heartbeat it should aggressively try to establish a new tunnel
  - client side should support multiple  (probably 2) of these running concurrently eg if one tunnel has problems, can switch to a healthy tunnel, kill the exisitng one and re-establish.  since server side is sticky hopefully this will allow connecting to multiple remote heads.
  - goal: we should never need to restart client instances to get them to reconnect, they should know they are in a disconnected state
- should support bearer authentication, starting with expecting a valid non-expired JWT signed by cortex.  this should be optional to start with and the server side should support specifying a JWT public secret file for validating the JWT against.  we don't need to protect all traffic but ideally we can require a valid cortex token.
- need to add a new server/docker/Dockerfile for building just the server component.
- the server side should be able to safely run any number of server components.
- the server side should emit prometheus metrics for it's primary operations. It should use uber/tally as the main interface for emitting metrics from code, backed by a prometheus recorder.
- the server side should emit structured zap JSON logging.

### Investigation

- the client routing side should support the existing accept.json format.  See examples in agent/server/snykbroker/accept_files for the format to handle.  This stack will replace the snyk-broker and reflector pieces of the exisitng architecture.


- please investigate the BROKER_SERVER interface here: https://github.com/cortexapps/snyk-broker/blob/16805ee1f3318c783df7ed35085ec9aa941bff6e/lib/server/infra/dispatcher.ts#L178.  we want the server to support interacting with a server that supports this interface, given a hostport eg BROKER_SERVER_URL
- In https://github.com/cortexapps/snyk-broker/ undertand the usage of the BROKER_TOKEN raw and hashed versions in the API.  Each server instance will need to keep track of it's client connections and the raw and hashed token for each.
- We want to be very paranoid about how to deal with problems, for example if the GCP load balancer doesn't have long enough TTLs can we recognize infrastructure closing our ports; can we handle the server side instances rolling, etc.