package run

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aojea/krun/pkg/exec"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
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
	Example: `  # Run a command on pods belonging to a JobSet
  krun run --jobset-name=stoelinga -- pip install -r requirements.txt

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

		var config *rest.Config
		var err error
		if kubeconfig == "" {
			if home := homedir.HomeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			} else {
				kubeconfig = os.Getenv("KUBECONFIG")
			}
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			klog.Fatalf("can not create client-go configuration: %v", err)
		}

		// creates the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Fatalf("can not create client-go client: %v", err)
		}

		klog.V(2).Infof("Listing pods in namespace %q with selector %q", namespace, labelSelector)
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			klog.Fatalf("failed to get pods: %v", err)
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
