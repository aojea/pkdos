package cdc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// SyncPods synchronizes files to a set of pods using a Leader-Follower (Hub-Peer) approach.
// 1. Syncs local files to the first pod (Leader).
// 2. Starts a Hub on the Leader.
// 3. Peers download from the Hub.
func SyncPods(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pods []corev1.Pod, srcPath, remoteDir string, exclude *regexp.Regexp) error {
	if len(pods) == 0 {
		return fmt.Errorf("no pods to sync")
	}

	leader := pods[0]
	klog.Infof("Selected leader pod: %s", leader.Name)

	// If there is only one pod, we can cleanup the artifacts immediately after ingest
	// If there are multiple pods, we need to keep the artifacts for the peers to download,
	// and the Hub will cleanup on exit.
	cleanupLeader := len(pods) == 1

	klog.Info("Syncing to leader...")
	if err := SyncLocalToLeader(ctx, config, client, leader, srcPath, remoteDir, exclude, cleanupLeader); err != nil {
		return fmt.Errorf("failed to sync to leader: %w", err)
	}

	if len(pods) == 1 {
		return nil
	}

	// Start Hub on Leader
	klog.Info("Starting hub on leader...")

	// pipe to capture hub output
	pr, pw := io.Pipe()

	// Keep-alive pipe for Hub Stdin
	stdinReader, stdinWriter := io.Pipe()

	// We need a way to start the hub and keep it running until peers are done.
	// We can use a context that we cancel after peers finish.
	hubCtx, cancelHub := context.WithCancel(ctx)
	defer cancelHub()

	go func() {
		defer func() {
			if err := pw.Close(); err != nil {
				klog.Errorf("error closing pipe writer: %v", err)
			}
		}()
		// Use port 0 to let OS assign a free port
		cmd := []string{AgentFile, "-mode", "hub", "-dir", remoteDir, "-tracker-port", "0"}
		// We expect this to block until context is cancelled OR stdin is closed
		_ = ExecCmd(hubCtx, config, client, leader, cmd, remotecommand.StreamOptions{
			Stdin:  stdinReader,
			Stdout: pw,
			Stderr: os.Stderr,
		})
	}()

	// Close stdinWriter when we are done to signal Hub to exit
	defer func() {
		_ = stdinWriter.Close()
	}()

	// Read Hub Output to find the port
	// "Hub listening on :38573"
	scanner := bufio.NewScanner(pr)
	var hubPort string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Hub listening on") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				hubPort = parts[len(parts)-1]
				klog.Infof("Hub started on port %s", hubPort)
				break
			}
		}
	}
	// Consume remaining output in background to avoid blocking
	go func() {
		_, _ = io.Copy(io.Discard, pr)
	}()

	if hubPort == "" {
		return fmt.Errorf("failed to get hub port")
	}

	// Get Leader IP
	leaderIP := leader.Status.PodIP
	if leaderIP == "" {
		return fmt.Errorf("leader pod %s has no IP", leader.Name)
	}
	hubURL := fmt.Sprintf("http://%s", net.JoinHostPort(leaderIP, hubPort))

	// Run Peers
	peers := pods[1:]
	klog.Infof("Starting sync on %d peers...", len(peers))
	var wg sync.WaitGroup
	errCh := make(chan error, len(peers))

	for _, peer := range peers {
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			cmd := []string{AgentFile, "-mode", "peer", "-dir", remoteDir, "-tracker", hubURL, "-cleanup"}
			// This Exec should block until peer is done
			if err := ExecCmd(ctx, config, client, p, cmd, remotecommand.StreamOptions{
				Stdout: os.Stdout,
				Stderr: os.Stderr,
			}); err != nil {
				errCh <- fmt.Errorf("peer %s failed: %w", p.Name, err)
			}
		}(peer)
	}

	wg.Wait()
	close(errCh)

	if len(errCh) > 0 {
		return <-errCh // Return first error
	}

	klog.Info("SyncPods completed successfully")
	return nil
}
