package run

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/aojea/krun/internal/assets"
	"github.com/aojea/krun/pkg/cdc"
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
	useShell       bool
)

var RunCmd = &cobra.Command{
	Use:   "run [flags] -- [command...]",
	Short: "Run a command or upload files to matching pods",
	Example: `  # Run a command on pods belonging to Pods labeled with app=backend
  krun run --label-selector=app=backend -- pip install -r requirements.txt

  # Upload files and run a script
  krun run --label-selector=app=backend --upload-src=./bin --upload-dest=/tmp/bin -- /tmp/bin/start.sh

  # Run shell commands with pipes, cd, or && using --shell flag
  krun run --label-selector=app=backend --shell -- "cd /app && pip install -r requirements.txt"
  krun run --label-selector=app=backend --shell -- "apt update && apt install -y vim"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmdArgs := []string{}
		if cmd.ArgsLenAtDash() != -1 {
			cmdArgs = args[cmd.ArgsLenAtDash():]
		}
		if useShell {
			cmdArgs = exec.WrapCommandInShell(cmdArgs)
		}
		opts := Options{
			Kubeconfig:     kubeconfig,
			Namespace:      namespace,
			LabelSelector:  labelSelector,
			UploadSrc:      uploadSrc,
			UploadDest:     uploadDest,
			ExcludePattern: excludePattern,
			Timeout:        timeout,
			CmdArgs:        cmdArgs,
		}
		// Pass the root context from cobra command
		return Run(cmd.Context(), opts)
	},
}

type Options struct {
	Kubeconfig     string
	Namespace      string
	LabelSelector  string
	UploadSrc      string
	UploadDest     string
	ExcludePattern string
	Timeout        time.Duration
	CmdArgs        []string
}

func Run(ctx context.Context, opts Options) error {
	// Validate inputs
	if len(opts.CmdArgs) == 0 && opts.UploadSrc == "" {
		return fmt.Errorf("you must provide either a command (as arguments) or --upload-src (or both)")
	}
	if opts.UploadSrc != "" && opts.UploadDest == "" {
		return fmt.Errorf("if --upload-src is provided, --upload-dest is required")
	}

	if opts.LabelSelector == "" {
		return fmt.Errorf("you must provide a --label-selector to select target pods")
	}

	// Compile exclude regex if provided
	var excludeRegex *regexp.Regexp
	if opts.ExcludePattern != "" {
		var err error
		excludeRegex, err = regexp.Compile(opts.ExcludePattern)
		if err != nil {
			return fmt.Errorf("invalid exclude pattern: %v", err)
		}
	}

	// Setup Context
	var ctxCancel context.CancelFunc
	if opts.Timeout > 0 {
		ctx, ctxCancel = context.WithTimeout(ctx, opts.Timeout)
	} else {
		ctx, ctxCancel = context.WithCancel(ctx)
	}
	defer ctxCancel()

	// Defer error handling for the metrics server
	defer runtime.HandleCrash()

	config, clientset, err := clientset.GetClient(opts.Kubeconfig)
	if err != nil {
		return err
	}

	klog.V(2).Infof("Listing pods in namespace %q with selector %q", opts.Namespace, opts.LabelSelector)
	pods, err := clientset.CoreV1().Pods(opts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: opts.LabelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}

	if len(pods.Items) == 0 {
		klog.Infoln("No pods found with selector:", opts.LabelSelector)
		return nil
	}

	klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods.Items))

	// 1. Upload Files (SyncPods)
	if opts.UploadSrc != "" {
		agentData, err := assets.GetAgentFsyncBinaryForArch()
		if err != nil {
			return fmt.Errorf("failed to get agent binary: %w", err)
		}

		err = exec.UploadExecutableOnPods(ctx, config, clientset, pods.Items, cdc.AgentFile, agentData)
		if err != nil {
			return fmt.Errorf("failed to upload executable: %w", err)
		}
		// Cleanup agent binary
		defer func() {
			// Use a new context so cleanup isn't cancelled
			cleanupCtx := context.Background()
			_ = exec.RemovePathsFromPods(cleanupCtx, config, clientset, pods.Items, cdc.AgentFile)
		}()

		err = cdc.SyncPods(ctx, config, clientset, pods.Items, opts.UploadSrc, opts.UploadDest, excludeRegex)
		if err != nil {
			return fmt.Errorf("failed to sync pods: %w", err)
		}
	}

	// 2. Execute Command
	if len(opts.CmdArgs) > 0 {
		return exec.ExecuteOnPods(ctx, config, clientset, pods.Items, opts.CmdArgs)
	}
	return nil
}

func init() {
	RunCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	RunCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	RunCmd.Flags().StringVarP(&labelSelector, "label-selector", "l", "", "Label selector for pods (e.g. app=my-app)")
	RunCmd.Flags().StringVar(&uploadSrc, "upload-src", "", "Local path to folder/file to upload")
	RunCmd.Flags().StringVar(&uploadDest, "upload-dest", "", "Remote path (e.g. /tmp/app)")
	RunCmd.Flags().StringVar(&excludePattern, "exclude", "", "Regex pattern to exclude files when uploading")
	RunCmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for the execution")
	RunCmd.Flags().BoolVar(&useShell, "shell", false, "Wrap command with 'sh -c' to enable shell features like pipes, &&, ||, and cd")
}
