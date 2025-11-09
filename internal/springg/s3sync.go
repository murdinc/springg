package springg

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Syncer handles syncing vector indexes to/from S3
type S3Syncer struct {
	client   *s3.Client
	bucket   string
	dataPath string
	lastSync map[string]time.Time // Track last sync time per index
	cfg      *Config
}

// NewS3Syncer creates a new S3 syncer
func NewS3Syncer(cfg *Config) (*S3Syncer, error) {
	if !cfg.S3Enabled {
		return nil, nil // S3 disabled, return nil
	}

	// Load AWS config
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(cfg.S3Region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg)

	return &S3Syncer{
		client:   client,
		bucket:   cfg.S3Bucket,
		dataPath: cfg.DataPath,
		lastSync: make(map[string]time.Time),
		cfg:      cfg,
	}, nil
}

// DownloadAllIndexes downloads all indexes from S3 on startup using block-based storage
func (s *S3Syncer) DownloadAllIndexes(ctx context.Context) error {
	if s == nil {
		return nil // S3 disabled
	}

	log.Printf("📥 Downloading indexes from S3 bucket: %s", s.bucket)

	// List all objects in the bucket
	listInput := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	}

	result, err := s.client.ListObjectsV2(ctx, listInput)
	if err != nil {
		return fmt.Errorf("failed to list S3 objects: %w", err)
	}

	// Find all manifest files
	indexNames := make(map[string]bool)
	for _, obj := range result.Contents {
		key := aws.ToString(obj.Key)

		// Look for manifest files: {indexName}/manifest.json
		if strings.HasSuffix(key, "/manifest.json") {
			// Extract index name (everything before /manifest.json)
			indexName := strings.TrimSuffix(key, "/manifest.json")
			indexNames[indexName] = true
		}
	}

	if len(indexNames) == 0 {
		log.Printf("ℹ️  No indexes found in S3 bucket")
		return nil
	}

	log.Printf("📥 Found %d indexes in S3", len(indexNames))

	// Download each index
	totalBlocks := 0
	for indexName := range indexNames {
		blocks, err := s.downloadIndexFromBlocks(ctx, indexName)
		if err != nil {
			log.Printf("⚠️  Failed to download index %s: %v", indexName, err)
			continue
		}
		totalBlocks += blocks
	}

	if totalBlocks > 0 {
		log.Printf("✅ Downloaded %d blocks from S3", totalBlocks)
	}

	return nil
}

// UploadIndex uploads a single index to S3 using block-based storage
func (s *S3Syncer) UploadIndex(ctx context.Context, indexName string) error {
	if s == nil {
		return nil // S3 disabled
	}

	// Note: Using block-based storage only now for efficiency
	// Legacy full-file upload removed to save bandwidth and storage costs

	s.lastSync[indexName] = time.Now()
	return nil
}

// UploadIndexBlocks uploads only changed blocks for an index to S3
func (s *S3Syncer) UploadIndexBlocks(ctx context.Context, indexName string, index *VectorIndex) error {
	if s == nil {
		return nil // S3 disabled
	}

	// Generate current manifest
	localManifest := index.GenerateManifest()

	// Download existing manifest from S3 (if exists)
	s3Manifest, err := s.downloadManifest(ctx, indexName)
	if err != nil {
		// No manifest exists, upload all blocks
		log.Printf("📤 No existing S3 manifest for %s, uploading all blocks", indexName)
		return s.uploadAllBlocks(ctx, indexName, localManifest, index)
	}

	// Compare manifests and upload only changed blocks
	changedBlocks := []int{}
	for _, localBlock := range localManifest.Blocks {
		// Check if block exists in S3 manifest
		var s3Block *BlockChecksum
		if localBlock.BlockIndex < len(s3Manifest.Blocks) {
			s3Block = &s3Manifest.Blocks[localBlock.BlockIndex]
		}

		// Upload if new block or hash changed
		if s3Block == nil || s3Block.Hash != localBlock.Hash {
			changedBlocks = append(changedBlocks, localBlock.BlockIndex)
		}
	}

	if len(changedBlocks) == 0 {
		log.Printf("✅ Index %s blocks unchanged", indexName)
		s.lastSync[indexName] = time.Now()
		return nil
	}

	log.Printf("📤 Uploading %d changed blocks for %s", len(changedBlocks), indexName)

	// Upload changed blocks
	for _, blockIdx := range changedBlocks {
		vectors, err := index.GetBlockVectors(blockIdx)
		if err != nil {
			log.Printf("⚠️  Failed to get block %d: %v", blockIdx, err)
			continue
		}

		if err := s.uploadBlock(ctx, indexName, blockIdx, vectors); err != nil {
			log.Printf("⚠️  Failed to upload block %d: %v", blockIdx, err)
		}
	}

	// Upload new manifest
	if err := s.uploadManifest(ctx, indexName, localManifest); err != nil {
		return err
	}

	// Record successful sync
	s.lastSync[indexName] = time.Now()
	return nil
}

