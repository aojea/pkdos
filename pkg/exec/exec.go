package exec

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

func UploadAndExecuteOnPods(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pods []corev1.Pod, uploadSrc, uploadDest string, excludeRegex *regexp.Regexp, commandArgs []string) error {

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

			// --- PHASE 1: UPLOAD (TAR STREAMING) ---
			if uploadSrc != "" {
				// We create a pipe: makeTar writes to 'pw', execCmd reads from 'pr'
				pr, pw := io.Pipe()

				// Start Tar Producer
				go func() {
					defer pw.Close() //nolint:errcheck
					if err := makeTar(uploadSrc, pw, excludeRegex); err != nil {
						// Closing with error ensures the execCmd stream fails fast
						pw.CloseWithError(err)
						klog.Errorf("Tar Error: %s %s\n", prefix, err)
						cancel()
					}
				}()

				// 1. Create destination directory
				mkdirCmd := []string{"mkdir", "-p", uploadDest}
				// We must provide at least one stream (stdin, stdout, stderr) for k8s exec.
				err := execCmd(ctx, config, clientset, p, mkdirCmd, nil, io.Discard, nil)
				if err != nil {
					logCh <- logEntry{prefix: prefix, text: fmt.Sprintf("Mkdir Error: %v", err), out: os.Stderr}
					// If mkdir fails, we stop
					return
				}

				// 2. Run 'tar' to consume the stream
				tarCmd := []string{"tar", "-xmf", "-", "-C", uploadDest}

				// Pass 'pr' as Stdin
				err = execCmd(ctx, config, clientset, p, tarCmd, pr, nil, nil)
				if err != nil {
					logCh <- logEntry{prefix: prefix, text: fmt.Sprintf("Transfer Error: %v", err), out: os.Stderr}
					// If upload fails, we probably shouldn't run the command
					return
				}
				logCh <- logEntry{prefix: prefix, text: fmt.Sprintf("Synced %s -> %s", uploadSrc, uploadDest), out: os.Stderr}
			}

			if len(commandArgs) > 0 {
				// Prepare pipes for output
				prOut, pwOut := io.Pipe()
				prErr, pwErr := io.Pipe()

				// Start Log Processors
				go logStream(ctx, prOut, logCh, prefix, os.Stdout)
				go logStream(ctx, prErr, logCh, prefix, os.Stderr)

				// Execute
				err := execCmd(ctx, config, clientset, p, commandArgs, nil, pwOut, pwErr)

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

// makeTar walks the source and writes a tarball to the writer
func makeTar(srcPath string, writer io.Writer, excludeRegex *regexp.Regexp) error {
	absSrcPath, err := filepath.Abs(filepath.Clean(srcPath))
	if err != nil {
		return err
	}

	// Check if the source is a directory
	info, err := os.Stat(absSrcPath)
	if err != nil {
		return err
	}

	// If it's a directory, we use the directory itself as the base.
	// This means files inside will have paths relative to the directory,
	// effectively stripping the directory name from the tar archive.
	baseDir := absSrcPath
	if !info.IsDir() {
		// If it's a file, we use its parent as the base, preserving the filename.
		baseDir = filepath.Dir(absSrcPath)
	}

	tw := tar.NewWriter(writer)
	defer tw.Close() //nolint:errcheck

	return filepath.Walk(absSrcPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Rebase the path so it's relative to the upload root
		relPath, err := filepath.Rel(baseDir, file)
		if err != nil {
			return err
		}

		// If we are uploading a directory, the walk starts with the directory itself.
		// Its relative path is ".". We skip adding a tar entry for "." to avoid
		// messing with the destination root permissions or creating a "./" folder.
		if relPath == "." {
			return nil
		}

		if excludeRegex != nil && excludeRegex.MatchString(relPath) {
			// If it matches and is a directory, skip the whole tree
			if fi.IsDir() {
				return filepath.SkipDir
			}
			// If it's a file, just skip adding it
			return nil
		}

		// Create header
		header, err := tar.FileInfoHeader(fi, fi.Name())
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
