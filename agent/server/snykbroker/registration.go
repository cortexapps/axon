package snykbroker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/cortexapps/axon/common"
	"github.com/cortexapps/axon/config"
)

// Registration is an interface for registering an integration with the Cortex API
// Where the integration uses the Cortex API Key plus the integration+alias combo to
// receive a token and a server URI to be used for snyk-broker
type Registration interface {
	Register(integration common.Integration, alias string) (*RegistrationInfoResponse, error)
}

var ErrUnauthorized = os.ErrPermission

type RegistrationError struct {
	error
	Message    string
	StatusCode int
}

func (e *RegistrationError) Error() string {
	return fmt.Sprintf("registration error: %s (%v)", e.Message, e.error)
}

type registration struct {
	config     config.AgentConfig
	proxyPort  int
	httpClient *http.Client
}

func NewRegistration(config config.AgentConfig, httpClient *http.Client) Registration {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &registration{
		config:     config,
		httpClient: httpClient,
	}
}

func (r *registration) SetProxyPort(port int) {
	r.proxyPort = port
}

func (r *registration) Register(integration common.Integration, alias string) (*RegistrationInfoResponse, error) {
	// Call the Cortex API to get
	//
	// Relay server URL
	// Broker token

	// Define the URL
	target := fmt.Sprintf(
		"%s/api/v1/relay/register",
		r.config.CortexApiBaseUrl,
	)

	reqBody := &registerRequest{
		Integration:   integration,
		Alias:         alias,
		InstanceId:    r.config.InstanceId,
		ClientVersion: common.ClientVersion,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("error marshalling request body: %v", err)
	}

	req, err := http.NewRequest("POST", target, bytes.NewBuffer(jsonBody))
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.config.CortexApiToken))

	resp, err := r.httpClient.Do(req)
	if err == net.ErrClosed {
		return nil, fmt.Errorf("cortex API server not available, check CORTEX_API_BASE_URL")
	}
	if err != nil {
		re := &RegistrationError{error: err, Message: "error making relay registration request"}
		fmt.Println("Error: ", re)

		return nil, &RegistrationError{error: err, Message: "error making relay registration request"}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// Read the body
		rr := &RegistrationInfoResponse{}
		err := json.NewDecoder(resp.Body).Decode(rr)
		if err != nil {
			return nil, fmt.Errorf("error decoding response body: %v", err)
		}
		return rr, nil
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized

	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil && err != io.EOF {
		return nil, &RegistrationError{error: err, Message: "error reading response body"}
	}
	return nil, &RegistrationError{error: fmt.Errorf("unexpected status code: %d", resp.StatusCode), Message: string(respBody), StatusCode: resp.StatusCode}
}

type RegistrationInfoResponse struct {
	ServerUri string `json:"serverUri"`
	Token     string `json:"token"`
}

type registerRequest struct {
	Integration        common.Integration `json:"integration,omitempty"`
	Alias              string             `json:"alias"`
	InstanceId         string             `json:"instanceId"`
	IntegrationSubtype string             `json:"integrationSubtype,omitempty"`
	ClientVersion      string             `json:"clientVersion,omitempty"`
}
