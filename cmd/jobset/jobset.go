package jobset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aojea/krun/pkg/exec"
	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"

	jobsetapi "sigs.k8s.io/jobset/api/jobset/v1alpha2"
	jobsetclient "sigs.k8s.io/jobset/client-go/clientset/versioned"
)

const (
	regexpDefaultExclude = `(^|/)\.`
)

// Global variables for flags
var (
	kubeconfig string
	namespace  string
	name       string
	// run subcommand flags
	uploadSrc      string
	uploadDest     string
	timeout        time.Duration
	excludePattern string
	excludeRegex   *regexp.Regexp
	// launch subcommand flags
	tpuType string
	image   string
)

var JobSetCmd = &cobra.Command{
	Use: "jobset [flags] [subcommand]",
}

var RunSubcmd = &cobra.Command{
	Use:   "run [flags] -- [command...]",
	Short: "Run a command or upload files to matching pods",
	Example: `  # Run a command on pods belonging to a JobSet
  krun jobset run --name=stoelinga -- pip install -r requirements.txt

  # Upload files and run a script
  krun jobset run --name=stoelinga --upload-src=./bin --upload-dest=/tmp/bin -- /tmp/bin/start.sh`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate inputs
		if len(args) == 0 && uploadSrc == "" {
			klog.Fatal("You must provide either a command (as arguments) or --upload-src (or both)")
		}
		if uploadSrc != "" && uploadDest == "" {
			klog.Fatal("If --upload-src is provided, --upload-dest is required")
		}

		if name == "" {
			klog.Fatal("You must provide a --jobset-name to select target pods")
		}
		labelSelector := jobsetapi.JobSetNameKey + "=" + name

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

var LaunchSubcmd = &cobra.Command{
	Use:   "launch [flags]",
	Short: "Launch a jobset",
	Example: `  # Run a command on pods belonging to a JobSet
  krun jobset run --name=stoelinga -- pip install -r requirements.txt

  # Upload files and run a script
  krun jobset run --name=stoelinga --upload-src=./bin --upload-dest=/tmp/bin -- /tmp/bin/start.sh`,
	RunE: func(cmd *cobra.Command, args []string) error {

		// Setup Context
		ctx := cmd.Context()
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
		clientset, err := jobsetclient.NewForConfig(config)
		if err != nil {
			klog.Fatalf("can not create client-go client: %v", err)
		}

		// Create the JobSet
		js, err := GenerateTPUJobSet(name, namespace, tpuType, image, "sleep infinity")
		if err != nil {
			return fmt.Errorf("failed to generate jobset: %w", err)
		}
		klog.Infof("Creating JobSet %q in namespace %q with accelerator %q...", name, namespace, tpuType)
		createdJS, err := clientset.JobsetV1alpha2().JobSets(namespace).Create(ctx, js, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create jobset: %w", err)
		}

		klog.Infof("JobSet %q created successfully.", createdJS.Name)
		return nil
	},
}

func init() {
	JobSetCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	JobSetCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	JobSetCmd.Flags().StringVarP(&name, "jobset-name", "j", "", "Name of the JobSet")

	// Subcommand to run commands/upload files to pods in the JobSet
	JobSetCmd.AddCommand(RunSubcmd)
	RunSubcmd.Flags().StringVar(&uploadSrc, "upload-src", "./", "Local path to folder/file to upload (default is current directory)")
	RunSubcmd.Flags().StringVar(&uploadDest, "upload-dest", "./", "Remote path (e.g. /tmp/app) (default is current directory)")
	RunSubcmd.Flags().StringVar(&excludePattern, "exclude", regexpDefaultExclude, "Regex pattern to exclude files when uploading (default excludes all hidden files and folders)")
	RunSubcmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for the execution")

	JobSetCmd.AddCommand(LaunchSubcmd)
	LaunchSubcmd.Flags().StringVar(&tpuType, "tpu-type", "tpu7x-16", "Type of TPU to launch and topology")
	LaunchSubcmd.Flags().StringVar(&image, "image", "gcr.io/tensorflow/tensorflow:latest", "Container image to use for the TPU workers")

}

// TPUHardwareSpec defines the physical properties of a TPU generation
type TPUHardwareSpec struct {
	AcceleratorLabel string // e.g., "tpu-v4-pod", "tpu-v5-lite-podslice"
	ChipsPerNode     int    // How many chips exist on a single physical node
}

// KnownTPUs maps the parsed prefix to hardware specs.
// In a real app, this might be config-driven.
var KnownTPUs = map[string]TPUHardwareSpec{
	"v4":        {AcceleratorLabel: "tpu-v4-pod", ChipsPerNode: 4}, // Note: varies by topology, simplified for example
	"v5litepod": {AcceleratorLabel: "tpu-v5-lite-podslice", ChipsPerNode: 8},
	"v5p":       {AcceleratorLabel: "tpu-v5p-slice", ChipsPerNode: 8},
	"tpu7x":     {AcceleratorLabel: "tpu-v7x-slice", ChipsPerNode: 8}, // Hypothetical example from your prompt
}

// ParseTPUType splits "tpu7x-64" into "tpu7x" (type) and 64 (total chips)
func ParseTPUType(input string) (string, int, error) {
	// Find the last hyphen to separate type from count
	lastDash := strings.LastIndex(input, "-")
	if lastDash == -1 {
		return "", 0, fmt.Errorf("invalid format: expected [type]-[chips], got %s", input)
	}

	tpuType := input[:lastDash]
	chipsStr := input[lastDash+1:]

	chips, err := strconv.Atoi(chipsStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid chip count: %v", err)
	}

	return tpuType, chips, nil
}

// GenerateTPUJobSet creates the K8s JobSet object based on the tpu-type
func GenerateTPUJobSet(name, namespace, tpuTypeString, imageName, cmd string) (*jobsetapi.JobSet, error) {

	// 1. Parse the Input
	acceleratorType, totalChips, err := ParseTPUType(tpuTypeString)
	if err != nil {
		return nil, err
	}

	// 2. Look up Hardware Specs
	spec, exists := KnownTPUs[acceleratorType]
	if !exists {
		// Fallback or error if type is unknown
		return nil, fmt.Errorf("unknown tpu type: %s", acceleratorType)
	}

	// If we need 64 chips and there are 8 chips per node, we need 8 nodes.
	if totalChips%spec.ChipsPerNode != 0 {
		return nil, fmt.Errorf("requested %d chips, but must be a multiple of %d (chips per node)", totalChips, spec.ChipsPerNode)
	}
	numNodes := int32(totalChips / spec.ChipsPerNode)

	// Standard GKE TPU Resource Request
	tpuResource := corev1.ResourceList{
		"google.com/tpu": resource.MustParse(fmt.Sprintf("%d", spec.ChipsPerNode)),
	}

	jobSet := &jobsetapi.JobSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: jobsetapi.JobSetSpec{
			ReplicatedJobs: []jobsetapi.ReplicatedJob{
				{
					Name: "main", // The main workload
					Template: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Parallelism:  &numNodes,                             // Run on 'numNodes' pods simultaneously
							Completions:  &numNodes,                             // Job is done when all pods finish
							BackoffLimit: func(i int32) *int32 { return &i }(0), // Fail fast for this demo
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									NodeSelector: map[string]string{
										"cloud.google.com/gke-tpu-accelerator": spec.AcceleratorLabel,
										"cloud.google.com/gke-tpu-topology":    tpuTypeString, // Often passed as topology label
									},
									Containers: []corev1.Container{
										{
											Name:    "workload",
											Image:   imageName,
											Command: strings.Split(cmd, " "),
											Resources: corev1.ResourceRequirements{
												Limits:   tpuResource,
												Requests: tpuResource,
											},
											Env: []corev1.EnvVar{
												{
													Name:  "TPU_TYPE",
													Value: tpuTypeString,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return jobSet, nil
}
