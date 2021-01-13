// +build functional

package cri_containerd

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func runLogRotationContainer(t *testing.T, sandboxRequest *runtime.RunPodSandboxRequest, request *runtime.CreateContainerRequest, log string, logArchive string) {
	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request.PodSandboxId = podID
	request.SandboxConfig = sandboxRequest.Config

	containerID := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerID)

	startContainer(t, client, ctx, containerID)
	defer stopContainer(t, client, ctx, containerID)

	// Give some time for log output to accumulate.
	time.Sleep(3 * time.Second)

	// Rotate the logs. This is done by first renaming the existing log file,
	// then calling ReopenContainerLog to cause containerd to start writing to
	// a new log file.

	if err := os.Rename(log, logArchive); err != nil {
		t.Fatalf("failed to rename log: %v", err)
	}

	if _, err := client.ReopenContainerLog(ctx, &runtime.ReopenContainerLogRequest{ContainerId: containerID}); err != nil {
		t.Fatalf("failed to reopen log: %v", err)
	}

	// Give some time for log output to accumulate.
	time.Sleep(3 * time.Second)
}

func runContainerLifetime(t *testing.T, client runtime.RuntimeServiceClient, ctx context.Context, containerID string) {
	defer removeContainer(t, client, ctx, containerID)
	startContainer(t, client, ctx, containerID)
	stopContainer(t, client, ctx, containerID)
}

func Test_RotateLogs_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	image := "alpine:latest"
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatalf("failed creating temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("failed deleting temp dir: %v", err)
		}
	}()
	log := filepath.Join(dir, "log.txt")
	logArchive := filepath.Join(dir, "log-archive.txt")

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, image})
	logrus.SetLevel(logrus.DebugLevel)

	sandboxRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)

	request := &runtime.CreateContainerRequest{
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: image,
			},
			Command: []string{
				"ash",
				"-c",
				"i=0; while true; do echo $i; i=$(expr $i + 1); sleep .1; done",
			},
			LogPath: log,
			Linux:   &runtime.LinuxContainerConfig{},
		},
	}

	runLogRotationContainer(t, sandboxRequest, request, log, logArchive)

	// Make sure we didn't lose any values while rotating. First set of output
	// should be in logArchive, followed by the output in log.

	logArchiveFile, err := os.Open(logArchive)
	if err != nil {
		t.Fatal(err)
	}
	defer logArchiveFile.Close()

	logFile, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	s := bufio.NewScanner(io.MultiReader(logArchiveFile, logFile))
	expected := 0
	for s.Scan() {
		v := strings.Fields(s.Text())
		n, err := strconv.Atoi(v[len(v)-1])
		if err != nil {
			t.Fatalf("failed to parse log value as integer: %v", err)
		}
		if n != expected {
			t.Fatalf("missing expected output value: %v (got %v)", expected, n)
		}
		expected++
	}
}

func Test_RunContainer_Events_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})
	client := newTestRuntimeClient(t)

	podctx, podcancel := context.WithCancel(context.Background())
	defer podcancel()
	targetNamespace := "k8s.io"

	sandboxRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)

	podID := runPodSandbox(t, client, podctx, sandboxRequest)
	defer removePodSandbox(t, client, podctx, podID)
	defer stopPodSandbox(t, client, podctx, podID)

	request := &runtime.CreateContainerRequest{
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageLcowAlpine,
			},
			Command: []string{
				"top",
			},
			Linux: &runtime.LinuxContainerConfig{},
		},
		PodSandboxId:  podID,
		SandboxConfig: sandboxRequest.Config,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	topicNames, filters := getTargetRunTopics()
	eventService := newTestEventService(t)
	stream, errs := eventService.Subscribe(ctx, filters...)

	containerID := createContainer(t, client, podctx, request)
	runContainerLifetime(t, client, podctx, containerID)

	for _, topic := range topicNames {
		select {
		case env := <-stream:
			if topic != env.Topic {
				t.Fatalf("event topic %v does not match expected topic %v", env.Topic, topic)
			}
			if targetNamespace != env.Namespace {
				t.Fatalf("event namespace %v does not match expected namespace %v", env.Namespace, targetNamespace)
			}
			t.Logf("event topic seen: %v", env.Topic)

			id, _, err := convertEvent(env.Event)
			if err != nil {
				t.Fatalf("topic %v event: %v", env.Topic, err)
			}
			if id != containerID {
				t.Fatalf("event topic %v belongs to container %v, not targeted container %v", env.Topic, id, containerID)
			}
		case e := <-errs:
			t.Fatalf("event subscription err %v", e)
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("event %v deadline exceeded", topic)
			}
		}
	}
}

