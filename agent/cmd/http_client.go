package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	gohttp "net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
)

func createHttpTransport(config config.AgentConfig, logger *zap.Logger) *gohttp.Transport {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: config.HttpDisableTLS,
	}

	// ensure no proxy has localhost
	if hasProxy() {
		noProxy := os.Getenv("NO_PROXY")
		settings := strings.Split(noProxy, ",")
		if !strings.Contains(noProxy, "localhost") {
			settings = append(settings, "localhost")
		}
		if !strings.Contains(noProxy, "127.0.0.1") {
			settings = append(settings, "127.0.0.1")
		}
		newNoProxy := strings.Join(settings, ",")
		os.Setenv("NO_PROXY", newNoProxy)
	}

	// Load custom CA cert if provided
	var caPEM []byte

	appendFile := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			panic(fmt.Errorf("error reading CA cert file %s: %v", path, err))
		}
		logger.Info("Found custom CA cert", zap.String("path", path))
		caPEM = append(caPEM, data...)
	}

	if config.HttpCaCertFilePath != "" {
		logger.Info("CA_CERT_PATH set, looking for cert files", zap.String("path", config.HttpCaCertFilePath))
		stat, err := os.Stat(config.HttpCaCertFilePath)
		if err != nil {
			panic(fmt.Errorf("error checking CA cert file %s: %v", config.HttpCaCertFilePath, err))
		}
		if stat.IsDir() {
			files, err := filepath.Glob(path.Join(config.HttpCaCertFilePath, "*.pem"))
			if err != nil {
				panic(fmt.Errorf("error reading CA cert directory %s: %v", config.HttpCaCertFilePath, err))
			}
			for _, file := range files {
				appendFile(file)
			}
		} else {
			appendFile(config.HttpCaCertFilePath)
		}
	}

	if len(caPEM) > 0 {

		if config.HttpDisableTLS {
			panic("Cannot use custom CA cert with TLS verification disabled")
		}

		roots := x509.NewCertPool()
		if ok := roots.AppendCertsFromPEM(caPEM); ok {
			tlsConfig.RootCAs = roots
			tlsConfig.InsecureSkipVerify = false
		}
	}

	return &gohttp.Transport{
		Proxy:           gohttp.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
	}
}

func createHttpClient(config config.AgentConfig, transport *gohttp.Transport) *gohttp.Client {

	return &gohttp.Client{
		Transport: transport,
	}
}

func hasProxy() bool {
	proxy := os.Getenv("HTTP_PROXY")
	if proxy == "" {
		proxy = os.Getenv("HTTPS_PROXY")
	}
	return proxy != ""
}
