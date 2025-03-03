package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

func Init(language, name, targetRoot string) error {

	targetDir := filepath.Join(targetRoot, name)
	err := os.MkdirAll(targetDir, os.ModePerm)
	if err != nil {
		return err
	}

	fmt.Printf("Initializing `%s` (%s) project at '%s' \n", name, language, targetDir)

	scaffoldDir := "/app/scaffold"

	if envScaffoldDir := os.Getenv("SCAFFOLD_DIR"); envScaffoldDir != "" {
		scaffoldDir = envScaffoldDir
	}

	scaffoldLangDir := filepath.Join(scaffoldDir, language)

	scaffoldDockerImage := "ghcr.io/cortexapps/cortex-axon-agent:latest"

	if envScaffoldDockerImage := os.Getenv("SCAFFOLD_DOCKER_IMAGE"); envScaffoldDockerImage != "" {
		scaffoldDockerImage = envScaffoldDockerImage
	}

	_, err = os.Stat(scaffoldLangDir)
	if err != nil {
		return fmt.Errorf("language %s not supported (scaffold dir=%s)", language, scaffoldLangDir)
	}

	fmt.Println("Copying scaffold files from", scaffoldLangDir, "to", targetDir)
	err = exec.Command("cp", "-R", scaffoldLangDir+"/.", targetDir+"/.").Run()

	if err != nil {
		return fmt.Errorf("failed to copy scaffold files: %v", err)
	}

	err = templateAllFiles(targetDir, struct {
		ProjectName string
		DockerImage string
	}{
		ProjectName: name,
		DockerImage: scaffoldDockerImage,
	},
	)

	if err != nil {
		return fmt.Errorf("failed to template files: %v", err)
	}

	fmt.Println("Project initialized successfully to", targetDir)
	return nil
}

func templateAllFiles(dir string, args interface{}) error {

	funcMap := template.FuncMap{
		"toLower": strings.ToLower,
	}

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		fileContent, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		template := template.Must(template.New(path).Funcs(funcMap).Parse(string(fileContent)))

		buf := &bytes.Buffer{}

		err = template.Execute(buf, args)

		if err != nil {
			return err
		}

		err = os.WriteFile(path, buf.Bytes(), 0644)

		return err
	})
}