func Test_RunContainer_ForksThenExits_ShowsAsExited_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})
	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	podRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)
	podID := runPodSandbox(t, client, ctx, podRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	containerRequest := &runtime.CreateContainerRequest{
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageLcowAlpine,
			},
			Command: []string{
				// Fork a background process (that runs forever), then exit.
				"ash",
				"-c",
				"ash -c 'while true; do echo foo; sleep 1; done' &",
			},
			Linux: &runtime.LinuxContainerConfig{},
		},
		PodSandboxId:  podID,
		SandboxConfig: podRequest.Config,
	}
	containerID := createContainer(t, client, ctx, containerRequest)
	defer removeContainer(t, client, ctx, containerID)
	startContainer(t, client, ctx, containerID)
	defer stopContainer(t, client, ctx, containerID)

	// Give the container init time to exit.
	time.Sleep(5 * time.Second)

	// Validate that the container shows as exited. Once the container init
	// dies, the forked background process should be killed off.
	statusResponse, err := client.ContainerStatus(ctx, &runtime.ContainerStatusRequest{ContainerId: containerID})
	if err != nil {
		t.Fatalf("failed to get container status: %v", err)
	}
	if statusResponse.Status.State != runtime.ContainerState_CONTAINER_EXITED {
		t.Fatalf("container expected to be exited but is in state %s", statusResponse.Status.State)
	}
}

func Test_RunContainer_ZeroVPMEM_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})

	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)
	sandboxRequest.Config.Annotations = map[string]string{
		"io.microsoft.virtualmachine.lcow.preferredrootfstype":         "initrd",
		"io.microsoft.virtualmachine.devices.virtualpmem.maximumcount": "0",
	}

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request := &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageLcowAlpine,
			},
			Command: []string{
				"top",
			},
		},
		SandboxConfig: sandboxRequest.Config,
	}

	containerID := createContainer(t, client, ctx, request)
	runContainerLifetime(t, client, ctx, containerID)
}

func Test_RunContainer_ZeroVPMEM_Multiple_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})

	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)
	sandboxRequest.Config.Annotations = map[string]string{
		"io.microsoft.virtualmachine.lcow.preferredrootfstype":         "initrd",
		"io.microsoft.virtualmachine.devices.virtualpmem.maximumcount": "0",
	}

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request := &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: "",
			},
			Image: &runtime.ImageSpec{
				Image: imageLcowAlpine,
			},
			Command: []string{
				"top",
			},
		},
		SandboxConfig: sandboxRequest.Config,
	}

	request.Config.Metadata.Name = "Container-1"
	containerIDOne := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerIDOne)
	startContainer(t, client, ctx, containerIDOne)
	defer stopContainer(t, client, ctx, containerIDOne)

	request.Config.Metadata.Name = "Container-2"
	containerIDTwo := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerIDTwo)
	startContainer(t, client, ctx, containerIDTwo)
	defer stopContainer(t, client, ctx, containerIDTwo)
}

func Test_RunContainer_GMSA_WCOW_Process(t *testing.T) {
	requireFeatures(t, featureWCOWProcess, featureGMSA)

	credSpec := gmsaSetup(t)
	pullRequiredImages(t, []string{imageWindowsNanoserver})
	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxRequest := getRunPodSandboxRequest(t, wcowProcessRuntimeHandler)

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request := &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageWindowsNanoserver,
			},
			Command: []string{
				"cmd",
				"/c",
				"ping",
				"-t",
				"127.0.0.1",
			},
			Windows: &runtime.WindowsContainerConfig{
				SecurityContext: &runtime.WindowsContainerSecurityContext{
					CredentialSpec: credSpec,
				},
			},
		},
		SandboxConfig: sandboxRequest.Config,
	}

	containerID := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerID)
	startContainer(t, client, ctx, containerID)
	defer stopContainer(t, client, ctx, containerID)

	// No klist and no powershell available
	cmd := []string{"cmd", "/c", "set", "USERDNSDOMAIN"}
	containerExecReq := &runtime.ExecSyncRequest{
		ContainerId: containerID,
		Cmd:         cmd,
		Timeout:     20,
	}
	r := execSync(t, client, ctx, containerExecReq)
	if r.ExitCode != 0 {
		t.Fatalf("failed with exit code %d running 'set USERDNSDOMAIN': %s", r.ExitCode, string(r.Stderr))
	}
	// Check for USERDNSDOMAIN environment variable. This acts as a way tell if a
	// user is joined to an Active Directory Domain and is successfully
	// authenticated as a domain identity.
	if !strings.Contains(string(r.Stdout), "USERDNSDOMAIN") {
		t.Fatalf("expected to see USERDNSDOMAIN entry")
	}
}

