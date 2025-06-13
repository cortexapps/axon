package common

import (
	"os"
)

func ApplyEnv(envVars map[string]string) {

	for k, v := range envVars {
		os.Setenv(k, v)
	}
}