// uploadAllBlocks uploads all blocks for an index
func (s *S3Syncer) uploadAllBlocks(ctx context.Context, indexName string, manifest *IndexManifest, index *VectorIndex) error {
	for _, block := range manifest.Blocks {
		vectors, err := index.GetBlockVectors(block.BlockIndex)
		if err != nil {
			return err
		}

		if err := s.uploadBlock(ctx, indexName, block.BlockIndex, vectors); err != nil {
			return err
		}
	}

	if err := s.uploadManifest(ctx, indexName, manifest); err != nil {
		return err
	}

	// Record successful sync
	s.lastSync[indexName] = time.Now()
	return nil
}

// uploadBlock uploads a single block to S3
func (s *S3Syncer) uploadBlock(ctx context.Context, indexName string, blockIndex int, vectors []VectorEntry) error {
	key := fmt.Sprintf("%s/blocks/block-%03d.json", indexName, blockIndex)

	data, err := json.Marshal(vectors)
	if err != nil {
		return err
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
	})

	return err
}

// uploadManifest uploads the manifest to S3
func (s *S3Syncer) uploadManifest(ctx context.Context, indexName string, manifest *IndexManifest) error {
	key := fmt.Sprintf("%s/manifest.json", indexName)

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
	})

	return err
}

// downloadManifest downloads the manifest from S3
func (s *S3Syncer) downloadManifest(ctx context.Context, indexName string) (*IndexManifest, error) {
	key := fmt.Sprintf("%s/manifest.json", indexName)

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	var manifest IndexManifest
	if err := json.NewDecoder(result.Body).Decode(&manifest); err != nil {
		return nil, err
	}

	return &manifest, nil
}

// downloadIndexFromBlocks downloads an index from S3 using block-based storage
// and reconstructs the .bin and .meta files locally
func (s *S3Syncer) downloadIndexFromBlocks(ctx context.Context, indexName string) (int, error) {
	log.Printf("📥 Downloading index %s from S3 blocks", indexName)

	// Download manifest
	manifest, err := s.downloadManifest(ctx, indexName)
	if err != nil {
		return 0, fmt.Errorf("failed to download manifest: %w", err)
	}

	log.Printf("📥 Manifest has %d blocks for index %s", len(manifest.Blocks), indexName)

	// Download all blocks and collect vectors
	allVectors := make(map[string]*VectorEntry)
	dimensions := 0
	for _, blockInfo := range manifest.Blocks {
		vectors, err := s.downloadBlock(ctx, indexName, blockInfo.BlockIndex)
		if err != nil {
			log.Printf("⚠️  Failed to download block %d: %v", blockInfo.BlockIndex, err)
			continue
		}

		// Add vectors to collection
		for i := range vectors {
			allVectors[vectors[i].ID] = &vectors[i]
			// Get dimensions from first vector
			if dimensions == 0 && len(vectors[i].Vector) > 0 {
				dimensions = len(vectors[i].Vector)
			}
		}
	}

	if len(allVectors) == 0 {
		log.Printf("⚠️  No vectors downloaded for index %s", indexName)
		return 0, nil
	}

	if dimensions == 0 {
		return 0, fmt.Errorf("could not determine dimensions for index %s", indexName)
	}

	log.Printf("📥 Downloaded %d vectors across %d blocks (dimensions=%d)", len(allVectors), len(manifest.Blocks), dimensions)

	// Now reconstruct .bin and .meta files
	if err := s.reconstructIndexFiles(indexName, dimensions, allVectors); err != nil {
		return 0, fmt.Errorf("failed to reconstruct index files: %w", err)
	}

	log.Printf("✅ Successfully downloaded index %s from S3", indexName)
	return len(manifest.Blocks), nil
}

