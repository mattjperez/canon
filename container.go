package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/multierr"
	"gopkg.in/yaml.v3"
)

//go:embed canon_setup.sh
var canonSetupScript string

var canonMountPoint = "/host"

func stopContainer(ctx context.Context, cli *client.Client, containerID string) error {
	stopTimeout := time.Second * 10
	err := cli.ContainerStop(ctx, containerID, &stopTimeout)
	if err != nil {
		return err
	}

	// wait for the container to exit
	statusCh, errCh := cli.ContainerWait(ctx, containerID, container.WaitConditionRemoved)
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case status := <-statusCh:
		if status.Error != nil && status.Error.Message != "" {
			return fmt.Errorf("Error waiting for container stop: %s\n", status.Error.Message)
		}
	}
	return nil
}

func startContainer(ctx context.Context, cli *client.Client, profile *Profile, sshSock string) (string, error) {
	cfg := &container.Config{
		Image:        profile.Image,
		AttachStdout: true,
	}
	if profile.Ssh {
		cfg.Env = []string{"CANON_SSH=true"}
	}
	hostCfg := &container.HostConfig{AutoRemove: true}
	netCfg := &network.NetworkingConfig{}
	platform := &v1.Platform{OS: "linux", Architecture: profile.Arch}
	if profile.Ssh {
		if sshSock != "" {
			mnt := mount.Mount{
				Type:   "bind",
				Source: sshSock,
				Target: sshSock,
			}
			hostCfg.Mounts = append(hostCfg.Mounts, mnt)
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		userSSHDir := filepath.Join(home, ".ssh")
		// TODO check inside the container for the actual home directory
		canonSSHDir := "/home/" + profile.User + "/.ssh"

		mnt := mount.Mount{
			Type:     "bind",
			Source:   userSSHDir,
			Target:   canonSSHDir,
			ReadOnly: true,
		}
		hostCfg.Mounts = append(hostCfg.Mounts, mnt)
	}

	if profile.Netrc {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		userNetRC := filepath.Join(home, ".netrc")
		canonNetRC := "/home/" + profile.User + "/.netrc"
		mnt := mount.Mount{
			Type:     "bind",
			Source:   userNetRC,
			Target:   canonNetRC,
			ReadOnly: true,
		}
		hostCfg.Mounts = append(hostCfg.Mounts, mnt)
	}

	if profile.Path == string(os.PathSeparator) {
		fmt.Fprintf(os.Stderr,
			"WARNING: profile path is root (%s) so mounting entire host system to %s\n",
			string(os.PathSeparator),
			canonMountPoint,
		)
	}

	mnt := mount.Mount{
		Type:   "bind",
		Source: profile.Path,
		Target: canonMountPoint,
	}
	hostCfg.Mounts = append(hostCfg.Mounts, mnt)

	// label the image with the running profile data
	profYaml, err := yaml.Marshal(profile)
	if err != nil {
		return "", err
	}

	cfg.Labels = map[string]string{
		"com.viam.canon.type":         "one-shot",
		"com.viam.canon.profile":      profile.Name,
		"com.viam.canon.profile-data": string(profYaml),
	}
	if profile.Persistent {
		cfg.Labels["com.viam.canon.type"] = "persistent"
	}
	name := fmt.Sprintf("canon-%s-%x", profile.Name, rand.Uint32())

	// fill out the entrypoint template
	canonSetupScript = strings.Replace(canonSetupScript, "__CANON_USER__", profile.User, -1)
	canonSetupScript = strings.Replace(canonSetupScript, "__CANON_GROUP__", profile.Group, -1)
	canonSetupScript = strings.Replace(canonSetupScript, "__CANON_UID__", fmt.Sprint(os.Getuid()), -1)
	canonSetupScript = strings.Replace(canonSetupScript, "__CANON_GID__", fmt.Sprint(os.Getgid()), -1)
	cfg.Entrypoint = []string{}
	cfg.Cmd = []string{"bash", "-c", canonSetupScript}

	resp, err := cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, platform, name)
	if err != nil {
		// if we don't have the image or have the wrong architecture, we have to pull it
		if strings.Contains(err.Error(), "does not match the specified platform") || strings.Contains(err.Error(), "No such image") {
			err2 := update(ImageDef{Image: cfg.Image, Platform: platform.OS + "/" + platform.Architecture})
			if err2 != nil {
				return "", err2
			}
			resp, err = cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, platform, name)
			if err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}

	for _, warn := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "Warning during container creation: %s\n", warn)
	}

	containerID := resp.ID
	fmt.Printf("Container ID: %s\n", containerID)

	err = cli.ContainerStart(ctx, containerID, types.ContainerStartOptions{})
	if err != nil {
		return containerID, err
	}

	hijack, err := cli.ContainerAttach(ctx, containerID, types.ContainerAttachOptions{Stream: true, Stdout: true})
	defer hijack.Close()

	scanner := bufio.NewScanner(hijack.Reader)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "CANON_READY") {
			break
		}
	}
	return containerID, scanner.Err()
}

func terminate(profile *Profile, all bool) error {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return err
	}
	f := filters.NewArgs()
	if all {
		f.Add("label", "com.viam.canon.profile")
	} else {
		f.Add("label", "com.viam.canon.profile="+profile.Name)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{Filters: f})
	if err != nil {
		return err
	}
	if len(containers) > 1 && !all {
		return errors.New("multiple matching containers found, please retry with '--all' option")
	}
	timeout := time.Second * 5
	for _, c := range containers {
		fmt.Printf("terminating %s\n", c.Labels["com.viam.canon.profile"])
		err = multierr.Combine(err, cli.ContainerStop(context.Background(), c.ID, &timeout))
	}
	return err
}

func getPersistentContainer(ctx context.Context, cli *client.Client, profile *Profile) (string, error) {
	f := filters.NewArgs()
	f.Add("label", "com.viam.canon.type=persistent")
	f.Add("label", "com.viam.canon.profile="+profile.Name)
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{Filters: f})
	if err != nil {
		return "", err
	}
	if len(containers) > 1 {
		return "", fmt.Errorf("more than one container is running for profile %s, please terminate all containers and retry", profile.Name)
	}
	if len(containers) < 1 {
		return "", nil
	}

	curProfYaml, err := yaml.Marshal(profile)
	if err != nil {
		return "", err
	}

	profYaml, ok := containers[0].Labels["com.viam.canon.profile-data"]
	if !ok {
		return "", fmt.Errorf("no profile data on persistent container for %s, please terminate all containers and retry", profile.Name)
	}

	if profYaml != string(curProfYaml) {
		return "", fmt.Errorf(
			"existing container settings for %s don't match current settings, please terminate all containers and retry",
			profile.Name,
		)
	}

	return containers[0].ID, nil
}
