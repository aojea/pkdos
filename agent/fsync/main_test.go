package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aojea/krun/pkg/cdc"
)

func TestRunCheck(t *testing.T) {
	// Setup temporary chunks directory
	chunksDir := t.TempDir()

	// Create a dummy chunk
	chunkData := []byte("hello world")
	sum := sha256.Sum256(chunkData)
	chunkHash := hex.EncodeToString(sum[:])
	err := os.WriteFile(filepath.Join(chunksDir, chunkHash), chunkData, 0644)
	if err != nil {
		t.Fatalf("Failed to write chunk file: %v", err)
	}

	// Define manifest with one existing and one missing chunk
	manifest := Manifest{
		Chunks: []ChunkInfo{
			{Hash: chunkHash, Size: uint(len(chunkData))},
			{Hash: "missingChunk", Size: 100},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Failed to marshal manifest: %v", err)
	}

	// Run check
	var out bytes.Buffer
	err = runCheck(bytes.NewReader(manifestBytes), &out, chunksDir)
	if err != nil {
		t.Fatalf("runCheck failed: %v", err)
	}

	// Verify output
	var missing []string
	err = json.Unmarshal(out.Bytes(), &missing)
	if err != nil {
		t.Fatalf("Failed to unmarshal output: %v", err)
	}
	expected := []string{"missingChunk"}
	if !reflect.DeepEqual(missing, expected) {
		t.Errorf("Expected missing chunks %v, got %v", expected, missing)
	}
}

func TestRunIngest(t *testing.T) {
	dataDir := t.TempDir()
	chunksDir := filepath.Join(dataDir, ChunksDir)
	err := os.MkdirAll(chunksDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create chunks dir: %v", err)
	}

	// Create tar data
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Add Manifest
	manifestData := []byte(`{"chunks":[]}`)
	hdr := &tar.Header{
		Name: ManifestFile,
		Mode: 0644,
		Size: int64(len(manifestData)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("Failed to write manifest header: %v", err)
	}
	if _, err := tw.Write(manifestData); err != nil {
		t.Fatalf("Failed to write manifest data: %v", err)
	}

	// Add a Chunk
	chunkName := "chunk123"
	chunkData := []byte("some data")
	hdr = &tar.Header{
		Name: chunkName,
		Mode: 0644,
		Size: int64(len(chunkData)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("Failed to write chunk header: %v", err)
	}
	if _, err := tw.Write(chunkData); err != nil {
		t.Fatalf("Failed to write chunk data: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("Failed to close tar writer: %v", err)
	}

	// Run Ingest
	err = runIngest(&buf, dataDir, chunksDir, false, false)
	if err != nil {
		t.Fatalf("runIngest failed: %v", err)
	}

	// Verify files created
	if _, err := os.Stat(filepath.Join(dataDir, ManifestFile)); os.IsNotExist(err) {
		t.Errorf("Manifest file was not created")
	}
	if _, err := os.Stat(filepath.Join(chunksDir, chunkName)); os.IsNotExist(err) {
		t.Errorf("Chunk file was not created")
	}
}

// TestRunHubAndPeerIntegration benchmarks the Hub and Peer interaction
// This attempts to start a real Hub and Peer on localhost and sync a file
func TestRunHubAndPeerIntegration(t *testing.T) {
	// Setup directories
	hubDir := t.TempDir()
	peerDir := t.TempDir()
	hubChunksDir := filepath.Join(hubDir, ChunksDir)
	peerChunksDir := filepath.Join(peerDir, ChunksDir)

	if err := os.MkdirAll(hubChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create hub chunks dir: %v", err)
	}
	if err := os.MkdirAll(peerChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create peer chunks dir: %v", err)
	}

	// Create content on Hub: A valid TAR file containing one file "test.txt"
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	fileContent := []byte("hello sync")
	hdr := &tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len(fileContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("Failed to write header: %v", err)
	}
	if _, err := tw.Write(fileContent); err != nil {
		t.Fatalf("Failed to write content: %v", err)
	}
	_ = tw.Close()

	chunkData := buf.Bytes()
	sum := sha256.Sum256(chunkData)
	chunkHash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(hubChunksDir, chunkHash), chunkData, 0644); err != nil {
		t.Fatalf("Failed to write chunk to hub: %v", err)
	}

	manifest := Manifest{Chunks: []ChunkInfo{{Hash: chunkHash, Size: uint(len(chunkData))}}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Failed to marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, ManifestFile), manifestBytes, 0644); err != nil {
		t.Fatalf("Failed to write manifest to hub: %v", err)
	}

	// Use httptest Server for Hub
	ts := httptest.NewServer(newHubHandler(hubDir))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Peer
	// Peer runs until it syncs or context cancelled.
	if err := runPeer(ctx, peerDir, ts.URL, true, false); err != nil {
		t.Fatalf("runPeer failed: %v", err)
	}

	// Verify Peer has the extracted file
	extractedPath := filepath.Join(peerDir, "test.txt")
	if _, err := os.Stat(extractedPath); os.IsNotExist(err) {
		t.Fatalf("Peer did not extract the file")
	}
	content, err := os.ReadFile(extractedPath)
	if err != nil {
		t.Fatalf("Failed to read extracted file: %v", err)
	}
	if !bytes.Equal(content, fileContent) {
		t.Errorf("Extracted content mismatch. Got %s, want %s", content, fileContent)
	}
}

func TestExhaustiveSync(t *testing.T) {
	// Setup directories
	hubDir := t.TempDir()
	peerDir := t.TempDir()
	sourceDir := t.TempDir() // For generating source files
	hubChunksDir := filepath.Join(hubDir, ChunksDir)
	peerChunksDir := filepath.Join(peerDir, ChunksDir)

	if err := os.MkdirAll(hubChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create hub chunks dir: %v", err)
	}
	if err := os.MkdirAll(peerChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create peer chunks dir: %v", err)
	}

	// Generate 100 files in sourceDir
	numFiles := 100
	for i := 0; i < numFiles; i++ {
		name := filepath.Join(sourceDir, fmt.Sprintf("file-%d.txt", i))
		// Make content large enough to trigger multiple chunks (e.g. 50KB per file -> 5MB total)
		// 5MB should definitely be > 1 chunk (default average is usually 512KB-1MB)
		base := fmt.Sprintf("content-%d-", i)
		content := bytes.Repeat([]byte(base), 5000)
		if err := os.WriteFile(name, content, 0644); err != nil {
			t.Fatalf("Failed to write source file: %v", err)
		}
	}

	// Helper to generate chunks using CDC
	generateAndWrite := func(src string) Manifest {
		m, err := cdc.GenerateManifest(src, nil, hubChunksDir)
		if err != nil {
			t.Fatalf("GenerateManifest failed: %v", err)
		}

		// Convert cdc.Manifest to local Manifest (types are identical in JSON structure)
		manifestBytes, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Failed to marshal manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(hubDir, ManifestFile), manifestBytes, 0644); err != nil {
			t.Fatalf("Failed to write manifest: %v", err)
		}

		// We need to return the manifest as our local type for assertions if we want.
		// Or just decode it back.
		var localManifest Manifest
		if err := json.Unmarshal(manifestBytes, &localManifest); err != nil {
			t.Fatalf("Failed to unmarshal local manifest: %v", err)
		}
		return localManifest
	}

	// Phase 1: Initial Sync
	manifest := generateAndWrite(sourceDir)

	requestCounts := make(map[string]int)
	var mu sync.Mutex
	h := newHubHandler(hubDir)
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCounts[r.URL.Path]++
		mu.Unlock()
		h.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(wrapper)
	defer ts.Close()

	ctx := context.Background()

	start := time.Now()
	if err := runPeer(ctx, peerDir, ts.URL, false, false); err != nil {
		t.Fatalf("Initial sync failed: %v", err)
	}
	t.Logf("Initial sync of %d files took %v", numFiles, time.Since(start))

	// Verify all chunks downloaded
	for _, chunk := range manifest.Chunks {
		if _, err := os.Stat(filepath.Join(peerChunksDir, chunk.Hash)); os.IsNotExist(err) {
			t.Errorf("Chunk %s missing after initial sync", chunk.Hash)
		}
	}

	// Reset counters
	mu.Lock()
	for k := range requestCounts {
		delete(requestCounts, k)
	}
	mu.Unlock()

	// Phase 2: Modify ONE file (last one)
	lastFile := filepath.Join(sourceDir, fmt.Sprintf("file-%d.txt", numFiles-1))
	if err := os.WriteFile(lastFile, []byte("modified-content-at-the-end-plus-extra"), 0644); err != nil {
		t.Fatalf("Failed to modify file: %v", err)
	}

	// Regenerate manifest on Hub
	manifest2 := generateAndWrite(sourceDir)

	// Sync again
	start = time.Now()
	if err := runPeer(ctx, peerDir, ts.URL, false, false); err != nil {
		t.Fatalf("Incremental sync failed: %v", err)
	}
	t.Logf("Incremental sync took %v", time.Since(start))

	// Verify Logic
	downloadedChunks := 0
	for _, chunk := range manifest2.Chunks {
		mu.Lock()
		count := requestCounts["/chunks/"+chunk.Hash]
		mu.Unlock()
		if count > 0 {
			downloadedChunks++
		}
	}

	t.Logf("Phase 2 downloaded %d chunks out of %d total", downloadedChunks, len(manifest2.Chunks))

	if downloadedChunks == 0 {
		t.Error("Expected some chunks to be downloaded in Phase 2")
	}
	if downloadedChunks == len(manifest2.Chunks) {
		// Note: CDC might change boundaries such that more chunks change, but for a single file modification
		// in a set of 100 small files, it should DEFINITELY NOT reuse 0 chunks if the other 99 are identical
		// and chunk boundaries align.
		// However, tar order matters. file-99 is likely last in tar.
		// If tar order is deterministic (lexical), then modifying the last file should only affect the end of the tar stream.
		t.Error("Expected NOT all chunks to be downloaded in Phase 2 (incremental sync failed)")
	}

	// Verify modified file is updated
	peerLastFile := filepath.Join(peerDir, fmt.Sprintf("file-%d.txt", numFiles-1))
	content, err := os.ReadFile(peerLastFile)
	if err != nil {
		t.Fatalf("Failed to read modified file from peer: %v", err)
	}
	if !bytes.Contains(content, []byte("modified-content")) {
		t.Errorf("Modified file content not updated. Got: %s", content)
	}
}

func TestMirroring(t *testing.T) {
	// Setup Source and Dest
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	dstChunksDir := filepath.Join(dstDir, ChunksDir)
	if err := os.MkdirAll(dstChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create dst chunks dir: %v", err)
	}

	// 1. Create file and DIRECTORY in Dst that DOES NOT exist in Src
	// This represents a file from a previous sync or a stray file
	extraFile := filepath.Join(dstDir, "extra.txt")
	if err := os.WriteFile(extraFile, []byte("should be deleted"), 0644); err != nil {
		t.Fatalf("Failed to create extra file: %v", err)
	}
	extraDir := filepath.Join(dstDir, "extraDir")
	if err := os.MkdirAll(extraDir, 0755); err != nil {
		t.Fatalf("Failed to create extra dir: %v", err)
	}
	extraFileInDir := filepath.Join(extraDir, "file.txt")
	if err := os.WriteFile(extraFileInDir, []byte("should be deleted"), 0644); err != nil {
		t.Fatalf("Failed to create extra file in dir: %v", err)
	}

	// 2. Create file in Source
	if err := os.WriteFile(filepath.Join(srcDir, "keep.txt"), []byte("keep me"), 0644); err != nil {
		t.Fatalf("Failed to create keep file: %v", err)
	}

	// 3. Generate Manifest from Source
	// We need a temp dir for chunks on the "hub" side (simulated)
	hubChunksDir := t.TempDir()
	cdcManifest, err := cdc.GenerateManifest(srcDir, nil, hubChunksDir)
	if err != nil {
		t.Fatalf("GenerateManifest failed: %v", err)
	}

	// Convert cdc.Manifest to local Manifest
	manifestBytes, _ := json.Marshal(cdcManifest)
	var manifest Manifest
	_ = json.Unmarshal(manifestBytes, &manifest)

	// 4. Copy Chunks to Dst (Simulate download)
	for _, chunk := range cdcManifest.Chunks {
		srcChunk := filepath.Join(hubChunksDir, chunk.Hash)
		dstChunk := filepath.Join(dstChunksDir, chunk.Hash)
		data, err := os.ReadFile(srcChunk)
		if err != nil {
			t.Fatalf("Failed to read chunk: %v", err)
		}
		if err := os.WriteFile(dstChunk, data, 0644); err != nil {
			t.Fatalf("Failed to write chunk: %v", err)
		}
	}

	// Apply Manifest (Reconstruct)
	created, err := applyManifest(dstChunksDir, dstDir, &manifest)
	if err != nil {
		t.Fatalf("applyManifest failed: %v", err)
	}

	// cleanup extraneous files (mirroring)
	if err := cleanupExtraneousFiles(dstDir, created); err != nil {
		t.Fatalf("cleanupExtraneousFiles failed: %v", err)
	}

	// Verify "extra.txt" is DELETED
	if _, err := os.Stat(extraFile); !os.IsNotExist(err) {
		t.Errorf("Extra file %s was NOT deleted", extraFile)
	}

	// Verify "keep.txt" exists
	if _, err := os.Stat(filepath.Join(dstDir, "keep.txt")); os.IsNotExist(err) {
		t.Errorf("Keep file was not created")
	}

	// Verify "extraDir" is DELETED
	if _, err := os.Stat(extraDir); !os.IsNotExist(err) {
		t.Errorf("Extra dir %s was NOT deleted", extraDir)
	}
}
