package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	pb "github.com/cortexapps/axon/.generated/proto/github.com/cortexapps/axon"

	"github.com/cortexapps/axon/config"
	"go.uber.org/zap"
)

type HistoryManager interface {
	Start() error
	Close() error
	Write(ctx context.Context, ex *pb.HandlerExecution) error
	GetHistory(ctx context.Context, handlerName string, includeLogs bool, tail int32) ([]*pb.HandlerExecution, error)
}

type historyManager struct {
	config  config.AgentConfig
	logger  *zap.Logger
	running atomic.Bool
	done    chan struct{}
}

func NewHistoryManager(config config.AgentConfig, logger *zap.Logger) HistoryManager {
	return &historyManager{
		config: config,
		logger: logger,
		done:   make(chan struct{}),
	}
}

const cleanupInterval = time.Hour

func (s *historyManager) Start() error {
	if s.running.CompareAndSwap(false, true) {
		s.logger.Info("history manager started")
		historyPath, err := s.getHistoryDirectory()
		if err != nil {
			s.logger.Error("failed to get history directory", zap.Error(err))
			return err
		}
		maxHistoryAge := s.config.HandlerHistoryMaxAge
		maxSizeBytes := s.config.HandlerHistoryMaxSizeBytes
		go func() {
			for s.running.Load() {
				select {
				case <-s.done:
					return
				case <-time.After(cleanupInterval):
					minTimestamp := time.Now().Add(-maxHistoryAge)
					s.cleanupDirectory(historyPath, minTimestamp, maxSizeBytes, nil)
				}
			}
		}()
	}
	return nil
}

func (s *historyManager) Close() error {
	if s.running.CompareAndSwap(true, false) {
		close(s.done)
	}
	return nil
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

func (s *historyManager) getHistoryDirectory() (string, error) {
	if s.config.HandlerHistoryPath == "" {
		s.logger.Warn("history path not set")
		return "", nil
	}

	err := os.MkdirAll(s.config.HandlerHistoryPath, 0755)
	if err != nil {
		s.logger.Error("failed to create history path", zap.Error(err))
		return "", err
	}
	return s.config.HandlerHistoryPath, nil
}

func (s *historyManager) getHistoryPath(handlerName string, timestamp time.Time) string {

	return fmt.Sprintf("%s/%d-%s.json", s.config.HandlerHistoryPath, timestamp.UnixMilli(), handlerName)
}

func (s *historyManager) cleanupDirectory(path string, minTimestamp time.Time, maxSizeBytes int64, extractTimestamp func(info os.FileInfo) time.Time) (int, error) {
	// Read all files in the directory
	files, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}

	infos := make([]os.FileInfo, 0, len(files))
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		info, err := file.Info()
		if err != nil {
			s.logger.Error("failed to get file info", zap.String("path", file.Name()), zap.Error(err))
			continue
		}
		infos = append(infos, info)
	}

	// Sort files by modification time (newest first)
	sort.Slice(infos, func(i, j int) bool {
		leftModTime := infos[i].ModTime()
		rightModTime := infos[j].ModTime()
		return leftModTime.UnixMilli() > rightModTime.UnixMilli()
	})

	if extractTimestamp == nil {
		extractTimestamp = func(info os.FileInfo) time.Time {
			return info.ModTime()
		}
	}

	var deleteCount int
	var remainingSize int64 = int64(maxSizeBytes)
	for _, info := range infos {
		timestamp := extractTimestamp(info)
		tooOld := timestamp.Before(minTimestamp)
		if !tooOld {
			remainingSize -= info.Size()
		}
		if tooOld || remainingSize < 0 {
			filePath := filepath.Join(path, info.Name())
			err := os.Remove(filePath)
			if err != nil {
				s.logger.Error("failed to remove history file", zap.String("path", filePath), zap.Error(err))
			} else {
				deleteCount++
			}
		}
	}

	s.logger.Info("cleaned up history directory", zap.String("path", path), zap.Int("deleted_file_count", deleteCount))
	return deleteCount, nil
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
	files, err := os.ReadDir(s.config.HandlerHistoryPath)

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
			filePath := filepath.Join(s.config.HandlerHistoryPath, file.Name())
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
