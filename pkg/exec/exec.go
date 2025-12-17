package exec

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

func ExecuteOnPods(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pods []corev1.Pod, commandArgs []string) error {
	klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// do not block on logging
	logCh := make(chan logEntry, 1000)
	loggerDone := make(chan struct{})
	go logger(logCh, loggerDone)

	// each pod is processed in a separate goroutine
	var wg sync.WaitGroup
	for i, pod := range pods {
		if ctx.Err() != nil {
			klog.Infof("Context done, cancelling remaining %d operations... %v", len(pods)-i, ctx.Err())
			break
		}
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			prefix := fmt.Sprintf("[%s]", p.Name)

			if len(commandArgs) > 0 {
				// Prepare pipes for output
				prOut, pwOut := io.Pipe()
				prErr, pwErr := io.Pipe()

				// Start Log Processors
				go logStream(ctx, prOut, logCh, prefix, os.Stdout)
				go logStream(ctx, prErr, logCh, prefix, os.Stderr)

				// Execute
				err := ExecCmd(ctx, config, clientset, p, commandArgs, remotecommand.StreamOptions{Stdout: pwOut, Stderr: pwErr})

				_ = pwOut.Close()
				_ = pwErr.Close()

				if err != nil {
					logCh <- logEntry{prefix: prefix, text: fmt.Sprintf("Command Error: %v", err), out: os.Stderr}
				}
			}
		}(pod)
	}

	wg.Wait()
	close(logCh)
	// wait for logger to finish
	<-loggerDone

	if ctx.Err() != nil {
		klog.Infof("Context done, cancelling remaining operations... %v", ctx.Err())
		return ctx.Err()
	}
	return nil
}

func ExecCmd(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pod corev1.Pod, command []string, options remotecommand.StreamOptions) error {
	klog.V(4).Infof("Executing command %v on pod %s/%s", command, pod.Namespace, pod.Name)
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: command,
		Stdin:   options.Stdin != nil,
		Stdout:  options.Stdout != nil,
		Stderr:  options.Stderr != nil,
		TTY:     options.Tty,
	}

	req.VersionedParams(option, scheme.ParameterCodec)

	exec, err := remotecommand.NewWebSocketExecutor(config, "GET", req.URL().String())
	if err != nil {
		return err
	}

	return exec.StreamWithContext(ctx, options)
}

func UploadExecutableOnPods(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pods []corev1.Pod, filePath string, filedata []byte) error {
	var mu sync.Mutex
	var allErrors []error
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd := []string{"sh", "-c", fmt.Sprintf("cat > %s && chmod +x %s", filePath, filePath)}
			err := ExecCmd(ctx, config, clientset, p, cmd, remotecommand.StreamOptions{
				Stdin:  bytes.NewReader(filedata),
				Stdout: &stdout,
				Stderr: &stderr,
			})
			if err != nil {
				mu.Lock()
				allErrors = append(allErrors, fmt.Errorf("failed to upload executable to pod %s stdout: %s stderr: %s: %w", p.Name, stdout.String(), stderr.String(), err))
				mu.Unlock()
			}
		}(pod)
	}
	wg.Wait()

	return errors.Join(allErrors...)
}

// RemovePathsFromPods removes a list of paths from a list of pods using rm -rf
func RemovePathsFromPods(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pods []corev1.Pod, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	var mu sync.Mutex
	var allErrors []error
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			// rm -rf path1 path2 ...
			cmd := append([]string{"rm", "-rf"}, paths...)
			err := ExecCmd(ctx, config, clientset, p, cmd, remotecommand.StreamOptions{
				Stdout: &stdout,
				Stderr: &stderr,
			})
			if err != nil {
				mu.Lock()
				allErrors = append(allErrors, fmt.Errorf("failed to remove paths from pod %s stdout: %s stderr: %s: %w", p.Name, stdout.String(), stderr.String(), err))
				mu.Unlock()
			}
		}(pod)
	}
	wg.Wait()
	return errors.Join(allErrors...)
}

func logStream(ctx context.Context, r io.Reader, ch chan<- logEntry, prefix string, out io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		select {
		case ch <- logEntry{prefix: prefix, text: scanner.Text(), out: out}:
		case <-ctx.Done():
			return
		}
	}
}

type logEntry struct {
	prefix string
	text   string
	out    io.Writer
}

func logger(ch <-chan logEntry, done chan<- struct{}) {
	for entry := range ch {
		_, _ = fmt.Fprintf(entry.out, "%s %s\n", entry.prefix, entry.text)
	}
	done <- struct{}{}
}
