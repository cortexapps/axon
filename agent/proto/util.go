package proto

import (
	"fmt"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func ToProtoError(err error, code string) *pb.Error {
	if err == nil {
		return nil
	}
	return &pb.Error{
		Code:    code,
		Message: err.Error(),
	}
}

func SetInvocationResult(req *pb.ReportInvocationRequest, result interface{}) {
	if result == nil {
		return
	}
	req.Message = &pb.ReportInvocationRequest_Result{
		Result: &pb.InvokeResult{
			Value: fmt.Sprintf("%v", result),
		},
	}
}

func SetInvocationError(req *pb.ReportInvocationRequest, code string, err error) {
	if err == nil && code == "" {
		return
	}
	var message string = ""
	if err != nil {
		message = err.Error()
	}
	req.Message = &pb.ReportInvocationRequest_Error{
		Error: &pb.Error{
			Code:    code,
			Message: message,
		},
	}
}

func ReportToExecution(req *pb.ReportInvocationRequest, sentAt time.Time) *pb.HandlerExecution {
	execution := &pb.HandlerExecution{
		DispatchId:             req.HandlerInvoke.DispatchId,
		HandlerName:            req.HandlerInvoke.HandlerName,
		InvocationId:           req.HandlerInvoke.InvocationId,
		StartClientTimestamp:   req.StartClientTimestamp,
		DurationMs:             req.DurationMs,
		PublishServerTimestamp: timestamppb.New(sentAt),
		ReceiveServerTimestamp: timestamppb.Now(),
		Error:                  req.GetError(),
		Logs:                   req.Logs,
	}
	return execution

}
