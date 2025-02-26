package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"

	"github.com/nearform/initium-cli/src/services/project"

	"github.com/nearform/initium-cli/src/utils/defaults"
	"github.com/nearform/initium-cli/src/utils/logger"
)

type DockerService struct {
	project        project.Project
	DockerFileName string
	Client         client.Client
	AuthConfig     types.AuthConfig
	dockerImage    DockerImage
}

// Create a new instance of the DockerService
func New(project project.Project, dockerImage DockerImage, dockerFileName string) (DockerService, error) {
	client, err := getClient()
	if err != nil {
		return DockerService{}, err
	}

	return DockerService{
		project:        project,
		DockerFileName: dockerFileName,
		Client:         *client,
		dockerImage:    dockerImage,
	}, nil
}

func getClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.PrintError("Failed to create docker client: ", err)
		return nil, err
	}

	return cli, nil
}

func (ds DockerService) generateDockerfile(tarWriter *tar.Writer) error {
	// Add another file to the build context from an array of bytes
	fileBytes, err := ds.project.Dockerfile()
	if err != nil {
		return fmt.Errorf("Loading dockerfile %v", err)
	}
	hdr := &tar.Header{
		Name:    defaults.GeneratedDockerFile,
		Mode:    0600,
		Size:    int64(len(fileBytes)),
		ModTime: time.Now(),
	}

	if err := tarWriter.WriteHeader(hdr); err != nil {
		return fmt.Errorf("Writing Dockerfile header %v", err)
	}
	if _, err := tarWriter.Write(fileBytes); err != nil {
		return fmt.Errorf("Writing Dockerfile content %v", err)
	}

	return nil
}

func (ds DockerService) buildContext() (*bytes.Reader, error) {
	// Get the context for the docker build
	existingBuildContext, err := archive.TarWithOptions(ds.dockerImage.Directory, &archive.TarOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to create build context %v", err)
	}

	// Create a new in-memory tar archive for the combined build context
	var combinedBuildContext bytes.Buffer
	tarWriter := tar.NewWriter(&combinedBuildContext)

	// Copy the existing build context into the new tar archive
	tarReader := tar.NewReader(existingBuildContext)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break // End of the archive
		}
		if err != nil {
			return nil, fmt.Errorf("Error copy context %v", err)
		}
		if err := tarWriter.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("Error copy context %v", err)
		}
		if _, err := io.Copy(tarWriter, tarReader); err != nil {
			return nil, fmt.Errorf("Error copy context %v", err)
		}
	}

	// if dockerfile-name is not specified generate a dockerfile
	if ds.DockerFileName == defaults.GeneratedDockerFile {
		if err = ds.generateDockerfile(tarWriter); err != nil {
			return nil, err
		}
	}

	// Close the tar archive
	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("Closing tarWriter %v", err)
	}

	// Convert the combined build context to an io.Reader
	return bytes.NewReader(combinedBuildContext.Bytes()), nil
}

// Build Docker image
func (ds DockerService) Build() error {
	logger.PrintInfo("Building " + ds.dockerImage.LocalTag())

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancel()

	combinedBuildContextReader, err := ds.buildContext()
	if err != nil {
		return fmt.Errorf("Creating docker context %v", err)
	}

	// Get the options for the docker build
	buildOptions := types.ImageBuildOptions{
		Context:    combinedBuildContextReader,
		Dockerfile: ds.DockerFileName,
		Tags:       []string{ds.dockerImage.LocalTag()},
		Remove:     true,
	}

	// Build the image
	buildResponse, err := ds.Client.ImageBuild(ctx, combinedBuildContextReader, buildOptions)
	if err != nil {
		return fmt.Errorf("Failed to build docker image %v", err)
	}

	defer buildResponse.Body.Close()

	logger.PrintStream(buildResponse.Body)

	return nil
}

// Push Docker image
func (ds DockerService) Push() error {
	logger.PrintInfo("Pushing to " + ds.dockerImage.RemoteTag())
	logger.PrintInfo("User: " + ds.AuthConfig.Username)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*120)
	defer cancel()

	encodedJSON, err := json.Marshal(ds.AuthConfig)
	if err != nil {
		return err
	}

	ipo := types.ImagePushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(encodedJSON),
	}

	err = ds.Client.ImageTag(ctx, ds.dockerImage.LocalTag(), ds.dockerImage.RemoteTag())
	if err != nil {
		return fmt.Errorf("Tagging local image for remote %v", err)
	}

	pushResponse, err := ds.Client.ImagePush(ctx, ds.dockerImage.RemoteTag(), ipo)
	defer pushResponse.Close()
	if err != nil {
		return fmt.Errorf("Failed to push docker image %v", err)
	}

	return logger.PrintStream(pushResponse)
}
