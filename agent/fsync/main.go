package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

// sync with pkg/cdc/sync.go
const (
	ManifestFile = "manifest.json"
	ChunksDir    = "krun-chunks"
)

func main() {
	klog.InitFlags(nil)
	var (
		mode        = flag.String("mode", "peer", "Mode: hub | peer | check | ingest")
		dataDir     = flag.String("dir", "/app", "Data directory")
		trackerURL  = flag.String("tracker", "", "Tracker URL (for peers)")
		trackerPort = flag.Int("tracker-port", 8000, "Tracker port (for hub)")
		cleanup     = flag.Bool("cleanup", false, "Cleanup artifacts after sync")
		mirror      = flag.Bool("mirror", true, "Mirror destination (delete extraneous files)")
	)
	flag.Parse()
	defer klog.Flush()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		klog.Exitf("Failed to create data dir %s: %v", *dataDir, err)
	}

	chunksPath := filepath.Join(*dataDir, ChunksDir)
	if err := os.MkdirAll(chunksPath, 0755); err != nil {
		klog.Exitf("Failed to create chunks dir: %v", err)
	}

	switch *mode {
	case "hub":
		runHub(ctx, *dataDir, *trackerPort)
	case "peer":
		if *trackerURL == "" {
			klog.Exit("Tracker URL is required for peer mode")
		}
		if err := runPeer(ctx, *dataDir, *trackerURL, *cleanup, *mirror); err != nil {
			klog.Exit(err)
		}
	case "check":
		// Step 1 of Sync: Read Manifest from Stdin, Print missing hashes to Stdout
		if err := runCheck(os.Stdin, os.Stdout, chunksPath); err != nil {
			klog.Exit(err)
		}
	case "ingest":
		// Step 2 of Sync: Read Tar from Stdin, Save to disk, Update Manifest
		if err := runIngest(os.Stdin, *dataDir, chunksPath, *cleanup, *mirror); err != nil {
			klog.Exit(err)
		}
	default:
		klog.Exitf("Unknown mode: %s", *mode)
	}
}

// Manifest represents the ordered list of chunks
type Manifest struct {
	Chunks []ChunkInfo `json:"chunks"`
}

type ChunkInfo struct {
	Hash string `json:"hash"`
	Size uint   `json:"size"`
}

// runHub serves the files to Peers (Read-Only)
func runHub(ctx context.Context, dir string, port int) {
	ctx, cancel := context.WithCancel(ctx)
	mux := newHubHandler(dir)

	// Cleanup on exit
	defer func() {
		klog.Info("Hub cleaning up artifacts...")
		_ = os.RemoveAll(filepath.Join(dir, ChunksDir))
		_ = os.Remove(filepath.Join(dir, ManifestFile))
	}()

	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		klog.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	// Print the actual address we are listening on (important if port was 0)
	// We print to Stdout so the caller (SyncPods) can parse it.
	fmt.Printf("Hub listening on %s\n", listener.Addr().String())
	// Ensure stdout is flushed
	_ = os.Stdout.Sync()
	server := &http.Server{Handler: mux}

	// Monitor Stdin for EOF to exit
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Stdin closed, initiate shutdown
		klog.Info("Stdin closed, shutting down hub...")
		_ = server.Shutdown(context.Background())
		cancel()
	}()

	go func() {
		klog.Infof("Hub serving on %s", listener.Addr())
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("HTTP server failed: %v", err)
		}
	}()

	<-ctx.Done()
	_ = server.Shutdown(context.Background())
}

func newHubHandler(dir string) http.Handler {
	mux := http.NewServeMux()
	chunksPath := filepath.Join(dir, ChunksDir)
	manifestPath := filepath.Join(dir, ManifestFile)

	// Serve Manifest from Disk
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.ServeFile(w, r, manifestPath)
	})

	// Serve Chunks from Disk
	mux.Handle("/chunks/", http.StripPrefix("/chunks/", http.FileServer(http.Dir(chunksPath))))
	return mux
}

