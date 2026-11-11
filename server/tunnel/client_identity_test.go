package tunnel

import (
	"testing"

	"github.com/cortexapps/axon-server/broker"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zapcore"
)

func identityFieldMap(i ClientIdentity) map[string]any {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range i.LogFields(broker.NewToken("t")) {
		f.AddTo(enc)
	}
	return enc.Fields
}

// An agent that sends its relay mode and image variant has both logged with
// its version.
func TestLogFields_RelayModeAndImageVariant(t *testing.T) {
	f := identityFieldMap(ClientIdentity{InstanceID: "i", ClientVersion: "3.1.4", RelayMode: "grpc-tunnel", ImageVariant: "grpc"})
	assert.Equal(t, "3.1.4", f["clientVersion"])
	assert.Equal(t, "grpc-tunnel", f["relayMode"])
	assert.Equal(t, "grpc", f["imageVariant"])
}

// An older agent sends neither; the fields are left out rather than logged
// empty.
func TestLogFields_OlderAgentOmitsNewFields(t *testing.T) {
	f := identityFieldMap(ClientIdentity{InstanceID: "i", ClientVersion: "0.0.1"})
	assert.NotContains(t, f, "relayMode")
	assert.NotContains(t, f, "imageVariant")
}
