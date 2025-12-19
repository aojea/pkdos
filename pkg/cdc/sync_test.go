package cdc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestSyncLocalToLeader(t *testing.T) {
	// Setup Source Dir
	srcDir := t.TempDir()
	err := os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte("hello world"), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Mock ExecCmd
	execCallCount := 0
	originalExecCmd := ExecCmd
	defer func() { ExecCmd = originalExecCmd }()

	ExecCmd = func(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pod corev1.Pod, cmd []string, options remotecommand.StreamOptions) error {
		execCallCount++
		mode := ""
		// Parse mode from cmd
		for i, arg := range cmd {
			if arg == "-mode" && i+1 < len(cmd) {
				mode = cmd[i+1]
			}
		}

		if mode == "check" {
			// Mock Check: return missing chunk
			var m Manifest
			_ = json.NewDecoder(options.Stdin).Decode(&m)

			// Assume all chunks missing
			missing := []string{}
			for _, c := range m.Chunks {
				missing = append(missing, c.Hash)
			}
			_ = json.NewEncoder(options.Stdout).Encode(missing)
			return nil
		}

		if mode == "ingest" {
			// Mock Ingest: consume stdin
			_, _ = io.Copy(io.Discard, options.Stdin)
			return nil
		}

		return nil
	}

	pod := corev1.Pod{}
	pod.Name = "test-pod"

	err = SyncLocalToLeader(context.Background(), nil, nil, pod, srcDir, "/remote/path", nil, false)
	if err != nil {
		t.Fatalf("SyncLocalToLeader failed: %v", err)
	}

	if execCallCount < 2 {
		t.Errorf("Expected at least 2 exec calls (check, ingest), got %d", execCallCount)
	}
}

func TestSyncPods(t *testing.T) {
	// Setup pods
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-0"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
		}, // Leader
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
		}, // Peer 1
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-2"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.3"},
		}, // Peer 2
	}

	// Mock ExecCmd
	originalExecCmd := ExecCmd
	defer func() { ExecCmd = originalExecCmd }()

	var mu sync.Mutex
	execHistory := []string{}

	ExecCmd = func(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pod corev1.Pod, cmd []string, options remotecommand.StreamOptions) error {
		mode := ""
		for i, arg := range cmd {
			if arg == "-mode" && i+1 < len(cmd) {
				mode = cmd[i+1]
			}
		}

		mu.Lock()
		execHistory = append(execHistory, fmt.Sprintf("%s:%s", pod.Name, mode))
		mu.Unlock()

		if mode == "hub" {
			// Simulate Hub Output
			_, _ = fmt.Fprintln(options.Stdout, "Hub listening on :12345")
			// Block until context cancelled to simulate long-running process
			<-ctx.Done()
			return nil
		}

		if mode == "check" || mode == "ingest" {
			// Used by SyncLocalToLeader
			if mode == "ingest" {
				// Verify cleanup flag is NOT present (since multiple pods)
				for _, arg := range cmd {
					if arg == "-cleanup" {
						mu.Lock()
						execHistory = append(execHistory, fmt.Sprintf("%s:ingest:cleanup_found", pod.Name))
						mu.Unlock()
					}
				}
			}
			if mode == "check" {
				_ = json.NewEncoder(options.Stdout).Encode([]string{}) // No missing chunks
			}
			return nil
		}

		if mode == "peer" {
			// Simulate Peer duration
			return nil
		}

		return nil
	}

	// Setup Temp Source Dir
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte("data"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	err := SyncPods(context.Background(), nil, nil, pods, srcDir, "/remote/path", nil)
	if err != nil {
		t.Fatalf("SyncPods failed: %v", err)
	}

	// We check if "pod-0:hub" happened, and "pod-1:peer", "pod-2:peer" happened.
	hasHub := false
	peerCount := 0
	for _, entry := range execHistory {
		if strings.Contains(entry, "pod-0:hub") {
			hasHub = true
		}
		if strings.Contains(entry, "peer") {
			peerCount++
		}
	}

	if !hasHub {
		t.Error("Hub was not started on leader")
	}
	if peerCount != 2 {
		t.Errorf("Expected 2 peers to sync, got %d", peerCount)
	}
}

func TestGenerateManifest(t *testing.T) {
	// Setup temporary source and chunks directories
	srcDir := t.TempDir()
	chunksDir := t.TempDir()

	// 1. Create source files
	// Create "file1.txt"
	if err := os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("content1"), 0644); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}
	// Create "subdir/file2.txt"
	subDir := filepath.Join(srcDir, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "file2.txt"), []byte("content2"), 0644); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	// 2. Run GenerateManifest
	manifest, err := GenerateManifest(srcDir, nil, chunksDir)
	if err != nil {
		t.Fatalf("GenerateManifest failed: %v", err)
	}

	// 3. Verify chunks are created
	if len(manifest.Chunks) == 0 {
		t.Error("Expected chunks in manifest, got 0")
	}

	for _, chunk := range manifest.Chunks {
		chunkPath := filepath.Join(chunksDir, chunk.Hash)
		if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
			t.Errorf("Chunk %s was not written to disk", chunk.Hash)
		}
	}

	// 4. Verify exclusions
	// Create excluded file
	if err := os.WriteFile(filepath.Join(srcDir, "ignore.me"), []byte("ignore"), 0644); err != nil {
		t.Fatalf("Failed to write ignore.me: %v", err)
	}
	// Run again with exclusion
	chunksDir2 := t.TempDir()
	exclude := regexp.MustCompile(`ignore\.me`)
	manifest2, err := GenerateManifest(srcDir, exclude, chunksDir2)
	if err != nil {
		t.Fatalf("GenerateManifest with exclusion failed: %v", err)
	}

	if len(manifest.Chunks) != len(manifest2.Chunks) {
		t.Errorf("Expected same number of chunks with exclusion (got %d vs %d)", len(manifest2.Chunks), len(manifest.Chunks))
	}
}
