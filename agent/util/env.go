package util

import (
	"os"
	"strings"
)

func SaveEnv(clear bool) map[string]string {
	env := make(map[string]string)
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	if clear {
		os.Clearenv()
	}
	return env
}

func RestoreEnv(env map[string]string) {
	for key, value := range env {
		if value == "" {
			os.Unsetenv(key)
		} else {
			os.Setenv(key, value)
		}
	}
}
