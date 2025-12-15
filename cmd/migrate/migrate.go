package migrate

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aojea/krun/pkg/clientset"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const defaultBuilderImage = "ghcr.io/aojea/krun-agent:latest"

var (
	kubeconfig    string
	namespace     string
	container     string
	selector      string
	keepOld       bool
	builderImage  string
	snapshotImage string
)

var MigrateCmd = &cobra.Command{
	Use:   "migrate [POD_NAME]",
	Short: "Live-migrate a pod to a new image using checkpointing",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		podName := args[0]
		ctx := cmd.Context()

		// 1. Setup Clientset
		_, clientset, err := clientset.GetClient(kubeconfig)
		if err != nil {
			return err
		}

		// 2. Pre-flight checks & Source Info
		klog.Infof("🔍 Pre-flight checks for pod %s/%s...", namespace, podName)
		sourcePod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod: %w", err)
		}

		sourceNode := sourcePod.Spec.NodeName
		if sourceNode == "" {
			return fmt.Errorf("pod is not scheduled on a node")
		}

		// Identify Target Container (Single container support for now, or loop?)
		// The Agent 'send' command currently takes ONE container ID.
		// The Agent 'receive' command takes ONE container name.
		// We will assume single container migration for now or migrated sequentially.
		// Logic: If multiple containers, we might need multiple streams or sequential.
		// Let's stick to the first found container or the specified one.
		targetContainerName := container
		if targetContainerName == "" {
			if len(sourcePod.Spec.Containers) > 0 {
				targetContainerName = sourcePod.Spec.Containers[0].Name // Default to first
			}
		}

		// Find Container ID
		var sourceContainerID string
		for _, status := range sourcePod.Status.ContainerStatuses {
			if status.Name == targetContainerName {
				// Format: containerd://<id>
				parts := strings.Split(status.ContainerID, "://")
				if len(parts) == 2 {
					sourceContainerID = parts[1]
				}
				break
			}
		}
		if sourceContainerID == "" {
			return fmt.Errorf("failed to find container ID for %s (is it running?)", targetContainerName)
		}

		klog.Infof("📍 Source: %s on %s (ID: %s)", podName, sourceNode, sourceContainerID)

		// 3. Create Destination Pod (Mirror)
		destPodName := fmt.Sprintf("%s-migrated", podName)
		klog.Infof("🚀 Creating Destination Pod %s...", destPodName)

		destPod := sourcePod.DeepCopy()
		destPod.Name = destPodName
		destPod.ResourceVersion = ""
		destPod.UID = ""
		destPod.CreationTimestamp = metav1.Time{}
		destPod.Status = corev1.PodStatus{}
		destPod.Spec.NodeName = "" // Let scheduler pick (or use selector)

		// Add Init Container Gate
		// We'll invoke a blocking command.
		// Name must be "migration-gate" as expected by Agent Receive logic.
		initContainer := corev1.Container{
			Name:  "migration-gate",
			Image: "busybox", // We assume busybox is available or we use builderImage?
			// Using builderImage (krun-agent) is safer as we know we have it?
			// But krun-agent might be large? It's Debian based.
			// Let's use builderImage (Agent) as it's guaranteed to be pulled by us.
			Command: []string{"/bin/sh", "-c", "echo 'Blocking for migration...'; sleep infinity"},
		}
		// Prepend Init Container
		destPod.Spec.InitContainers = append([]corev1.Container{initContainer}, destPod.Spec.InitContainers...)

		// Update image if snapshotImage is provided
		if snapshotImage != "" {
			for i := range destPod.Spec.Containers {
				if destPod.Spec.Containers[i].Name == targetContainerName {
					destPod.Spec.Containers[i].Image = snapshotImage
					break
				}
			}
		}

		// Handle Selector
		if selector != "" {
			parts := strings.Split(selector, "=")
			if len(parts) == 2 {
				if destPod.Spec.NodeSelector == nil {
					destPod.Spec.NodeSelector = make(map[string]string)
				}
				destPod.Spec.NodeSelector[parts[0]] = parts[1]
			}
		}

		_, err = clientset.CoreV1().Pods(namespace).Create(ctx, destPod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create destination pod: %w", err)
		}

		// Wait for Scheduling (to know Dest Node)
		klog.Infof("⏳ Waiting for Destination Pod scheduling...")
		var destNode string
		// var destPodIP string // Removed unused variable

		// We wait for it to be assigned a Node. It might be stuck in Init:0/1, which is GOOD.
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			p, err := clientset.CoreV1().Pods(namespace).Get(ctx, destPodName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if p.Spec.NodeName != "" {
				destNode = p.Spec.NodeName
				// destPodIP = p.Status.PodIP
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("timeout waiting for destination pod scheduling: %w", err)
		}

		// 4. Start Receiver Agent (On Dest Node)
		receivePort := "9000" // Make flag?
		receiverPodName := fmt.Sprintf("migrator-receiver-%s", destNode)
		receiverPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: receiverPodName, Namespace: namespace},
			Spec: corev1.PodSpec{
				NodeName:      destNode,
				RestartPolicy: corev1.RestartPolicyNever,
				HostNetwork:   true, // Needed to listen on Node IP
				Containers: []corev1.Container{
					{
						Name:            "receiver",
						Image:           builderImage,
						Command:         []string{"/usr/local/bin/krun-agent", "migrate-agent", "receive", "--port", receivePort, "--pod-name", destPodName, "--container-name", targetContainerName, "--socket", "/run/containerd/containerd.sock"},
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
						VolumeMounts:    []corev1.VolumeMount{
							{Name: "run", MountPath: "/run/containerd"}, 
							{Name: "varlib", MountPath: "/var/lib/containerd"},
						},
					},
				},
				Volumes: []corev1.Volume{
					{Name: "run", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd"}}},
					{Name: "varlib", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/containerd"}}},
				},
			},
		}

		klog.Infof("🎧 Starting Receiver Agent on %s...", destNode)
		_, err = clientset.CoreV1().Pods(namespace).Create(ctx, receiverPod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to start receiver: %w", err)
		}
		defer func() {
			klog.Infof("🧹 Cleaning up receiver...")
			clientset.CoreV1().Pods(namespace).Delete(context.Background(), receiverPodName, metav1.DeleteOptions{})
		}()

		// Wait for Receiver to be Running
		var destNodeIP string
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			p, err := clientset.CoreV1().Pods(namespace).Get(ctx, receiverPodName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			if p.Status.PodIP == "" {
				return false, nil
			}
			// host network pod IPs are the node IPs
			destNodeIP = p.Status.PodIP
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("timeout waiting for receiver pod to start: %w", err)
		}

		klog.Infof("✅ Destination Scheduled: %s on %s (%s)", destPodName, destNode, destNodeIP)

		// 5. Start Sender Agent (On Source Node)
		// We launch it to run 'send' command.
		senderPodName := fmt.Sprintf("migrator-sender-%s", sourceNode)
		senderPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: senderPodName, Namespace: namespace},
			Spec: corev1.PodSpec{
				NodeName:      sourceNode,
				RestartPolicy: corev1.RestartPolicyNever,
				HostNetwork:   true,
				Containers: []corev1.Container{
					{
						Name:            "sender",
						Image:           builderImage,
						Command:         []string{"/usr/local/bin/krun-agent", "migrate-agent", "send", "--container-id", sourceContainerID, "--target-ip", destNodeIP, "--port", receivePort, "--socket", "/run/containerd/containerd.sock"},
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
						VolumeMounts:    []corev1.VolumeMount{{Name: "run", MountPath: "/run/containerd"}, {Name: "varlib", MountPath: "/var/lib/containerd"}},
					},
				},
				Volumes: []corev1.Volume{
					{Name: "run", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd"}}},
					{Name: "varlib", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/containerd"}}},
				},
			},
		}

		klog.Infof("📤 Starting Sender Agent on %s...", sourceNode)
		_, err = clientset.CoreV1().Pods(namespace).Create(ctx, senderPod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to start sender: %w", err)
		}
		defer func() {
			klog.Infof("🧹 Cleaning up sender...")
			clientset.CoreV1().Pods(namespace).Delete(context.Background(), senderPodName, metav1.DeleteOptions{})
		}()

		// Wait for Sender to Complete (Success or Fail)
		klog.Info("⏳ Waiting for Migration to complete...")
		// We watch the Sender pod status. If it succeeds (Completed), we assume migration done.
		// If Receiver fails, Sender should fail (connection broken).
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
			p, err := clientset.CoreV1().Pods(namespace).Get(ctx, senderPodName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if p.Status.Phase == corev1.PodSucceeded {
				return true, nil
			}
			if p.Status.Phase == corev1.PodFailed {
				// Retrieve logs
				logs, _ := getPodLogs(ctx, clientset, namespace, senderPodName)
				return false, fmt.Errorf("sender failed: %s", logs)
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("migration wait failed: %w", err)
		}

		klog.Info("🎉 Migration Stream Complete!")

		// Cleanup Old Pod
		if !keepOld {
			klog.Info("🗑️  Deleting old pod...")
			_ = clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
		}

		return nil
	},
}

func init() {
	MigrateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	MigrateCmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	MigrateCmd.Flags().StringVarP(&container, "container", "c", "", "Specific container to checkpoint (default: all containers)")
	MigrateCmd.Flags().StringVarP(&selector, "selector", "s", "", "Node selector for new pod (e.g. 'disktype=ssd')")
	MigrateCmd.Flags().BoolVar(&keepOld, "keep-old", false, "Do not delete old pod")
	MigrateCmd.Flags().StringVar(&builderImage, "builder-image", defaultBuilderImage, "Image used for the builder pod")
	MigrateCmd.Flags().StringVar(&snapshotImage, "snapshot-image", "", "Image name for the checkpoint snapshot")
	if env := os.Getenv("BUILDER_IMAGE"); env != "" {
		builderImage = env
	}
}

func getPodLogs(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) (string, error) {
	req := clientset.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer podLogs.Close()

	buf := new(strings.Builder)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}
