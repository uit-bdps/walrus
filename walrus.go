package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fjukstad/walrus/pipeline"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
)

var stageMutexes []*sync.Mutex
var completedConditions []*sync.Cond
var completedStages []bool
var stageIndex map[string]int

func run(c *client.Client, p *pipeline.Pipeline, rootpath, filename string) error {
	stageMutexes = make([]*sync.Mutex, len(p.Stages))
	completedConditions = make([]*sync.Cond, len(p.Stages))
	completedStages = make([]bool, len(p.Stages))

	for i := range stageMutexes {
		stageMutexes[i] = &sync.Mutex{}
		completedConditions[i] = sync.NewCond(stageMutexes[i])
	}

	stageIndex = make(map[string]int, len(p.Stages))

	// Name to index mapping
	for i, stage := range p.Stages {
		stageIndex[stage.Name] = i
	}

	e := make(chan error, len(p.Stages))

	for i, stage := range p.Stages {
		go func(i int, stage *pipeline.Stage) {
			mountpath := "/walrus/" + stage.Name
			hostpath := rootpath + "/" + stage.Name

			repo, tag := getRepoAndTag(stage.Image)
			image := repo + ":" + tag
			rc, err := c.ImagePull(context.Background(), image,
				types.ImagePullOptions{})
			if err != nil {
				e <- errors.Wrap(err, "Could not pull image")
				return
			}

			defer rc.Close()

			_, err = ioutil.ReadAll(rc)
			if err != nil {
				e <- errors.Wrap(err, "error reading image pull")
			}

			// If the stage has any inputs it waits for these stages to complete
			// before starting
			if len(stage.Inputs) > 0 {
				for _, input := range stage.Inputs {
					index := stageIndex[input]
					cond := completedConditions[index]
					cond.L.Lock()
					for !completedStages[index] {
						cond.Wait()
					}
					cond.L.Unlock()
				}
			}

			// If the stage can be cached, check for a previous run. If this
			// container does not exist we need to run the stage again!
			if stage.Cache {
				_, _, err = exitCode(c, stage.Name)
				if err != nil {
					fmt.Println(err)
					fmt.Println("Warning: Could not find cached container", stage.Name, "will re-run the stage")
					stage.Cache = false
				}
			}

			// try to open output directory, if it exists then we can serve the
			// "cached"/old results
			_, err = os.Open(hostpath)

			if !stage.Cache || err != nil {
				// Removes a container with the same name as the stage.
				// This container could have been a previous run that the user
				// does not wish to cache, or a cached container which output
				// directory has been deleted. We ignore any error message
				// thrown9.
				c.ContainerRemove(context.Background(), stage.Name,
					types.ContainerRemoveOptions{RemoveVolumes: true,
						Force: true})

				// Note the 0777 permission bits. We use such liberal bits since
				// we do not know about the users within the docker containers
				// that are going to be run. We want to fix this later!
				err = os.MkdirAll(hostpath, 0777)
				if err != nil {
					e <- errors.Wrap(err, "Could not create output directory for stage")
					return
				}

				binds := []string{hostpath + ":" + mountpath}
				binds = append(binds, stage.Volumes...)

				resp, err := c.ContainerCreate(context.Background(),
					&container.Config{Image: image,
						Env:        stage.Env,
						Cmd:        stage.Cmd,
						Entrypoint: stage.Entrypoint,
					},
					&container.HostConfig{
						Binds:       binds,
						VolumesFrom: stage.Inputs},
					&network.NetworkingConfig{},
					stage.Name)

				if err != nil || resp.ID == " " {
					e <- errors.Wrap(err, "Could not create container "+stage.Name)
					return
				}
				containerId := resp.ID

				err = c.ContainerStart(context.Background(), containerId,
					types.ContainerStartOptions{})

				if err != nil {
					e <- errors.Wrap(err, "Could not start container "+stage.Name)
					return
				}

				_, err = c.ContainerWait(context.Background(), containerId)
				if err != nil {
					e <- errors.Wrap(err, "Failed to wait for container to finish")
					return
				}

			}

			cond := completedConditions[i]
			cond.L.Lock()

			// Notifies waiting stages on completion
			completedStages[i] = true

			cond.Broadcast()
			cond.L.Unlock()

			exitCode, errmsg, err := exitCode(c, stage.Name)
			if err != nil {
				e <- errors.Wrap(err, "Could not get exit code for stage "+stage.Name)
			}

			logs, err := getLogs(c, stage.Name)
			if err != nil {
				e <- err
				return
			}

			err = writeLogs(logs, hostpath)
			if err != nil {
				e <- errors.Wrap(err, "Could not write logs for stage "+stage.Name)
				return
			}

			if exitCode != 0 {
				e <- errors.New(stage.Name + " failed with exit code " + strconv.Itoa(exitCode) + "\n" + errmsg + "\n" + logs)
				return
			}
			fmt.Println(stage.Name, "completed successfully.")

			e <- nil
		}(i, stage)
	}
	var err error
	for i := 0; i < len(p.Stages); i++ {
		err = <-e
		if err != nil {
			fmt.Println(err)
		}
	}

	// Restore permission bits to output directory
	// err := filepath.Walk(rootpath, func(name string, info os.FileInfo, err error) error {
	// 	return os.Chmod(name, 0666)
	// })
	// if err != nil {
	// 	return err
	// }

	return err
}

