# Axon Helm Chart

This Helm chart is used to deploy Axon Relay to enable integrating Cortex with internal services.

See details on Axon Relay [here](../../../README.relay.md).

This chart provides a simple and configurable way to manage Axon Relay deployments in Kubernetes.

A single deployment type (e.g. Github) must be enabled by a single deployment of Axon Relay. If you want to deploy multiple Axon Relay types (e.g. Github and Jira), you will need to deploy multiple instances of this chart.

## Configuration

The critical configuration options are:

- `relay.integration`: The type of integration (e.g., `github`, `jira`, etc.).
- `relay.alias`: The alias name for the integration, which must match the alias used in the Cortex UI.
- `relay.env`: Environment variables required for the integration. These should be set in your `values.yaml` file or passed as command-line arguments during installation.


## Installation

1. Create your values file, for example: `values.github.yaml`. Each integration type has a set of variables that need to be set. See [here](../../../README.relay.md#environment-variables-summary) for more details. For example, for GitHub, you would set the following.  
    ```yaml
    relay:
        integration: github
        alias: github-relay
        env:
            GITHUB_API: "https://api.github.com"
            GITHUB_GRAPHQL: "https://api.github.com/graphql"
    ```

2. Configure your secrets. You can use Kubernetes secrets to store sensitive information like API tokens. Create a secret with the required tokens, using the Github example above:
    ```bash
    kubectl create secret generic axon-secrets --from-literal=CORTEX_API_TOKEN=<your_cortex_token> --from-literal=GITHUB_TOKEN=<your_github_token>
    ```

3. Install the chart:
    ```bash
    cd examples/relay/helm-chart
    helm install axon-github . --namespace <namespace> --values values.github.yaml
    ```

    Replace `<namespace>` with the desired Kubernetes namespace.

4. Verify the installation via the Cortex UI. Under Settings >> Integrations, create an integration of the type and alias specified above.  Push the "Test Configuration" button to verify. Note that the deployment configured here will report errors until that is created, then will begin to report success.  You can do these steps in any order, but the integration must be created before the deployment will work.

## Adding Secrets for Additional CA certs

For running in proxy environments, you can set some additional values:

```yaml
proxy:
    server: "http://proxy.mycompany.com:8080" # your proxy server
    disableTLS: true # for debugging only, do not use in production
    certSecretName: axon-ca-cert # name of the Kubernetes secret containing the CA cert files
```

If you use `certSecretName`, the secret must contain a PEM file as a value. The secret will be mounted into the container at `/etc/ssl/axon-certs/`.

The secret should be created like this:

```bash
 kubectl create secret generic axon-ca-cert --from-file=/path/to/my-cert.pem
```

This will then be mounted into the container and picked up by the agent and the Snyk Broker.


## Uninstallation

To uninstall the chart:
```bash
helm uninstall axon-github --namespace <namespace>
```