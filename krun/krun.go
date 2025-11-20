package main

import (
	"archive/tar"
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
	uploadSrc     = flag.String("upload-src", "", "Local path to folder/file to upload")
	uploadDest    = flag.String("upload-dest", "", "Remote path (e.g. /tmp/app)")
	timeout       = flag.Duration("timeout", 30*time.Second, "Timeout for the execution")
)

func main() {
	klog.InitFlags(nil)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: krun [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if klog.V(2).Enabled() {
		flag.VisitAll(func(f *flag.Flag) {
			klog.Infof("FLAG: --%s=%q", f.Name, f.Value)
		})
	}

	// We allow command ONLY, upload ONLY, or BOTH.
	if *commandStr == "" && *uploadSrc == "" {
		klog.Fatal("You must provide either --command or --upload-src (or both)")
	}
	if *uploadSrc != "" && *uploadDest == "" {
		klog.Fatal("If --upload-src is provided, --upload-dest is required")
	}

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

	var wg sync.WaitGroup
	var printMutex sync.Mutex

	for _, pod := range pods.Items {
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			prefix := fmt.Sprintf("[%s]", p.Name)

			// --- PHASE 1: UPLOAD (TAR STREAMING) ---
			if *uploadSrc != "" {
				// We create a pipe: makeTar writes to 'pw', execCmd reads from 'pr'
				pr, pw := io.Pipe()

				// Start Tar Producer
				go func() {
					defer pw.Close() //nolint:errcheck
					if err := makeTar(*uploadSrc, pw); err != nil {
						// Closing with error ensures the execCmd stream fails fast
						pw.CloseWithError(err)
					}
				}()

				// Run 'tar' on the remote pod to consume the stream
				// mkdir -p ensures dest exists. tar -xmf - -C dest extracts stdin.
				tarCmd := []string{"/bin/sh", "-c", fmt.Sprintf("mkdir -p '%s' && tar -xmf - -C '%s'", *uploadDest, *uploadDest)}

				// Pass 'pr' as Stdin
				err := execCmd(ctx, config, clientset, p, tarCmd, pr, nil, nil)
				if err != nil {
					printWithMutex(&printMutex, prefix, fmt.Sprintf("Upload Failed: %v", err))
					// If upload fails, we probably shouldn't run the command
					return
				}
				printWithMutex(&printMutex, prefix, fmt.Sprintf("Synced %s -> %s", *uploadSrc, *uploadDest))
			}

			// --- PHASE 2: EXECUTE COMMAND ---
			if *commandStr != "" {
				// Parse command (simple split for now, can be improved with shlex like library)
				cmdArray := strings.Fields(*commandStr)

				// Prepare pipes for output
				prOut, pwOut := io.Pipe()
				prErr, pwErr := io.Pipe()

				// Start Log Processors
				go logStream(prOut, &printMutex, prefix, os.Stdout)
				go logStream(prErr, &printMutex, prefix, os.Stderr)

				// Execute
				err := execCmd(ctx, config, clientset, p, cmdArray, nil, pwOut, pwErr)

				_ = pwOut.Close()
				_ = pwErr.Close()

				if err != nil {
					printWithMutex(&printMutex, prefix, fmt.Sprintf("Command Error: %v", err))
				}
			}
		}(pod)
	}

	wg.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		klog.Infoln("\nFinished due to TIMEOUT.")
		os.Exit(1)
	}
}

func execCmd(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pod corev1.Pod, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: command,
		Stdin:   stdin != nil,
		Stdout:  stdout != nil,
		Stderr:  stderr != nil,
		TTY:     false,
	}

	req.VersionedParams(option, scheme.ParameterCodec)

	exec, err := remotecommand.NewWebSocketExecutor(config, "GET", req.URL().String())
	if err != nil {
		return err
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// makeTar walks the source and writes a tarball to the writer
func makeTar(srcPath string, writer io.Writer) error {
	srcPath = filepath.Clean(srcPath)
	absSrcPath, err := filepath.Abs(srcPath)
	if err != nil {
		return err
	}

	// If src is a file, base dir is its parent. If dir, it's the dir itself.
	// This behavior controls whether the folder *itself* appears in the tar or just contents.
	// For "cp -r src/ dest/", usually we want contents.
	baseDir := filepath.Dir(absSrcPath)

	tw := tar.NewWriter(writer)
	defer tw.Close() //nolint:errcheck

	return filepath.Walk(srcPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Create header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		// Rebase the path so it's relative to the upload root
		relPath, err := filepath.Rel(baseDir, file)
		if err != nil {
			return err
		}
		header.Name = relPath

		// Ensure binaries are executable (simple heuristic: if we are uploading, preserve local mode)
		// header.Mode is already populated by FileInfoHeader from local file
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck

		_, err = io.Copy(tw, f)
		return err
	})
}

func logStream(r io.Reader, mu *sync.Mutex, prefix string, out io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		mu.Lock()
		_, _ = fmt.Fprintf(out, "%s %s\n", prefix, text)
		mu.Unlock()
	}
}

func printWithMutex(mu *sync.Mutex, prefix, text string) {
	mu.Lock()
	fmt.Printf("%s %s\n", prefix, text)
	mu.Unlock()
}
