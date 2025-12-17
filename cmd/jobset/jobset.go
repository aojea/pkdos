package jobset

import (
	"fmt"
	"strings"
	"time"

	"github.com/aojea/krun/cmd/run"
	"github.com/aojea/krun/pkg/clientset"
	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	jobsetapi "sigs.k8s.io/jobset/api/jobset/v1alpha2"
	jobsetclient "sigs.k8s.io/jobset/client-go/clientset/versioned"
	"sigs.k8s.io/yaml"
)

const (
	DefaultExclude = `(^|/)\.`
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

	// launch subcommand flags
	deviceType string
	image      string
	dryRun     bool
	numSlices  int
	mirror     bool
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
		if name == "" {
			klog.Fatal("You must provide a --jobset-name to select target pods")
		}
		labelSelector := jobsetapi.JobSetNameKey + "=" + name

		cmdArgs := []string{}
		if cmd.ArgsLenAtDash() != -1 {
			cmdArgs = args[cmd.ArgsLenAtDash():]
		}

		opts := run.Options{
			Kubeconfig:     kubeconfig,
			Namespace:      namespace,
			LabelSelector:  labelSelector,
			UploadSrc:      uploadSrc,
			UploadDest:     uploadDest,
			ExcludePattern: excludePattern,
			Timeout:        timeout,
			CmdArgs:        cmdArgs,
		}

		return run.Run(cmd.Context(), opts)
	},
}

var LaunchSubcmd = &cobra.Command{
	Use:   "launch [flags]",
	Short: "Launch a jobset",
	Example: `  # Launch a TPU JobSet
  krun jobset launch --name=stoelinga --device-type=tpu-7x-16 --image=python:3.12

  # Launch a GPU JobSet
  krun jobset launch --name=stoelinga --device-type=gpu-l4-1 --image=nvidia/cuda:12.9.1-cudnn-devel-ubuntu24.04`,
	RunE: func(cmd *cobra.Command, args []string) error {

		// Create the JobSet
		js, err := GenerateJobSet(name, namespace, deviceType, image, "sleep infinity", numSlices)
		if err != nil {
			return fmt.Errorf("failed to generate jobset: %w", err)
		}

		if dryRun {
			// Set TypeMeta for correct YAML output
			js.TypeMeta = metav1.TypeMeta{
				APIVersion: jobsetapi.SchemeGroupVersion.String(),
				Kind:       "JobSet",
			}
			yamlData, err := yaml.Marshal(js)
			if err != nil {
				return fmt.Errorf("failed to marshal jobset to yaml: %w", err)
			}
			fmt.Println(string(yamlData))
			return nil
		}

		// Setup Context
		ctx := cmd.Context()
		// Defer error handling for the metrics server
		defer runtime.HandleCrash()

		config, _, err := clientset.GetClient(kubeconfig)
		if err != nil {
			return err
		}
		// creates the clientset
		clientset, err := jobsetclient.NewForConfig(config)
		if err != nil {
			klog.Fatalf("can not create client-go client: %v", err)
		}

		klog.Infof("Creating JobSet %q in namespace %q with device type %q...", name, namespace, deviceType)
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
	JobSetCmd.PersistentFlags().StringVarP(&name, "name", "j", "", "Name of the JobSet")

	// Subcommand to run commands/upload files to pods in the JobSet
	JobSetCmd.AddCommand(RunSubcmd)
	RunSubcmd.Flags().StringVar(&uploadSrc, "upload-src", "", "Local path to folder/file to upload")
	RunSubcmd.Flags().StringVar(&uploadDest, "upload-dest", "", "Remote path (e.g. /tmp/app)")
	RunSubcmd.Flags().StringVar(&excludePattern, "exclude", DefaultExclude, "Regex pattern to exclude files when uploading (default excludes all hidden files and folders)")
	RunSubcmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for the execution")
	RunSubcmd.Flags().BoolVar(&mirror, "mirror", false, "Mirror destination (delete extraneous files in destination)")

	JobSetCmd.AddCommand(LaunchSubcmd)
	LaunchSubcmd.Flags().StringVar(&deviceType, "device-type", "tpu-7x-16", "Type of accelerator to launch (e.g. tpu-7x-16, gpu-l4-1)")
	LaunchSubcmd.Flags().StringVar(&image, "image", "ubuntu:24.04", "Container image to use for the workers")
	LaunchSubcmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the JobSet yaml without creating it")
	LaunchSubcmd.Flags().IntVar(&numSlices, "num-slices", 1, "Number of slices (replicas) to launch")

}

// GenerateJobSet creates the K8s JobSet object based on the device-type
func GenerateJobSet(name, namespace, deviceTypeString, imageName, cmd string, numSlices int) (*jobsetapi.JobSet, error) {

	// 1. Get System Characteristics
	sysChar, err := GetSystemCharacteristics(deviceTypeString)
	if err != nil {
		return nil, err
	}

	accChar, ok := acceleratorTypeToCharacteristics[sysChar.AcceleratorType]
	if !ok {
		return nil, fmt.Errorf("unknown accelerator type: %s", sysChar.AcceleratorType)
	}

	// 2. Calculate Resources and Node Selectors
	nodeSelector := map[string]string{}
	if accChar.MachineLabel != "" {
		switch sysChar.AcceleratorType {
		case AcceleratorTypeTPU:
			nodeSelector[accChar.MachineLabel] = sysChar.Topology
		case AcceleratorTypeGPU:
			nodeSelector[accChar.MachineLabel] = sysChar.GCEMachineType
		}
	}
	if accChar.AcceleratorLabel != "" {
		nodeSelector[accChar.AcceleratorLabel] = sysChar.GKEAccelerator
	}

	resourceList := corev1.ResourceList{}
	if sysChar.AcceleratorType == AcceleratorTypeTPU || sysChar.AcceleratorType == AcceleratorTypeGPU {
		resourceList[corev1.ResourceName(accChar.ResourceType)] = resource.MustParse(fmt.Sprintf("%d", sysChar.ChipsPerVM))
	}

	// 3. Construct JobSet
	// Calculate parallelism and completions
	// For TPU: Parallelism = Completions = VMsPerSlice (assuming 1 slice for now)
	// For GPU/CPU: Parallelism = Completions = VMsPerSlice (usually 1, but can be more for multi-node)
	// The Python code has vms_per_slice.
	// If we assume we are launching 1 slice (which seems to be the case for "launch a jobset"), then:
	numNodes := int32(sysChar.VMsPerSlice)
	replicas := int32(numSlices)

	jobSet := &jobsetapi.JobSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: jobsetapi.JobSetSpec{
			ReplicatedJobs: []jobsetapi.ReplicatedJob{
				{
					Name:     "j", // Single letter to keep pod names short
					Replicas: replicas,
					Template: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Parallelism:  &numNodes,                             // Run on 'numNodes' pods simultaneously
							Completions:  &numNodes,                             // Job is done when all pods finish
							BackoffLimit: func(i int32) *int32 { return &i }(0), // Fail fast for this demo
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									NodeSelector:  nodeSelector,
									Containers: []corev1.Container{
										{
											Name:    "workload",
											Image:   imageName,
											Command: strings.Split(cmd, " "),
											Resources: corev1.ResourceRequirements{
												Limits:   resourceList,
												Requests: resourceList,
											},
											Env: []corev1.EnvVar{
												{
													Name:  "DEVICE_TYPE",
													Value: deviceTypeString,
												},
												{
													Name:  "ACCELERATOR_TYPE",
													Value: string(sysChar.AcceleratorType),
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