// downloadBlock downloads a single block from S3
func (s *S3Syncer) downloadBlock(ctx context.Context, indexName string, blockIndex int) ([]VectorEntry, error) {
	key := fmt.Sprintf("%s/blocks/block-%03d.json", indexName, blockIndex)

	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	var vectors []VectorEntry
	if err := json.NewDecoder(result.Body).Decode(&vectors); err != nil {
		return nil, err
	}

	return vectors, nil
}

// reconstructIndexFiles creates .bin and .meta files from downloaded vectors
func (s *S3Syncer) reconstructIndexFiles(indexName string, dimensions int, vectors map[string]*VectorEntry) error {
	// Create sorted list of IDs for deterministic ordering
	sortedIDs := make([]string, 0, len(vectors))
	for id := range vectors {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	// Create .bin file
	binaryPath := filepath.Join(s.dataPath, indexName+".bin")
	tmpBinaryPath := binaryPath + ".tmp"

	file, err := os.Create(tmpBinaryPath)
	if err != nil {
		return fmt.Errorf("failed to create temp binary file: %w", err)
	}
	defer func() {
		file.Close()
		os.Remove(tmpBinaryPath) // Clean up on error
	}()

	// Write binary header
	header := BinaryHeader{
		Magic:       [8]byte{'S', 'P', 'R', 'I', 'N', 'G', 'G', '1'},
		Version:     1,
		Dimensions:  uint32(dimensions),
		VectorCount: uint32(len(vectors)),
	}

	if err := binary.Write(file, binary.LittleEndian, &header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	// Write vectors in sorted order
	metadata := make(map[string]*VectorMetadata)
	for _, id := range sortedIDs {
		entry := vectors[id]

		// Write key length and key
		keyBytes := []byte(id)
		keyLen := uint32(len(keyBytes))
		if err := binary.Write(file, binary.LittleEndian, keyLen); err != nil {
			return fmt.Errorf("failed to write key length: %w", err)
		}
		if _, err := file.Write(keyBytes); err != nil {
			return fmt.Errorf("failed to write key: %w", err)
		}

		// Write vector data
		if err := binary.Write(file, binary.LittleEndian, entry.Vector); err != nil {
			return fmt.Errorf("failed to write vector: %w", err)
		}

		// Collect metadata
		if entry.Metadata != nil {
			metadata[id] = entry.Metadata
		}
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync binary file: %w", err)
	}
	file.Close()

	// Atomic replacement of binary file
	if err := os.Rename(tmpBinaryPath, binaryPath); err != nil {
		return fmt.Errorf("failed to replace binary file: %w", err)
	}

	// Create .meta file if there's metadata
	if len(metadata) > 0 {
		metaPath := filepath.Join(s.dataPath, indexName+".meta")
		tmpMetaPath := metaPath + ".tmp"

		metaData, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}

		if err := os.WriteFile(tmpMetaPath, metaData, 0644); err != nil {
			return fmt.Errorf("failed to write metadata: %w", err)
		}

		if err := os.Rename(tmpMetaPath, metaPath); err != nil {
			os.Remove(tmpMetaPath)
			return fmt.Errorf("failed to replace metadata: %w", err)
		}
	}

	return nil
}

// UploadAllIndexes uploads all indexes to S3
func (s *S3Syncer) UploadAllIndexes(ctx context.Context, store *VectorStore) error {
	if s == nil {
		return nil // S3 disabled
	}

	log.Printf("📤 Syncing indexes to S3 bucket: %s", s.bucket)

	store.mu.RLock()
	indexNames := make([]string, 0, len(store.indexes))
	for name := range store.indexes {
		indexNames = append(indexNames, name)
	}
	store.mu.RUnlock()

	uploadCount := 0
	for _, name := range indexNames {
		if err := s.UploadIndex(ctx, name); err != nil {
			log.Printf("⚠️  Failed to upload index %s: %v", name, err)
			continue
		}
		uploadCount++
	}

	if uploadCount > 0 {
		log.Printf("✅ Uploaded %d indexes to S3", uploadCount)
	}

	return nil
}

// DeleteIndex deletes an index from S3 (manifest and all blocks)
func (s *S3Syncer) DeleteIndex(ctx context.Context, indexName string) error {
	if s == nil {
		return nil // S3 disabled
	}

	log.Printf("🗑️  Deleting index %s from S3", indexName)

	// List all objects with prefix {indexName}/
	listInput := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(indexName + "/"),
	}

	result, err := s.client.ListObjectsV2(ctx, listInput)
	if err != nil {
		return fmt.Errorf("failed to list S3 objects: %w", err)
	}

	if len(result.Contents) == 0 {
		log.Printf("ℹ️  No S3 objects found for index %s", indexName)
		return nil
	}

	// Delete all objects
	deleteCount := 0
	for _, obj := range result.Contents {
		key := aws.ToString(obj.Key)

		_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})

		if err != nil {
			log.Printf("⚠️  Failed to delete S3 object %s: %v", key, err)
			continue
		}

		deleteCount++
	}

	log.Printf("✅ Deleted %d S3 objects for index %s", deleteCount, indexName)

	// Clean up last sync tracking
	delete(s.lastSync, indexName)

	return nil
}

