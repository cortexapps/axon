package common

import _ "embed"

// ClientVersion is the protocol version the agent sends with its Cortex API
// registration, so the API can stay backward compatible with it. It is not
// the software version; that is config.BuildVersion.
const ClientVersion = "0.0.1"

// This is the version the GRPC client
//
//go:embed grpcversion.txt
var GrpcVersion string
