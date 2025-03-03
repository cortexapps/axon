package handler

import (
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
)

type HandlerInvoke struct {
	Id      string
	Name    string
	Reason  pb.HandlerInvokeType
	Args    map[string]string
	Timeout time.Duration
}
