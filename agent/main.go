package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var (
	// Flags
	agentSocket      string
	agentContainerID string
	agentTargetIP    string
	agentPort        string
	agentPodName     string
	agentContainer   string
)

// AgentCmd is the parent command for internal agent operations
var AgentCmd = &cobra.Command{
	Use:    "migrate-agent",
	Short:  "Internal agent commands",
	Hidden: true,
}

var SendCmd = &cobra.Command{
	Use:   "send",
	Short: "Checkpoint and stream container state",
	RunE:  runSend,
}

var ReceiveCmd = &cobra.Command{
	Use:   "receive",
	Short: "Receive stream and restore container",
	RunE:  runReceive,
}

func init() {
	AgentCmd.PersistentFlags().StringVar(&agentSocket, "socket", "/run/containerd/containerd.sock", "Containerd socket path")

	SendCmd.Flags().StringVar(&agentContainerID, "container-id", "", "Containerd Container ID to checkpoint")
	SendCmd.Flags().StringVar(&agentTargetIP, "target-ip", "", "Destination IP")
	SendCmd.Flags().StringVar(&agentPort, "port", "9000", "Destination Port")

	ReceiveCmd.Flags().StringVar(&agentPort, "port", "9000", "Listen Port")
	ReceiveCmd.Flags().StringVar(&agentPodName, "pod-name", "", "Target Pod Name (for restoration)")
	ReceiveCmd.Flags().StringVar(&agentContainer, "container-name", "", "Target Container Name")

	AgentCmd.AddCommand(SendCmd)
	AgentCmd.AddCommand(ReceiveCmd)
}

