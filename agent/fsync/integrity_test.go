package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegrityCheck(t *testing.T) {
	// Setup directories
	hubDir := t.TempDir()
	peerDir := t.TempDir()
	hubChunksDir := filepath.Join(hubDir, ChunksDir)

	if err := os.MkdirAll(hubChunksDir, 0755); err != nil {
		t.Fatalf("Failed to create hub chunks dir: %v", err)
	}

	realHashOfContent := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" // sha256("hello")
	wrongContent := []byte("EVIL DATA")

	// Write EVIL DATA to a file named after the GOOD HASH
	if err := os.WriteFile(filepath.Join(hubChunksDir, realHashOfContent), wrongContent, 0644); err != nil {
		t.Fatalf("Failed to write corrupted chunk: %v", err)
	}

	manifest := Manifest{Chunks: []ChunkInfo{{Hash: realHashOfContent, Size: uint(len(wrongContent))}}}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Failed to marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, ManifestFile), manifestBytes, 0644); err != nil {
		t.Fatalf("Failed to write manifest: %v", err)
	}

	// Serve
	ts := httptest.NewServer(newHubHandler(hubDir))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run Peer - Should fail
	err = runPeer(ctx, peerDir, ts.URL, false, false)
	if err == nil {
		t.Fatal("Expected integrity check failure, got nil")
	}
	t.Logf("Got expected error: %v", err)

	// Verify it didn't save the chunk
	if _, err := os.Stat(filepath.Join(peerDir, ChunksDir, realHashOfContent)); !os.IsNotExist(err) {
		t.Error("Corrupted chunk should not exist on disk")
	}
}
