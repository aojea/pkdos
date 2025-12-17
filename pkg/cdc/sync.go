package cdc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/aojea/krun/pkg/exec"
	"github.com/aojea/krun/pkg/files"

	"github.com/restic/chunker"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

const (
	ManifestFile = "manifest.json"
	ChunksDir    = "krun-chunks"
	AgentFile    = "/tmp/krun-agent"
)

type Manifest struct {
	Chunks []ChunkInfo `json:"chunks"`
}

type ChunkInfo struct {
	Hash string `json:"hash"`
	Size uint   `json:"size"`
	Data []byte `json:"-"` // Local optimization only
}

// ExecCmd allows mocking the remote execution in tests
var ExecCmd = exec.ExecCmd

// SyncLocalToLeader uploads changed chunks to the leader using kubectl exec
func SyncLocalToLeader(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pod corev1.Pod, srcPath, remoteDir string, exclude *regexp.Regexp, cleanup bool) error {
	klog.Info("Chunking local files...")

	// Create temp dir for chunks
	tmpDir, err := os.MkdirTemp("", "krun-chunks-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Generate Local Manifest & Chunks
	manifest, err := GenerateManifest(srcPath, exclude, tmpDir)
	if err != nil {
		return err
	}
	klog.Infof("Local data split into %d chunks", len(manifest.Chunks))

	// Check diff with Leader (Exec "check")
	klog.Info("Checking missing chunks on leader...")
	missingHashes, err := checkRemote(ctx, config, client, pod, remoteDir, manifest)
	if err != nil {
		return fmt.Errorf("remote check failed: %w", err)
	}
	klog.Infof("Leader missing %d chunks", len(missingHashes))

	// Upload Missing Chunks + Manifest (Exec "ingest")
	if len(missingHashes) > 0 || true { // Always upload manifest at least
		klog.Info("Uploading data...")
		err := ingestRemote(ctx, config, client, pod, remoteDir, missingHashes, tmpDir, manifest, cleanup)
		if err != nil {
			return fmt.Errorf("remote ingest failed: %w", err)
		}
	}

	return nil
}

func GenerateManifest(src string, exclude *regexp.Regexp, chunksDir string) (Manifest, error) {
	// Create a pipe to feed the Tar stream into the Chunker without allocating memory
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		if err := files.MakeTar(src, pw, exclude); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()

	chk := chunker.New(pr, chunker.Pol(0x3DA3358B4DC173))
	buf := make([]byte, chunker.MaxSize)

	m := Manifest{}

	for {
		chunk, err := chk.Next(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}

		sha := sha256.Sum256(chunk.Data)
		hash := hex.EncodeToString(sha[:])

		// Store data in disk for retrieval
		chunkPath := filepath.Join(chunksDir, hash)
		if err := os.WriteFile(chunkPath, chunk.Data, 0644); err != nil {
			return m, fmt.Errorf("failed to save chunk %s: %w", hash, err)
		}

		m.Chunks = append(m.Chunks, ChunkInfo{
			Hash: hash,
			Size: chunk.Length,
		})
	}
	return m, nil
}

// checkRemote runs `agent -mode check` on the pod
func checkRemote(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pod corev1.Pod, remoteDir string, m Manifest) ([]string, error) {
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal manifest: %w", err)
	}

	cmd := []string{AgentFile, "-mode", "check", "-dir", remoteDir}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	// Standard Exec
	err = ExecCmd(ctx, config, client, pod, cmd, remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(manifestJSON),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("exec error: %v (stderr: %s)", err, stderr.String())
	}

	var missing []string
	if err := json.NewDecoder(&stdout).Decode(&missing); err != nil {
		return nil, fmt.Errorf("bad response: %v", err)
	}
	return missing, nil
}

// ingestRemote runs `agent -mode ingest` and pipes a tarball of chunks
func ingestRemote(ctx context.Context, config *rest.Config, client *kubernetes.Clientset, pod corev1.Pod, remoteDir string, missing []string, chunksDir string, m Manifest, cleanup bool) error {
	// use a pipe to avoid allocating memory
	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = pw.Close() }()
		tw := tar.NewWriter(pw)
		defer func() { _ = tw.Close() }()

		// Add Missing Chunks
		for _, hash := range missing {
			// Read from disk
			data, err := os.ReadFile(filepath.Join(chunksDir, hash))
			if err != nil {
				return
			}

			header := &tar.Header{
				Name: hash, // Flat structure for chunks
				Size: int64(len(data)),
				Mode: 0644,
			}
			if err := tw.WriteHeader(header); err != nil {
				return
			}
			if _, err := tw.Write(data); err != nil {
				return
			}
		}

		// Add Manifest (ALWAYS add this last or ensure it's included so Hub can serve it)
		manifestBytes, err := json.Marshal(m)
		if err != nil {
			return
		}
		header := &tar.Header{
			Name: ManifestFile,
			Size: int64(len(manifestBytes)),
			Mode: 0644,
		}
		if err := tw.WriteHeader(header); err != nil {
			return
		}
		if _, err := tw.Write(manifestBytes); err != nil {
			return
		}
	}()

	cmd := []string{AgentFile, "-mode", "ingest", "-dir", remoteDir}
	if cleanup {
		cmd = append(cmd, "-cleanup")
	}
	return ExecCmd(ctx, config, client, pod, cmd, remotecommand.StreamOptions{
		Stdin:  pr,
		Stdout: io.Discard,
		Stderr: os.Stderr,
	})
}
