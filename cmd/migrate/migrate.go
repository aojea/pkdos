package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aojea/krun/pkg/clientset"
	"github.com/aojea/krun/pkg/exec"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

var (
	kubeconfig   string
	namespace    string
	container    string
	selector     string
	keepOld      bool
	builderImage string
)

var MigrateCmd = &cobra.Command{
	Use:   "migrate [POD_NAME] [SNAPSHOT_IMAGE]",
	Short: "Live-migrate a pod to a new image using checkpointing",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		podName := args[0]
		snapshotImage := args[1]
		ctx := cmd.Context()

		// 1. Setup Clientset
		config, clientset, err := clientset.GetClient(kubeconfig)
		if err != nil {
			return err
		}

		// 2. Pre-flight checks
		klog.Infof("🔍 Pre-flight checks for pod %s/%s...", namespace, podName)
		targetPod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod: %w", err)
		}

		nodeName := targetPod.Spec.NodeName
		if nodeName == "" {
			return fmt.Errorf("pod is not scheduled on a node")
		}

		// 3. Identify Target Containers
		var targetContainers []string
		if container != "" {
			found := false
			for _, c := range targetPod.Spec.Containers {
				if c.Name == container {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("container %q not found in pod", container)
			}
			targetContainers = []string{container}
		} else {
			for _, c := range targetPod.Spec.Containers {
				targetContainers = append(targetContainers, c.Name)
			}
			klog.Infof("ℹ️  Migrating all containers: %v", targetContainers)
		}

		klog.Infof("📍 Target: %s / %v on node %s", podName, targetContainers, nodeName)

		// 4. Create Builder Pod
		builderPodName := fmt.Sprintf("migrator-builder-%d", rand.Intn(100000))
		builderPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      builderPodName,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				NodeName:      nodeName,
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:    "builder",
						Image:   builderImage,
						Command: []string{"/bin/sh", "-c", "cp $(command -v criu) /host/usr/local/bin/criu && sleep 900"},
						SecurityContext: &corev1.SecurityContext{
							Privileged: func(b bool) *bool { return &b }(true),
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "h", MountPath: "/host"},
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "h",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/",
								Type: ptr.To(corev1.HostPathDirectory),
							},
						},
					},
				},
			},
		}

		klog.Infof("🚀 Pre-warming builder (%s)...", builderPodName)
		_, err = clientset.CoreV1().Pods(namespace).Create(ctx, builderPod, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create builder pod: %w", err)
		}

		// Ensure cleanup
		defer func() {
			klog.Infof("🧹 Cleaning up builder pod and injected binaries...")
			// Try to remove the injected binary using the builder pod if it's still running
			if err := exec.ExecCmd(ctx, config, clientset, *builderPod, []string{"rm", "-f", "/host/usr/local/bin/criu"}, nil, io.Discard, io.Discard); err != nil {
				klog.V(2).Infof("failed to cleanup criu binary: %v", err)
			}
			_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), builderPodName, metav1.DeleteOptions{
				GracePeriodSeconds: ptr.To(int64(0)),
			})
		}()

		// Wait for builder ready
		klog.Info("⏳ Waiting for builder...")
		if err := waitForPodReady(ctx, clientset, namespace, builderPodName); err != nil {
			return fmt.Errorf("builder pod failed to start: %w", err)
		}

		// 5. Trigger Checkpoints (Sequential for safety)
		containerCheckpoints := make(map[string]string)
		for _, cName := range targetContainers {
			klog.Infof("📸 TRIGGERING CHECKPOINT for container %s...", cName)
			// Path: /api/v1/nodes/$NODE/proxy/checkpoint/$NAMESPACE/$POD_NAME/$CONTAINER_NAME
			path := fmt.Sprintf("api/v1/nodes/%s/proxy/checkpoint/%s/%s/%s", nodeName, namespace, podName, cName)

			resultRaw, err := clientset.CoreV1().RESTClient().Post().
				AbsPath(path).
				SetHeader("Content-Type", "application/json").
				Body([]byte("{}")).
				DoRaw(ctx)
			if err != nil {
				return fmt.Errorf("checkpoint request failed for container %s: %w", cName, err)
			}

			var cpResult struct {
				Items []string `json:"items"`
			}
			if err := json.Unmarshal(resultRaw, &cpResult); err != nil {
				return fmt.Errorf("failed to parse checkpoint response for container %s: %w", cName, err)
			}
			if len(cpResult.Items) == 0 {
				return fmt.Errorf("no checkpoint items returned for container %s: %s", cName, string(resultRaw))
			}
			checkpointPath := cpResult.Items[0]
			containerCheckpoints[cName] = checkpointPath
			klog.Infof("✅ Saved %s: %s", cName, checkpointPath)
		}

		// 6. Parallel Execution (Push & Schedule)
		klog.Info("⚡ ENTERING PARALLEL EXECUTION MODE")

		var wg sync.WaitGroup
		// errChan capacity: Thread A (one per container) + Thread B (schedule)
		errChan := make(chan error, len(targetContainers)+1)

		// Thread A: Image Build & Push (One goroutine per container)
		for _, cName := range targetContainers {
			wg.Add(1)
			go func(cName string) {
				defer wg.Done()
				klog.Infof("   [Thread A-%s] 📦 Packaging & Pushing Image...", cName)

				// Determine Target Image
				// If multiple containers, append suffix.
				// If single container (even if implicitly selected), keep original.
				finalSnapshotImage := snapshotImage
				if len(targetContainers) > 1 {
					finalSnapshotImage = fmt.Sprintf("%s-%s", snapshotImage, cName)
				}

				checkpointPath := containerCheckpoints[cName]

				// Build script to run inside the builder pod
				remoteScript := fmt.Sprintf(`
set -e
if ! buildah login --get-login %s >/dev/null 2>&1; then echo 'Warn: No auth for registry'; fi
ctr=$(buildah from scratch)
buildah add "$ctr" "/host%s" /
buildah config --annotation=org.criu.checkpoint.container.name=restored "$ctr"
buildah commit "$ctr" "%s"
buildah push "%s"
`, finalSnapshotImage, checkpointPath, finalSnapshotImage, finalSnapshotImage)

				// Execute using the exported ExecCmd from pkg/exec
				// We discard stdout/stderr unless there is an error to keep output clean,
				// or we can stream it if verbose. For now, we print logs on error.
				cmd := []string{"/bin/sh", "-c", remoteScript}

				// We can pipe output to os.Stdout to show buildah progress
				if err := exec.ExecCmd(ctx, config, clientset, *builderPod, cmd, nil, os.Stdout, os.Stderr); err != nil {
					klog.Errorf("   [Thread A-%s] ❌ Image Push Failed: %v", cName, err)
					errChan <- fmt.Errorf("image push failed for %s: %w", cName, err)
					return
				}
				klog.Infof("   [Thread A-%s] ✅ Image Push Complete (%s).", cName, finalSnapshotImage)
			}(cName)
		}

		// Thread B: Schedule New Pod
		wg.Add(1)
		go func() {
			defer wg.Done()
			klog.Infof("   [Thread B] 🚀 Scheduling new Pod %s-migrated...", podName)

			// Construct new Pod
			newPod := targetPod.DeepCopy()
			newPod.Name = fmt.Sprintf("%s-migrated", podName)
			newPod.ResourceVersion = ""
			newPod.UID = ""
			newPod.CreationTimestamp = metav1.Time{}
			newPod.Status = corev1.PodStatus{}
			newPod.OwnerReferences = nil
			newPod.Spec.NodeName = "" // Clear node name to allow rescheduling

			// Update Images
			// We must apply the SAME naming logic here
			targetImage := snapshotImage // Use snapshotImage as base
			for i, c := range newPod.Spec.Containers {
				// check if this container was migrated
				isTarget := false
				for _, t := range targetContainers {
					if t == c.Name {
						isTarget = true
						break
					}
				}

				if isTarget {
					finalTargetImage := targetImage
					if len(targetContainers) > 1 {
						finalTargetImage = fmt.Sprintf("%s-%s", targetImage, c.Name)
					}
					newPod.Spec.Containers[i].Image = finalTargetImage
				}
			}

			// Handle Node Selector
			if selector != "" {
				parts := strings.Split(selector, "=")
				if len(parts) == 2 {
					if newPod.Spec.NodeSelector == nil {
						newPod.Spec.NodeSelector = make(map[string]string)
					}
					newPod.Spec.NodeSelector[parts[0]] = parts[1]
				}
			}

			// Delete old pod if requested (Non-blocking usually, but here we just trigger it)
			if !keepOld {
				klog.Info("   [Thread B] 🗑️  Deleting old pod...")
				zero := int64(0)
				_ = clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
					GracePeriodSeconds: &zero,
				})
			}

			// Apply new manifest
			_, err := clientset.CoreV1().Pods(namespace).Create(ctx, newPod, metav1.CreateOptions{})
			if err != nil {
				klog.Errorf("   [Thread B] ❌ Failed to create new pod: %v", err)
				errChan <- fmt.Errorf("failed to create new pod: %w", err)
				return
			}
			klog.Info("   [Thread B] ⏳ Pod created. Kubelet is now polling for image...")
		}()

		wg.Wait()
		close(errChan)

		// Check for errors
		for e := range errChan {
			if e != nil {
				return e
			}
		}

		klog.Info("🎉 Migration Complete!")
		return nil
	},
}

func init() {
	MigrateCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	MigrateCmd.Flags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	MigrateCmd.Flags().StringVarP(&container, "container", "c", "", "Specific container to checkpoint (default: all containers)")
	MigrateCmd.Flags().StringVarP(&selector, "selector", "s", "", "Node selector for new pod (e.g. 'disktype=ssd')")
	MigrateCmd.Flags().BoolVar(&keepOld, "keep-old", false, "Do not delete old pod")
	MigrateCmd.Flags().StringVar(&builderImage, "builder-image", "ghcr.io/aojea/krun-migrate:v0.0.1", "Image used for the builder pod")
	if env := os.Getenv("BUILDER_IMAGE"); env != "" {
		builderImage = env
	}
}

func waitForPodReady(ctx context.Context, clientset *kubernetes.Clientset, ns, name string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			p, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if p.Status.Phase == corev1.PodRunning {
				return nil
			}
		}
	}
}
