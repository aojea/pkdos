package run

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/aojea/krun/pkg/clientset"
	"github.com/aojea/krun/pkg/exec"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// Global variables for flags
var (
	kubeconfig     string
	namespace      string
	labelSelector  string
	uploadSrc      string
	uploadDest     string
	timeout        time.Duration
	excludePattern string
	excludeRegex   *regexp.Regexp
)

var RunCmd = &cobra.Command{
	Use:   "run [flags] -- [command...]",
	Short: "Run a command or upload files to matching pods",
	Example: `  # Run a command on pods belonging to Pods labeled with app=backend
  krun run --label-selector=app=backend -- pip install -r requirements.txt

  # Upload files and run a script
  krun run --label-selector=app=backend --upload-src=./bin --upload-dest=/tmp/bin -- /tmp/bin/start.sh`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate inputs
		if len(args) == 0 && uploadSrc == "" {
			klog.Fatal("You must provide either a command (as arguments) or --upload-src (or both)")
		}
		if uploadSrc != "" && uploadDest == "" {
			klog.Fatal("If --upload-src is provided, --upload-dest is required")
		}

		if labelSelector == "" {
			klog.Fatal("You must provide a --label-selector to select target pods")
		}

		// Compile exclude regex if provided
		if excludePattern != "" {
			var err error
			excludeRegex, err = regexp.Compile(excludePattern)
			if err != nil {
				klog.Fatalf("Invalid exclude pattern: %v", err)
			}
		}

		// Setup Context
		rootCtx := cmd.Context()
		var ctx context.Context
		var ctxCancel context.CancelFunc
		if timeout > 0 {
			ctx, ctxCancel = context.WithTimeout(rootCtx, timeout)
		} else {
			ctx, ctxCancel = context.WithCancel(rootCtx)
		}
		defer ctxCancel()

		// Defer error handling for the metrics server
		defer runtime.HandleCrash()

		config, clientset, err := clientset.GetClient(kubeconfig)
		if err != nil {
			return err
		}

		klog.V(2).Infof("Listing pods in namespace %q with selector %q", namespace, labelSelector)
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return fmt.Errorf("failed to get pods: %w", err)
		}

		if len(pods.Items) == 0 {
			klog.Infoln("No pods found with selector:", labelSelector)
			return nil
		}

		klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods.Items))

		cmdArgs := []string{}
		if cmd.ArgsLenAtDash() != -1 {
			cmdArgs = args[cmd.ArgsLenAtDash():]
		}

		return exec.UploadAndExecuteOnPods(ctx, config, clientset, pods.Items, uploadSrc, uploadDest, excludeRegex, cmdArgs)
	},
}

func init() {
	RunCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	RunCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	RunCmd.Flags().StringVarP(&labelSelector, "label-selector", "l", "", "Label selector for pods (e.g. app=my-app)")
	RunCmd.Flags().StringVar(&uploadSrc, "upload-src", "", "Local path to folder/file to upload")
	RunCmd.Flags().StringVar(&uploadDest, "upload-dest", "", "Remote path (e.g. /tmp/app)")
	RunCmd.Flags().StringVar(&excludePattern, "exclude", "", "Regex pattern to exclude files when uploading")
	RunCmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for the execution")
}
