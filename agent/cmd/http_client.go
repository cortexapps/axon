package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	gohttp "net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/cortexapps/axon/config"
	"github.com/cortexapps/axon/util"
	"go.uber.org/zap"
)

func createHttpTransport(config config.AgentConfig, logger *zap.Logger) *gohttp.Transport {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: config.HttpDisableTLS,
	}

	util.EnsureLocalhostNoProxy(true)

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
