package lfs

import (
	"context"
	"path/filepath"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
)

func StartServer(mountDir string) error {

	c, err := client.NewEnvClient()
	if err != nil {
		return errors.Wrap(err, "Could not create Docker client")
	}

	image := "fjukstad/lfs-server"
	_, err = c.ImagePull(context.Background(), image,
		types.ImagePullOptions{})

	if err != nil {
		return errors.Wrap(err, "Could not pull iamge")
	}

	hostPath, err := filepath.Abs(mountDir)
	if err != nil {
		return errors.Wrap(err,
			"Could not create absolute git-lfs directory path")
	}

	bind := hostPath + ":/lfs"

	ps := make(nat.PortSet)
	ps["9999/tcp"] = struct{}{}

	pm := make(nat.PortMap)
	pm["9999/tcp"] = []nat.PortBinding{nat.PortBinding{"0.0.0.0", "9999"}}

	resp, err := c.ContainerCreate(context.Background(),
		&container.Config{Image: image,
			ExposedPorts: ps},
		&container.HostConfig{
			Binds:        []string{bind},
			PortBindings: pm},
		&network.NetworkingConfig{},
		"git-lfs-server")

	if err != nil || resp.ID == " " {
		return errors.Wrap(err, "Could not create git-lfs server container")
	}

	containerId := resp.ID

	err = c.ContainerStart(context.Background(), containerId,
		types.ContainerStartOptions{})
	return err

}
