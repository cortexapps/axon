package common

// This is the version we send to Cortex to identify our
// client protocol Backcompat should always be possible with this
// sent along
const ClientVersion = "0.0.1"

// This is the version the GRPC client
// go:embed grpcversion.txt
var GrpcVersion string