func runSend(cmd *cobra.Command, args []string) error {
	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")

	client, err := containerd.New(agentSocket)
	if err != nil {
		return fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer client.Close()

	// 1. Connect to Destination
	klog.Infof("Connecting to receiver at %s:%s...", agentTargetIP, agentPort)
	conn, err := net.Dial("tcp", net.JoinHostPort(agentTargetIP, agentPort))
	if err != nil {
		return fmt.Errorf("failed to dial destination: %w", err)
	}
	defer conn.Close()

	// 2. Load Container
	klog.Infof("Loading container %s...", agentContainerID)
	container, err := client.LoadContainer(ctx, agentContainerID)
	if err != nil {
		return fmt.Errorf("failed to load container: %w", err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	// 3. Pause & Checkpoint
	klog.Info("Checkpointing task...")
	// We create a temporary image reference for the checkpoint
	checkpointRef := fmt.Sprintf("checkpoint-%s", agentContainerID)

	// Checkpoint creates an image in the content store
	image, err := task.Checkpoint(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint failed: %w", err)
	}
	klog.Infof("Checkpoint created: %s", image.Name())

	// 4. Export & Stream
	// We export the checkpoint image content directly to the TCP connection
	klog.Info("Streaming checkpoint data...")
	err = client.Export(ctx, conn, archive.WithImage(client.ImageService(), checkpointRef))
	if err != nil {
		return fmt.Errorf("export failed: %w", err)
	}

	klog.Info("Stream complete.")
	return nil
}

func runReceive(cmd *cobra.Command, args []string) error {
	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")

	client, err := containerd.New(agentSocket)
	if err != nil {
		return fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer client.Close()

	// 1. Listen for Stream
	ln, err := net.Listen("tcp", ":"+agentPort)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	klog.Infof("Listening on %s...", agentPort)

	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("failed to accept connection: %w", err)
	}
	defer conn.Close()

	// 2. Import Stream
	klog.Info("Receiving and importing checkpoint...")
	// Import reads the stream and saves it to the content store
	imgs, err := client.Import(ctx, conn)
	if err != nil {
		return fmt.Errorf("import failed: %w", err)
	}
	if len(imgs) == 0 {
		return fmt.Errorf("no images imported")
	}
	// Convert core/images.Image to client.Image
	checkpointImg, err := client.GetImage(ctx, imgs[0].Name)
	if err != nil {
		return fmt.Errorf("failed to get imported image: %w", err)
	}
	klog.Infof("Imported checkpoint: %s", checkpointImg.Name())

	// TODO: Filesystem Sync
	// Currently we ignore filesystem synchronization.
	// We assume the rootfs (PVCs/Images) are available on this node.
	// Future work: Receive an archive of the rootfs diff before the checkpoint stream.

	// 3. Wait for Init Container (The Gate)
	// We need to find the Pod's sandbox and ensure the Init container is running
	// effectively blocking the main app container from starting.
	// K8s naming convention: k8s_<container-name>_<pod-name>_<namespace>_<uid>_<restart-count>
	klog.Info("Waiting for Mirror Pod Init Container...")
	var podSandboxID string

	// Retry loop to find the pod
	for {
		containers, err := client.Containers(ctx, fmt.Sprintf("labels.\"io.kubernetes.pod.name\"==\"%s\"", agentPodName))
		if err != nil {
			klog.Warningf("Error listing containers: %v", err)
		} else if len(containers) > 0 {
			// Found the pod components. Grab the sandbox ID from one of them (usually they share labels)
			// Or better, look for the init container specifically.
			for _, c := range containers {
				labels, _ := c.Labels(ctx)
				if labels["io.kubernetes.container.name"] == "migration-gate" {
					// Found the init container
					task, err := c.Task(ctx, nil)
					if err == nil {
						status, _ := task.Status(ctx)
						if status.Status == containerd.Running {
							// It is running!
							klog.Info("Init container is running. Proceeding with restore.")
							// Get the sandbox ID (label io.kubernetes.docker.type usually or sandbox id)
							// Actually containerd doesn't always expose sandbox ID easily in labels unless using CRI plugin conventions.
							// But for 'NewContainer' we might need to attach to the same namespaces.
							// For simplicity, we will attempt to create the container using standard K8s naming
							// and let Kubelet 'adopt' it or we just inject it into the namespace.
							podSandboxID = labels["io.kubernetes.pod.sandbox.id"]
							goto Ready
						}
					}
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
Ready:

	// 4. Restore Container
	klog.Infof("Restoring container %s into sandbox %s...", agentContainer, podSandboxID)

	// We construct the new container.
	// We must match Kubelet's naming convention so Kubelet can find it later (maybe).
	// k8s_<ContainerName>_<PodName>_<Namespace>_<UID>_<Attempt>
	// Note: Generating the exact same ID Kubelet expects is hard.
	// Instead, we might just create it and hope Kubelet sees "it exists".

	restoreName := fmt.Sprintf("k8s_%s_%s_%s_restored", agentContainer, agentPodName, "default") // Simplified naming

	newContainer, err := client.NewContainer(
		ctx,
		restoreName,
		containerd.WithNewSnapshot(restoreName+"-snapshot", checkpointImg),
		// containerd.WithNewSpec(cio.WithStdio), // We should copy spec from checkpoint, but simplified here
		// Critical: Join the Sandbox Namespaces (Net, IPC, UTS)
		// containerd.WithSpec(func(_ context.Context, _ *client.Client, _ *containers.Container, s *specs.Spec) error {
		//    set namespaces to podSandboxID
		//    return nil
		// }),
	)
	if err != nil {
		return fmt.Errorf("failed to create container structure: %w", err)
	}
	defer newContainer.Delete(ctx, containerd.WithSnapshotCleanup)

	// Create Task from Checkpoint
	newTask, err := newContainer.NewTask(ctx, cio.NewCreator(cio.WithStdio), containerd.WithTaskCheckpoint(checkpointImg))
	if err != nil {
		return fmt.Errorf("failed to create task from checkpoint: %w", err)
	}
	defer newTask.Delete(ctx)

	// Start (Restore)
	if err := newTask.Start(ctx); err != nil {
		return fmt.Errorf("failed to start restored task: %w", err)
	}
	klog.Info("Restored task started successfully.")

	// 5. Unblock Init Container
	// We can signal the init container to exit.
	// Since we share the filesystem (if configured) or just use a signal.
	// Simple hack: Kill the Init Container task.
	// Kubelet will see Init finished (if exit 0) and start App containers.
	// Wait, if Kubelet starts App container, it might conflict with our restored container?
	// This is the race condition.
	// Ideally, we replace the process. But for this Proof of Concept:
	// We signal the init container to exit.

	// Locate Init Container again
	initContainers, _ := client.Containers(ctx, fmt.Sprintf("labels.\"io.kubernetes.pod.name\"==\"%s\"", agentPodName))
	for _, c := range initContainers {
		l, _ := c.Labels(ctx)
		if l["io.kubernetes.container.name"] == "migration-gate" {
			t, err := c.Task(ctx, nil)
			if err == nil {
				// Kill it with signal 0 to stop? Or kill?
				// To make it "success", maybe we should have designed the init container to exit on file.
				// Since we are privileged, we can just write the file to the overlay? Hard.
				// Let's just kill it.
				t.Kill(ctx, syscall.SIGKILL)
			}
		}
	}

	// Keep the restored task alive (monitor)
	statusC, err := newTask.Wait(ctx)
	if err != nil {
		return err
	}
	<-statusC

	return nil
}

func main() {
	if err := AgentCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
