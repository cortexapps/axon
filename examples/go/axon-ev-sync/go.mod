module github.com/cortexapps/axon_apps/axon-ev-sync

go 1.25.5

require (
	github.com/cortexapps/axon-go v0.0.0
	go.uber.org/zap v1.27.0
)

replace github.com/cortexapps/axon-go => ../../../sdks/go

require (
	github.com/google/uuid v1.6.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/net v0.33.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240903143218-8af14fe29dc1 // indirect
	google.golang.org/grpc v1.68.0 // indirect
	google.golang.org/protobuf v1.36.0 // indirect
)
