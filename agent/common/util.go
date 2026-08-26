package common

import (
	"fmt"
	"math/rand"
	"net"
	"os"
)

func ApplyEnv(envVars map[string]string) {

	for k, v := range envVars {
		os.Setenv(k, v)
	}
}

// ephemeralPortFloor is where Linux starts handing out source ports for
// outbound connections (32768; macOS starts higher). A listener bound below
// this cannot collide with a connection the same process just made.
const ephemeralPortFloor = 32768

// GetRandomPort returns a free port for a test server to listen on.
//
// It picks below the ephemeral range and confirms the port is actually
// bindable. The previous version did neither: it drew from 10000-50000, so
// roughly two picks in five landed in the ephemeral range where an outbound
// connection's source port could already be sitting, and it never checked
// availability at all. Both produce the same symptom — an intermittent
// "bind: address already in use" panic unrelated to the test that hits it.
//
// A port is only known free at the moment it is checked, so callers racing to
// bind could still collide; asking the OS closes the window that actually
// caused failures rather than pretending it is gone.
func GetRandomPort() int {
	const attempts = 50

	for i := 0; i < attempts; i++ {
		port := 10000 + rand.Intn(ephemeralPortFloor-10000)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // in use; try another
		}
		l.Close()
		return port
	}
	panic("could not find a free port below the ephemeral range")
}