func writeLogs(logs, path string) error {
	filename := path + "/walrus.log"
	return ioutil.WriteFile(filename, []byte(logs), 0777)
}

func exitCode(c *client.Client, container string) (int, string, error) {
	info, err := c.ContainerInspect(context.Background(), container)
	if err != nil {
		return 0, "", err
	}
	state := info.State
	return state.ExitCode, state.Error, nil
}

func getLogs(c *client.Client, container string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, _ := client.NewEnvClient()
	reader, err := client.ContainerLogs(ctx, container, types.ContainerLogsOptions{
		ShowStderr: true,
		ShowStdout: true,
	})
	if err != nil {
		return "", err
	}

	b, err := ioutil.ReadAll(reader)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(b), nil
}

// Stops any previously run pipeline and deletes the containers.
// Todo investigate if the docker pkg has defined some errors so that we don't
// have to do any string comparisons (ugly af).
func stopPreviousRun(c *client.Client, stages []*pipeline.Stage) error {
	for _, stage := range stages {
		err := c.ContainerKill(context.Background(), stage.Name, "9")
		if err != nil {
			if !strings.Contains(err.Error(), "No such") &&
				!strings.Contains(err.Error(), "not running") {
				return errors.Wrap(err, "Could not kill container "+stage.Name)
			}
		}

		// if the stage has caching enabled we can't remove it just yet. Its
		// exits codes etc. may be used in later pipeline runs
		if !stage.Cache {
			err = c.ContainerRemove(context.Background(), stage.Name,
				types.ContainerRemoveOptions{RemoveVolumes: true,
					Force: true})
			if err != nil {
				if !strings.Contains(err.Error(), "No such") {
					return errors.Wrap(err, "Could not remove container "+stage.Name)
				}
			}
		}
	}
	return nil
}

// Generate a list of volume mounts on the form
// /hostpath/stagename-containerid/:/walrus/stagename
func getInputVolumes(inputs []string, hostpath string) (volumes []string) {
	for _, input := range inputs {
		volumes = append(volumes, hostpath+"/"+input+":"+"/walrus"+"/"+input)
	}
	return volumes
}

func getRepoAndTag(pipelineImage string) (repo, tag string) {
	repoAndTag := strings.Split(pipelineImage, ":")
	if len(repoAndTag) == 1 {
		tag = "latest"
	} else {
		tag = repoAndTag[1]
	}
	repo = repoAndTag[0]

	return repo, tag
}

// Returns the full path of the  walrus configuration directory
func createConfigPath(hostpath string) string {
	return hostpath + "/" + ".walrus"
}

func fixMountPaths(stages []*pipeline.Stage) error {
	for i, stage := range stages {
		updatedVolumes := []string{}
		for _, volume := range stage.Volumes {
			hostClientPath := strings.Split(volume, ":")

			if len(hostClientPath) > 2 {
				return errors.New("Incorrect volume " + volume + " in pipeline description")
			}

			hostPath := hostClientPath[0]

			var clientPath string
			if len(hostClientPath) < 2 {
				clientPath = hostPath
			} else {
				clientPath = hostClientPath[1]
			}

			if strings.HasPrefix(hostPath, "/") {
				updatedVolumes = append(updatedVolumes, volume)
				continue
			}

			absPath, err := filepath.Abs(hostPath)
			if err != nil {
				return errors.Wrap(err, "Could not get the absolute path of the mount path")
			}

			mount := absPath + ":" + clientPath
			if stage.MountPropagation != "" {
				mount = mount + ":" + stage.MountPropagation
			}

			updatedVolumes = append(updatedVolumes, mount)
		}
		stages[i].Volumes = updatedVolumes
	}

	return nil
}

func main() {
	var configFilename = flag.String("i", "pipeline.json", "pipeline description file")
	var outputDir = flag.String("o", "walrus", "where walrus should store output data on the host")
	var web = flag.Bool("web", false, "host interactive visualization of the pipeline")
	var port = flag.String("p", ":9090", "port to run web server for pipeline visualization")

	flag.Parse()

	// set umask to 000 while walrus is running (we want to have full read/write
	// permissions to the output dirs while running.
	oldmask := syscall.Umask(000)
	defer syscall.Umask(oldmask)

	hostpath, err := filepath.Abs(*outputDir)
	if err != nil {
		fmt.Println("Check hostpath", err)
		return
	}

	flag.Parse()
	client, err := client.NewEnvClient()
	if err != nil {
		fmt.Println(err)
		return
	}

	p, err := pipeline.ParseConfig(*configFilename)
	if err != nil {
		fmt.Println(err)
		return
	}

	err = fixMountPaths(p.Stages)
	if err != nil {
		fmt.Println(err)
		return
	}

	err = stopPreviousRun(client, p.Stages)
	if err != nil {
		fmt.Println(err)
		return
	}

	if *web {
		go func() {
			err = startPipelineVisualization(p, *port)
			if err != nil {
				fmt.Println("Could not start pipeline visualization:", err)
			}
		}()
	}

	err = run(client, p, hostpath, *configFilename)

	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("All stages completed successfully.", "\nOutput written to ",
		hostpath)

}
