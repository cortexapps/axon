# Cortex Axon Relay

Relay allows customers to connect their internally-visible services (such as GitHub, Gitlab, Sonarqube, etc) to the cloud-hosted Cortex without opening any firewall ports.

This allows:

* Integrating data from internally-hosted services and systems
* Configuring integrations without exposing private API keys outside of the internal network

## Running Axon Relay

## Accessing internal services with Axon Relay

With Axon you can use your internal Github installation to access your internal services. The Cortex Axon agent will contact the Cortex cloud and establish a secure connection, which will be then used to route requests from there to your internal services.

### Setting up the Cortex Relay configuration 

1. Go to your Cortex [GitHub settings page](https://app.getcortexapp.com/admin/settings/github) 
2. Choose "Add Configuration", then "Relay".  Choose your alias name, for example `github-relay` or any unused name.
3. Click "Save"
4. On your machine, create a file called `.env` with the following contents, assuming you are using the public Github API:

```
CORTEX_API_TOKEN=your_cortex_token
GITHUB_TOKEN=your_github_token
```

For a GitHub App token, you'll need to set the following, using your GitHub host name:


Now you're ready to run the agent!  Create a file called `docker-compose.yml` with the following contents:

```yaml
services:
  axon:
    image: ghcr.io/cortexapps/cortex-axon-agent:latest
    env_file: .env
    env:
      - GITHUB_API=api.github.com
      - GITHUB_GRAPHQL=api.github.com/graphql
    command: [
      "relay",
      "-i", "github",
      "-a", "github-relay", # this is the alias you set up in the Cortex UI

      # if you are using a Github App token, add the following line
      # "-s", "app",
    ]
```

Note if you are using a private Github App installation (subtype `app`), you'll need to set the `GITHUB_API` and `GITHUB_GRAPHQL` to your internal Github API endpoints, for example:

```
GITHUB_API=https://github.mycompany.com/api/v3
GITHUB_GRAPHQL=https://github.mycompany.com/api/graphql
```

Now run `docker compose up` and you should see the agent start up and connect to Cortex, and be ready to handle traffic.

To test it, go to the settings page for your integration and push "Test Configurations". If you watch the logging output you should see the agent receive the request and forward it to your internal service, and the Cortex Integrations Settings page will show success.

You can see the list of built in file types [here](agent/server/snykbroker/accept_files), which will show the variables needed to execute. If an environment variable
is not found, the start will fail with an error message indicating the missing variable name.

Generally the naming works like:

* `*_API` - the root URL for the API, such as `https://api.github.com`
* `*_API` - just the host and root path for the integration such as `mycompany.github.com/api/v3`
* `*_HOST` - just the domain name, such as `mycompany.github.com`


### Environment Variables Summary

| Integration    | Environment Variables                                                                                               |
|----------------|---------------------------------------------------------------------------------------------------------------------|
| **GitHub**     | `GITHUB_API=api.github.com`, `GITHUB_GRAPHQL=api.github.com/graphql`, `GITHUB_TOKEN`                |
| **GitHub App** | Arg `-s app`; `GITHUB_API=https://myapp.github.com/api/v3`, `GITHUB_GRAPHQL=https://myapp.github.com/api/graphql`, `GITHUB_TOKEN` |
| **Prometheus** | `PROMETHEUS_API=http://mycompany.prometheus.internal`, `PROMETHEUS_USERNAME`, `PROMETHEUS_PASSWORD`                 |
| **Gitlab**     | `GITLAB_API=https://gitlab.com`, `GITLAB_TOKEN`                                                                     |
| **Sonarqube**  | `SONARQUBE_API=https://sonarqube.mycompany.com`, `SONARQUBE_TOKEN`                                                 |
| **Bitbucket Cloud**  | `BITBUCKET_API=https://api.bitbucket.org`, `BITBUCKET_TOKEN`                    |
| **Bitbucket Hosted**  | `BITBUCKET_API=https://bitbucket.mycompany.com`, `BITBUCKET_USERNAME`, `BITBUCKET_PASSWORD`                |

## How it works

Internally, Cortex Axon uses an open-source project published by Snyk called [Snyk Broker](https://docs.snyk.io/enterprise-setup/snyk-broker). 

Snyk Broker is a connector that uses websockets to create a secure tunnel between the internal network and the cloud-hosted Cortex. As the tunnel is initiated from the internal network, so no firewall ports need to be opened.

The flow is based on your Cortex API key, is used to authenticate the agent and secure the connection process.

1. On the Cortex side, the integration is registered in the integration configuration settings page, with an "alias" name that you choose, such as `github-relay`.
2. The Cortex Axon Docker container is started with the your Cortex API key, the integration type (e.g. `github`), and the alias name that you chose. 
3. The Cortex Axon agent connects to the Cortex service, authenticates, and registers itself with the integration type and alias name.
4. The agent starts an instance of the `snyk-broker` (client) process and uses configuration infromatio from the `/register` call to connect to the Cortex backend instance of the `snyk-broker` server.

Once this is established, it is a robust connection that allows API calls made on the Cortex side to be relayed to the internal network, and the responses to be relayed back to the Cortex service.


```mermaid
graph TD
   
    CortexAxonAgent -->|Registration| CortexService

    SnykBrokerServer -->|Github API Calls|SnykBrokerClient
    SnykBrokerClient -->|Github API Calls|InternalGithub
    CortexService -->|Github API Calls|SnykBrokerServer

    subgraph Cortex-Cloud
        CortexService        
        SnykBrokerServer
    end

    subgraph Customer-Network
        subgraph CortexAxonAgent
            SnykBrokerClient
        end
        InternalGithub
    end

    style Cortex-Cloud fill:#93f, color:white
    style CortexAxonAgent fill:#93f
    style Customer-Network fill:#070
    style SnykBrokerServer fill:#ffbf00,color:black
    style SnykBrokerClient fill:#ffbf00,color:black
    style InternalGithub fill:#39f
```


### Running the agent in Kubernetes

To run the agent in Kubernetes, you'll need to create a Deployment that runs the agent with similar configuration above:

* A `Deployment` with the `ghcr.io/cortexapps/cortex-axon-agent:latest` image. 1GB of memory and 1CPU should be sufficient.
* Enviroment variables or a `ConfigMap` for `GITHUB_API` and `GITHUB_GRAPHQL`
* Secrets for
    * `CORTEX_API_TOKEN`
    * `GITHUB_TOKEN`
    
Here is an example to get started with:

```yaml

apiVersion: v1
kind: ConfigMap
metadata:
  name: github-config
data:
  GITHUB_API: api.github.com # or your internal Github API
  GITHUB_GRAPHQL: api.github.com # or your internal Github GraphQL API, minus /graphql
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cortex-axon-agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app: cortex-axon-agent
  template:
    metadata:
      labels:
        app: cortex-axon-agent
    spec:
      containers:
      - name: cortex-axon-agent
        image: ghcr.io/cortexapps/cortex-axon-agent:latest
        resources:
          limits:
            memory: "2Gi"
            cpu: "1"
        args:
          - relay
          - -i
          - github
          - -a
          - github-relay
        envFrom:
        - configMapKeyRef:
          name: github-config
        - secretRef:
          name: relay-secrets # this will need to be created to store your Cortex and Github keys
```