// runCheck reads a JSON manifest from Stdin and writes missing chunks to Stdout
func runCheck(r io.Reader, w io.Writer, chunksDir string) error {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return fmt.Errorf("failed to decode manifest from stdin: %v", err)
	}

	var missing []string
	for _, chunk := range m.Chunks {
		p := filepath.Join(chunksDir, chunk.Hash)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			missing = append(missing, chunk.Hash)
		}
	}

	if err := json.NewEncoder(w).Encode(missing); err != nil {
		return fmt.Errorf("failed to write missing chunks to stdout: %v", err)
	}
	return nil
}

// runIngest reads a TAR stream from Stdin containing chunks and optionally the manifest
func runIngest(r io.Reader, dataDir, chunksDir string, cleanup, mirror bool) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %v", err)
		}

		// Security: prevent path traversal
		if filepath.Clean(header.Name) != header.Name || header.Name == ".." || header.Name[0] == '/' {
			klog.Warningf("Skipping suspicious file: %s", header.Name)
			continue
		}

		// Identify destination
		var target string
		if header.Name == ManifestFile {
			target = filepath.Join(dataDir, ManifestFile)
		} else {
			// Assume it's a chunk
			target = filepath.Join(chunksDir, filepath.Base(header.Name))
		}

		f, err := os.Create(target)
		if err != nil {
			return fmt.Errorf("failed to create file %s: %v", target, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return fmt.Errorf("failed to write file %s: %v", target, err)
		}
		_ = f.Close()
	}

	// Always Apply Manifest (reconstruct files)
	klog.Info("Ingest: applying manifest...")
	manifestPath := filepath.Join(dataDir, ManifestFile)
	f, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("failed to open manifest for apply: %v", err)
	}
	var m Manifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		_ = f.Close()
		return fmt.Errorf("failed to decode manifest for apply: %v", err)
	}
	_ = f.Close()

	created, err := applyManifest(chunksDir, dataDir, &m)
	if err != nil {
		return fmt.Errorf("failed to apply manifest: %v", err)
	}

	// cleanup extraneous files (miroring)
	if mirror {
		if err := cleanupExtraneousFiles(dataDir, created); err != nil {
			klog.Warningf("Failed to cleanup extraneous files: %v", err)
			// Don't fail the sync just because cleanup failed
		}
	}

	if cleanup {
		klog.Info("Cleaning up artifacts...")
		_ = os.RemoveAll(chunksDir)
		_ = os.Remove(filepath.Join(dataDir, ManifestFile))
	}

	klog.Info("Ingest completed successfully")
	return nil
}

// runPeer logic remains largely the same, relying on polling /manifest
func runPeer(ctx context.Context, dir, trackerURL string, cleanup, mirror bool) error {
	chunksDir := filepath.Join(dir, ChunksDir)
	var manifest Manifest

	// Poll for Manifest
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	klog.Infof("Peer waiting for manifest from %s...", trackerURL)
Loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := http.Get(trackerURL + "/manifest")
			if err == nil && resp.StatusCode == http.StatusOK {
				if err := json.NewDecoder(resp.Body).Decode(&manifest); err == nil {
					_ = resp.Body.Close()
					break Loop
				}
				_ = resp.Body.Close()
			}
		}
	}

	klog.Infof("Manifest received with %d chunks. Syncing...", len(manifest.Chunks))

	// Download missing chunks
	concurrency := 5
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	for _, chunk := range manifest.Chunks {
		// Check for previous errors
		select {
		case err := <-errCh:
			return err
		default:
		}

		chunkPath := filepath.Join(chunksDir, chunk.Hash)
		if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
			wg.Add(1)
			sem <- struct{}{}
			go func(c ChunkInfo) {
				defer wg.Done()
				defer func() { <-sem }()

				if err := downloadChunk(trackerURL, c.Hash, chunkPath); err != nil {
					// Try to report the first error
					select {
					case errCh <- fmt.Errorf("failed to download chunk %s: %v", c.Hash, err):
					default:
					}
				}
			}(chunk)
		}
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return err
	}

	created, err := applyManifest(chunksDir, dir, &manifest)
	if err != nil {
		return fmt.Errorf("failed to apply manifest: %v", err)
	}

	// cleanup extraneous files (miroring)
	if mirror {
		if err := cleanupExtraneousFiles(dir, created); err != nil {
			klog.Warningf("Failed to cleanup extraneous files: %v", err)
		}
	}

	// Always cleanup on peer check/sync success
	if cleanup {
		klog.Info("Peer cleaning up artifacts...")
		_ = os.RemoveAll(chunksDir)
		_ = os.Remove(filepath.Join(dir, ManifestFile))
	}

	klog.Info("Peer sync finished successfully.")
	return nil
}

