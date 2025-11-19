package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

var (
	kubeconfig    = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	labelSelector = flag.String("label-selector", "app=my-app", "Label selector for pods")
	namespace     = flag.String("namespace", "default", "Kubernetes namespace")
	commandStr    = flag.String("command", "echo Hello from $(hostname)", "Command to execute in pods")
	timeout       = flag.Duration("timeout", 30*time.Second, "Timeout for the execution")
)

func main() {
	klog.InitFlags(nil)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: krun [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	flag.VisitAll(func(f *flag.Flag) {
		klog.Infof("FLAG: --%s=%q", f.Name, f.Value)
	})

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctx, cancel := context.WithTimeout(rootCtx, *timeout)
	defer cancel()

	// Defer error handling for the metrics server
	defer runtime.HandleCrash()

	var config *rest.Config
	var err error
	if *kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			*kubeconfig = filepath.Join(home, ".kube", "config")
		} else {
			*kubeconfig = os.Getenv("KUBECONFIG")
		}
	}
	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		klog.Fatalf("can not create client-go configuration: %v", err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("can not create client-go client: %v", err)
	}

	// Find the pods to execute on
	pods, err := clientset.CoreV1().Pods(*namespace).List(ctx, metav1.ListOptions{
		LabelSelector: *labelSelector,
	})
	if err != nil {
		klog.Fatalf("failed to get pods: %v", err)
	}

	if len(pods.Items) == 0 {
		klog.Infoln("No pods found with selector:", labelSelector)
		os.Exit(0)
	}

	klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods.Items))

	cmdArray := strings.Fields(*commandStr)

	var wg sync.WaitGroup
	var printMutex sync.Mutex

	for _, pod := range pods.Items {
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()

			// Setup Pipe for stdout
			prStdout, pwStdout := io.Pipe()
			prStderr, pwStderr := io.Pipe()

			// Log Processor Goroutine
			go func() {
				scanner := bufio.NewScanner(prStdout)
				for scanner.Scan() {
					line := scanner.Text()
					printMutex.Lock()
					_, _ = fmt.Fprintf(os.Stdout, "[%s] %s\n", p.Name, line)
					printMutex.Unlock()
				}
			}()
			go func() {
				scanner := bufio.NewScanner(prStderr)
				for scanner.Scan() {
					line := scanner.Text()
					printMutex.Lock()
					_, _ = fmt.Fprintf(os.Stderr, "[%s] %s\n", p.Name, line)
					printMutex.Unlock()
				}
			}()
			// Execute Command
			err := execCmd(ctx, config, clientset, p.Name, p.Namespace, cmdArray, pwStdout, pwStderr)
			_ = pwStdout.Close() // Close pipe so scanner finishes
			_ = pwStderr.Close() // Close pipe so scanner finishes

			if err != nil {
				printMutex.Lock()
				if ctx.Err() == context.DeadlineExceeded {
					klog.Errorf("[%s] Execution timed out!", p.Name)
				} else if ctx.Err() == context.Canceled {
					klog.Errorf("[%s] Execution cancelled.", p.Name)
				} else {
					klog.Errorf("[%s] Execution Error: %v", p.Name, err)
				}
				printMutex.Unlock()
			}
		}(pod)
	}

	wg.Wait()

	// Check if we finished because of a timeout
	if ctx.Err() == context.DeadlineExceeded {
		klog.Infoln("\nProcess finished due to TIMEOUT.")
		os.Exit(1)
	}
	klog.Infoln("\nAll executions finished.")
	os.Exit(0)
}

// execCmd handles the specific API request to upgrade connection to SPDY
func execCmd(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, podName, namespace string, command []string, stdout io.Writer, stderr io.Writer) error {

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: command,
		Stdout:  true,
		Stderr:  true,
		TTY:     false, // Set to true if you need a TTY
	}

	// ParameterCodec is required to encode the options
	req.VersionedParams(option, scheme.ParameterCodec)

	// Create the executor
	exec, err := remotecommand.NewWebSocketExecutor(config, "GET", req.URL().String())
	if err != nil {
		return err
	}

	// Stream the output.
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	})

	return err
}
