package common

import (
	"math/rand"
	"os"
)

func ApplyEnv(envVars map[string]string) {

	for k, v := range envVars {
		os.Setenv(k, v)
	}
}

func GetRandomPort() int {
	port := 10000 + rand.Intn(50000-10000)
	if port == 0 {
		port = 10000
	}
	return port
}
