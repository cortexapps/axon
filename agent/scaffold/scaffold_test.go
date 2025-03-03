package scaffold

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInit(t *testing.T) {

	scaffoldDir := setupFakeScaffolds(t)

	defer os.RemoveAll(scaffoldDir)

	os.Setenv("SCAFFOLD_DIR", scaffoldDir)
	os.Setenv("SCAFFOLD_DOCKER_IMAGE", "my-docker-image:latest")

	targetDir := t.TempDir() + "/out"
	err := os.MkdirAll(targetDir, os.ModePerm)
	require.NoError(t, err)
	defer os.RemoveAll(targetDir)

	err = Init("go", "my-project", targetDir)
	require.NoError(t, err)
	targetDir = targetDir + "/my-project/"

	// check that the files were copied
	_, err = os.Stat(targetDir + "main.go")
	require.NoError(t, err)

	_, err = os.Stat(targetDir + "Dockerfile")
	require.NoError(t, err)

	// check for the templated values
	mainFile, err := os.ReadFile(targetDir + "/main.go")
	require.NoError(t, err)

	require.Contains(t, string(mainFile), "package main")
	require.Contains(t, string(mainFile), "github.com/cortexapps/my-project")

	dockerFile, err := os.ReadFile(targetDir + "/Dockerfile")
	require.NoError(t, err)

	require.Contains(t, string(dockerFile), "FROM my-docker-image:latest")

}

var fakeGoTemplate = `
package main

import (
	"github.com/cortexapps/{{.ProjectName}}"
)
`

var fakeDockerfile = `
FROM {{.DockerImage}}
RUN echo "hello"
`

func setupFakeScaffolds(t *testing.T) string {
	// setup fake scaffold directories
	dir := t.TempDir() + "/scaffold"
	err := os.MkdirAll(dir, os.ModePerm)

	require.NoError(t, err)

	err = os.MkdirAll(dir+"/go", os.ModePerm)
	require.NoError(t, err)

	// write the two fake scaffold files
	err = os.WriteFile(dir+"/go/main.go", []byte(fakeGoTemplate), os.ModePerm)
	require.NoError(t, err)

	err = os.WriteFile(dir+"/go/Dockerfile", []byte(fakeDockerfile), os.ModePerm)
	require.NoError(t, err)

	return dir
}