func Test_RunContainer_GMSA_WCOW_Hypervisor(t *testing.T) {
	requireFeatures(t, featureWCOWHypervisor, featureGMSA)

	credSpec := gmsaSetup(t)
	pullRequiredImages(t, []string{imageWindowsNanoserver})
	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxRequest := getRunPodSandboxRequest(t, wcowHypervisorRuntimeHandler)

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request := &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageWindowsNanoserver,
			},
			Command: []string{
				"cmd",
				"/c",
				"ping",
				"-t",
				"127.0.0.1",
			},
			Windows: &runtime.WindowsContainerConfig{
				SecurityContext: &runtime.WindowsContainerSecurityContext{
					CredentialSpec: credSpec,
				},
			},
		},
		SandboxConfig: sandboxRequest.Config,
	}

	containerID := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerID)
	startContainer(t, client, ctx, containerID)
	defer stopContainer(t, client, ctx, containerID)

	// No klist and no powershell available
	cmd := []string{"cmd", "/c", "set", "USERDNSDOMAIN"}
	containerExecReq := &runtime.ExecSyncRequest{
		ContainerId: containerID,
		Cmd:         cmd,
		Timeout:     20,
	}
	r := execSync(t, client, ctx, containerExecReq)
	if r.ExitCode != 0 {
		t.Fatalf("failed with exit code %d running 'set USERDNSDOMAIN': %s", r.ExitCode, string(r.Stderr))
	}
	// Check for USERDNSDOMAIN environment variable. This acts as a way tell if a
	// user is joined to an Active Directory Domain and is successfully
	// authenticated as a domain identity.
	if !strings.Contains(string(r.Stdout), "USERDNSDOMAIN") {
		t.Fatalf("expected to see USERDNSDOMAIN entry")
	}
}

func Test_RunContainer_SandboxDevice_LCOW(t *testing.T) {
	requireFeatures(t, featureLCOW)

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})

	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sandboxRequest := getRunPodSandboxRequest(t, lcowRuntimeHandler)
	sandboxRequest.Config.Linux = &runtime.LinuxPodSandboxConfig{
		SecurityContext: &runtime.LinuxSandboxSecurityContext{
			Privileged: true,
		},
	}

	podID := runPodSandbox(t, client, ctx, sandboxRequest)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	request := &runtime.CreateContainerRequest{
		PodSandboxId: podID,
		Config: &runtime.ContainerConfig{
			Metadata: &runtime.ContainerMetadata{
				Name: t.Name() + "-Container",
			},
			Image: &runtime.ImageSpec{
				Image: imageLcowAlpine,
			},
			Command: []string{
				"top",
			},

			Devices: []*runtime.Device{
				{
					HostPath: "/dev/fuse",
				},
			},
		},
		SandboxConfig: sandboxRequest.Config,
	}

	containerID := createContainer(t, client, ctx, request)
	defer removeContainer(t, client, ctx, containerID)
	startContainer(t, client, ctx, containerID)
	defer stopContainer(t, client, ctx, containerID)

	cmd := []string{"ls", "/dev/fuse"}
	containerExecReq := &runtime.ExecSyncRequest{
		ContainerId: containerID,
		Cmd:         cmd,
		Timeout:     20,
	}
	r := execSync(t, client, ctx, containerExecReq)
	if r.ExitCode != 0 {
		t.Fatalf("failed with exit code %d: %s", r.ExitCode, string(r.Stderr))
	}
	if string(r.Stdout) == "" {
		t.Fatal("did not find expected device /dev/fuse in container")
	}
}

func Test_RunContainer_NonDefault_User(t *testing.T) {
	requireFeatures(t, featureLCOW)

	type config struct {
		containerSecCtx *runtime.LinuxContainerSecurityContext
		name            string
	}
	client := newTestRuntimeClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pullRequiredLcowImages(t, []string{imageLcowK8sPause, imageLcowAlpine})

	podReq := getRunPodSandboxRequest(t, lcowRuntimeHandler)
	podID := runPodSandbox(t, client, ctx, podReq)
	defer removePodSandbox(t, client, ctx, podID)
	defer stopPodSandbox(t, client, ctx, podID)

	tests := []config{
		{
			containerSecCtx: &runtime.LinuxContainerSecurityContext{
				RunAsUsername: "guest",
			},
			name: "RunAsUsername",
		},
		{
			containerSecCtx: &runtime.LinuxContainerSecurityContext{
				RunAsUser: &runtime.Int64Value{
					Value: 10001,
				},
			},
			name: "RunAsUserUID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(_ *testing.T) {
			conReq := &runtime.CreateContainerRequest{
				Config: &runtime.ContainerConfig{
					Metadata: &runtime.ContainerMetadata{
						Name: t.Name() + "-Container",
					},
					Image: &runtime.ImageSpec{
						Image: imageLcowAlpine,
					},
					Command: []string{
						"top",
					},
					Linux: &runtime.LinuxContainerConfig{
						SecurityContext: test.containerSecCtx,
					},
				},
				PodSandboxId:  podID,
				SandboxConfig: podReq.Config,
			}

			containerID := createContainer(t, client, ctx, conReq)
			defer removeContainer(t, client, ctx, containerID)
			startContainer(t, client, ctx, containerID)
			defer stopContainer(t, client, ctx, containerID)
		})
	}
}