// ShouldSync checks if an index needs syncing based on last sync time
func (s *S3Syncer) ShouldSync(indexName string) bool {
	if s == nil {
		return false
	}

	lastSync, exists := s.lastSync[indexName]
	if !exists {
		return true // Never synced
	}

	return time.Since(lastSync) >= s.cfg.S3SyncDuration
}

// downloadFile downloads a file from S3
func (s *S3Syncer) downloadFile(ctx context.Context, key, localPath string) error {
	// Ensure directory exists
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Get object from S3
	getInput := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}

	result, err := s.client.GetObject(ctx, getInput)
	if err != nil {
		return fmt.Errorf("failed to get object from S3: %w", err)
	}
	defer result.Body.Close()

	// Create local file
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer file.Close()

	// Copy data
	if _, err := io.Copy(file, result.Body); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// uploadFile uploads a file to S3
func (s *S3Syncer) uploadFile(ctx context.Context, localPath, key string) error {
	// Open local file
	file, err := os.Open(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist, skip
		}
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Upload to S3
	putInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   file,
	}

	_, err = s.client.PutObject(ctx, putInput)
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	return nil
}

// WriteHeartbeat writes a heartbeat file to S3 to confirm connectivity
func (s *S3Syncer) WriteHeartbeat(ctx context.Context, nodeID string) error {
	if s == nil {
		return nil // S3 disabled
	}

	heartbeat := map[string]interface{}{
		"node_id":   nodeID,
		"timestamp": time.Now().Format(time.RFC3339),
		"status":    "healthy",
	}

	data, err := json.MarshalIndent(heartbeat, "", "  ")
	if err != nil {
		return err
	}

	key := fmt.Sprintf("_heartbeats/%s.json", nodeID)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(string(data)),
	})

	if err != nil {
		return fmt.Errorf("failed to write heartbeat: %w", err)
	}

	log.Printf("💓 Heartbeat written to S3: %s", key)
	return nil
}

// StartPeriodicSync starts a background goroutine that periodically syncs to S3
func (s *S3Syncer) StartPeriodicSync(ctx context.Context, store *VectorStore, cluster *ClusterManager) {
	if s == nil {
		return // S3 disabled
	}

	ticker := time.NewTicker(s.cfg.S3SyncDuration)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Check if this node should sync to S3 (cluster coordination)
				shouldSync := true
				if cluster != nil && cluster.GetClusterSize() > 1 {
					shouldSync = cluster.ShouldSyncToS3()
					if !shouldSync {
						log.Printf("⏭️  Skipping S3 sync (another node will handle it)")
						continue
					}
					log.Printf("✅ Selected for S3 sync among %d nodes", cluster.GetClusterSize())
				}

				// Sync dirty indexes
				store.mu.RLock()
				for name, index := range store.indexes {
					// Check if enough time has passed since last sync
					if s.ShouldSync(name) {
						// Flush to disk first (in case there are any pending changes)
						if err := index.Flush(); err != nil {
							log.Printf("⚠️  Failed to flush index %s: %v", name, err)
							continue
						}

						// Upload blocks to S3 (efficient incremental sync)
						if err := s.UploadIndexBlocks(ctx, name, index); err != nil {
							log.Printf("⚠️  Failed to sync index %s blocks to S3: %v", name, err)
					}
					}
				}
				store.mu.RUnlock()

				// Update cluster state whenever we run sync (even if no changes)
				if cluster != nil {
					cluster.UpdateS3SyncTime(time.Now())
				}
			}
		}
	}()

	log.Printf("🔄 S3 periodic sync started (interval: %s)", s.cfg.S3SyncInterval)
}
