// Fences the load-harness mains (three standalone package-main files run
// via `go run <file>.go` in golang containers) out of the agent module so
// `go build ./...` doesn't try to compile them as one package.
module loadharness

go 1.24
