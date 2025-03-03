package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
)

type HistoryManager interface {
	Write(ctx context.Context, ex *pb.HandlerExecution) error
	GetHistory(ctx context.Context, handlerName string, includeLogs bool, tail int32) ([]*pb.HandlerExecution, error)
}

type historyManager struct {
	config config.AgentConfig
	logger *zap.Logger
}

func NewHistoryManager(config config.AgentConfig, logger *zap.Logger) HistoryManager {
	return &historyManager{
		config: config,
		logger: logger,
	}
}

var fileParseRegExp = regexp.MustCompile(`^(\d+)-(.+)\.json$`)

func (s *historyManager) parseHistoryFileName(historyFilePath string) (string, time.Time, error) {

	matches := fileParseRegExp.FindStringSubmatch(historyFilePath)

	if len(matches) != 3 {
		return "", time.Time{}, fmt.Errorf("failed to parse history file name: %s", historyFilePath)
	}

	timestamp, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return "", time.Time{}, err
	}

	handlerName := matches[2]
	return handlerName, time.Unix(0, timestamp*int64(time.Millisecond)), nil

}

func (s *historyManager) getHistoryPath(handlerName string, timestamp time.Time) string {
	if s.config.HistoryPath == "" {
		s.logger.Warn("history path not set")
		return ""
	}

	err := os.MkdirAll(s.config.HistoryPath, 0755)
	if err != nil {
		s.logger.Error("failed to create history path", zap.Error(err))
		return ""
	}

	return fmt.Sprintf("%s/%d-%s.json", s.config.HistoryPath, timestamp.UnixMilli(), handlerName)

}

// ReportInvocation is called by the client to report the result of an invocation, which will
// log the result of an invocation into the history path.
func (s *historyManager) Write(ctx context.Context, execution *pb.HandlerExecution) error {

	jsonData, err := json.Marshal(execution)
	if err != nil {
		s.logger.Error("failed to marshal json", zap.Error(err))
		return err
	}

	historyFilePath := s.getHistoryPath(execution.HandlerName, execution.StartClientTimestamp.AsTime())

	err = os.WriteFile(historyFilePath, jsonData, 0644)
	if err != nil {
		s.logger.Error("failed to write history file", zap.Error(err))
		return err
	}
	return nil
}

// GetHandlerHistory returns the history of a handler
func (s *historyManager) GetHistory(ctx context.Context, handlerName string, includeLogs bool, tail int32) ([]*pb.HandlerExecution, error) {
	// search the handler history path for anything with this handler name
	files, err := os.ReadDir(s.config.HistoryPath)

	if os.IsNotExist(err) {
		return make([]*pb.HandlerExecution, 0), nil
	}
	if err != nil {
		return nil, err
	}

	historyItems := make([]*pb.HandlerExecution, 0, 16)
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		fileName, _, err := s.parseHistoryFileName(file.Name())

		if err != nil {
			s.logger.Error("failed to parse history file name", zap.Error(err))
			continue
		}

		if fileName == handlerName {
			filePath := filepath.Join(s.config.HistoryPath, file.Name())
			contents, err := os.ReadFile(filePath)
			if err != nil {
				return nil, err
			}

			execution := &pb.HandlerExecution{}
			err = json.Unmarshal(contents, execution)
			if err != nil {
				s.logger.Error("failed to unmarshal history file", zap.String("path", filePath), zap.Error(err))
				continue
			}

			if !includeLogs {
				execution.Logs = nil
			}

			historyItems = append(historyItems, execution)
		}
	}
	slices.SortFunc(historyItems, func(l, r *pb.HandlerExecution) int {
		return int(l.StartClientTimestamp.AsTime().Unix() - r.StartClientTimestamp.AsTime().Unix())
	})

	if tail > 0 && len(historyItems) > int(tail) {
		historyItems = historyItems[len(historyItems)-int(tail):]
	}

	return historyItems, nil
}