func downloadChunk(baseURL, hash, dest string) error {
	resp, err := http.Get(baseURL + "/chunks/" + hash)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	// Write to temporary file first
	tmpDest := dest + ".tmp"
	out, err := os.Create(tmpDest)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}

	// TeeReader to verify hash while writing
	hasher := sha256.New()
	reader := io.TeeReader(resp.Body, hasher)

	if _, err = io.Copy(out, reader); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpDest)
		return fmt.Errorf("failed to write chunk: %v", err)
	}
	_ = out.Close()

	// Verify Hash
	calculatedHash := hex.EncodeToString(hasher.Sum(nil))
	if calculatedHash != hash {
		_ = os.Remove(tmpDest)
		return fmt.Errorf("integrity check failed: expected %s, got %s", hash, calculatedHash)
	}

	// Rename to final destination
	if err := os.Rename(tmpDest, dest); err != nil {
		_ = os.Remove(tmpDest)
		return fmt.Errorf("failed to rename chunk: %v", err)
	}
	return nil
}

func applyManifest(chunksDir, targetDir string, m *Manifest) ([]string, error) {
	// Reconstruct stream and pipe to tar extraction
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		for _, chunk := range m.Chunks {
			data, err := os.ReadFile(filepath.Join(chunksDir, chunk.Hash))
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(data); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	var created []string
	tr := tar.NewReader(pr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		target := filepath.Join(targetDir, header.Name)
		created = append(created, target)

		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0755); err != nil {
				return nil, err
			}
			continue
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return nil, err
		}
		_ = f.Close()
	}
	return created, nil
}

func cleanupExtraneousFiles(targetDir string, keep []string) error {
	keepMap := make(map[string]bool)
	for _, p := range keep {
		keepMap[p] = true
	}
	// Always keep internal structures
	keepMap[filepath.Join(targetDir, ChunksDir)] = true
	keepMap[filepath.Join(targetDir, ManifestFile)] = true

	// Also keep parent directories of kept files
	for _, p := range keep {
		dir := filepath.Dir(p)
		for dir != targetDir && dir != "." && dir != "/" {
			keepMap[dir] = true
			dir = filepath.Dir(dir)
		}
	}

	// Post-order walk to delete children before parents
	// filepath.Walk is lexical, not post-order.
	// We can use Walk, but simple Remove on directory fails if not empty.
	// If we want to delete untracked directories, we should probably check if they are in keepMap.
	// If a directory is NOT in keepMap, it means NO file inside it is kept. So we can RemoveAll it.
	// However, we must be careful not to RemoveAll a directory that IS in keepMap (which implies some child is kept).

	return filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip root
		if path == targetDir {
			return nil
		}
		// If ChunksDir (directory), skip walking inside it, we manage it separately
		// Note: ChunksDir is in keepMap, so it would be skipped by keepMap check too,
		// but we want to SkipDir to avoid walking 1000s of chunks.
		if info.IsDir() && info.Name() == ChunksDir {
			return filepath.SkipDir
		}

		// Check if we should keep it
		if keepMap[path] {
			return nil
		}

		// If it's a directory and NOT in keepMap, it implies no children are kept (because we added parents of all kept files).
		// So we can safely RemoveAll it.
		if info.IsDir() {
			klog.Infof("Removing extraneous directory: %s", path)
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			return filepath.SkipDir // No need to walk deleted dir
		}

		klog.Infof("Removing extraneous file: %s", path)
		return os.Remove(path)
	})
}
