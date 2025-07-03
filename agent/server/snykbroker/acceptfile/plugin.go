package acceptfile

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"go.uber.org/zap"
)

type Plugin struct {
	Name     string
	FullPath string
	Logger   *zap.Logger
}

func NewPlugin(name, fullPath string, logger *zap.Logger) Plugin {
	_, err := os.Stat(fullPath)
	if err != nil {
		logger.Panic("Failed to stat plugin file", zap.String("path", fullPath),
			zap.Error(err))
	}
	return Plugin{
		Name:     name,
		FullPath: fullPath,
		Logger:   logger.Named("plugin-" + name),
	}
}

func isExecutable(fileInfo os.FileInfo) bool {
	// Check if the file is a regular file and has executable permissions
	return fileInfo.Mode().IsRegular() && fileInfo.Mode().Perm()&0111 != 0
}

func FindPlugin(name string, dirs []string, logger *zap.Logger) (Plugin, error) {
	// searches for the plugin in the provided directories
	for _, dir := range dirs {
		fullPath := dir + "/" + name
		if _, err := exec.LookPath(fullPath); err == nil {

			stat, err := os.Stat(fullPath)
			if err != nil {
				logger.Warn("Failed to stat plugin file", zap.String("path", fullPath), zap.Error(err))
				continue
			}

			if !isExecutable(stat) {
				logger.Warn("Plugin file is not executable", zap.String("path", fullPath))
				continue
			}

			return NewPlugin(name, fullPath, logger), nil
		}
	}
	return Plugin{}, fmt.Errorf("plugin %q not found in directories: %v", name, dirs)
}

func (p Plugin) Execute() (string, error) {

	// executes the plugin and returns the output from stdout
	p.Logger.Info("Executing plugin", zap.String("path", p.FullPath))

	cmd := exec.Command(p.FullPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if err != nil {
		p.Logger.Error("Plugin execution failed", zap.String("path", p.FullPath), zap.Error(err), zap.String("stderr", stderr.String()), zap.String("stdout", stdout.String()))
		return "", fmt.Errorf("failed to execute %q (%v), output was:\nstderr: %s, stdout:%s", p.FullPath, err, stderr.String(), stdout.String())
	}
	return stdout.String(), nil

}
