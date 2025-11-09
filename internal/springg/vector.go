package springg

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unsafe"
)

// VectorMetadata stores additional data associated with a vector
type VectorMetadata struct {
	PublishedDate string                 `json:"published_date,omitempty"`
	ModifiedDate  string                 `json:"modified_date,omitempty"`
	Custom        map[string]interface{} `json:"custom,omitempty"`
}

// VectorEntry represents a vector with its metadata
type VectorEntry struct {
	ID       string          `json:"id"`
	Vector   []float32       `json:"vector"`
	Metadata *VectorMetadata `json:"metadata,omitempty"`
}

// SearchResult represents a search result with similarity score
type SearchResult struct {
	ID       string                 `json:"id"`
	Score    float64                `json:"score"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// searchResult is an internal type for search processing
type searchResult struct {
	id   string
	dist float32
}

// VectorIndex represents a named vector index with binary storage
type VectorIndex struct {
	Name       string
	Dimensions int
	vectors    map[string][]float32
	metadata   map[string]*VectorMetadata
	sortedIDs  []string        // Deterministic sorted order for consistent hashing
	deletedIDs map[string]bool // Track deleted IDs for compaction
	mu         sync.RWMutex
	dataPath   string
	dirty      bool
	store      *VectorStore // Reference to parent store for memory checks

	// Write-Ahead Log
	wal *WAL

	// Async persistence
	saveQueue chan bool
	stopSave  chan bool
}

// VectorStore manages multiple vector indexes
type VectorStore struct {
	indexes     map[string]*VectorIndex
	mu          sync.RWMutex
	dataPath    string
	maxMemoryMB int // Maximum memory in MB (0 = unlimited)
}

// BinaryHeader represents the binary file header
type BinaryHeader struct {
	Magic       [8]byte // "SPRINGG1"
	Version     uint32
	Dimensions  uint32
	VectorCount uint32
	Reserved    [12]byte
}

// BlockChecksum represents a checksum for a block of vectors
type BlockChecksum struct {
	BlockIndex int    `json:"block_index"`
	StartID    int    `json:"start_id"`  // Starting position in sortedIDs
	EndID      int    `json:"end_id"`    // Ending position in sortedIDs
	Hash       string `json:"hash"`      // SHA256 hash of block data
	FirstKey   string `json:"first_key"` // First vector ID in block (for debugging)
	LastKey    string `json:"last_key"`  // Last vector ID in block (for debugging)
}

// IndexManifest represents the manifest of an index with block checksums
type IndexManifest struct {
	IndexName  string          `json:"index_name"`
	TotalCount int             `json:"total_count"`
	BlockSize  int             `json:"block_size"`
	Blocks     []BlockChecksum `json:"blocks"`
}

// NewVectorStore creates a new vector store
func NewVectorStore(dataPath string, maxMemoryMB int) (*VectorStore, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	store := &VectorStore{
		indexes:     make(map[string]*VectorIndex),
		dataPath:    dataPath,
		maxMemoryMB: maxMemoryMB,
	}

	// Load existing indexes from disk
	if err := store.loadIndexes(); err != nil {
		return nil, fmt.Errorf("failed to load indexes: %w", err)
	}

	return store, nil
}

// CreateIndex creates a new vector index
func (vs *VectorStore) CreateIndex(name string, dimensions int) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if _, exists := vs.indexes[name]; exists {
		return fmt.Errorf("index %s already exists", name)
	}

	// Create WAL
	wal, err := NewWAL(name, vs.dataPath)
	if err != nil {
		return fmt.Errorf("failed to create WAL: %w", err)
	}

	index := &VectorIndex{
		Name:       name,
		Dimensions: dimensions,
		vectors:    make(map[string][]float32),
		metadata:   make(map[string]*VectorMetadata),
		sortedIDs:  make([]string, 0),
		deletedIDs: make(map[string]bool),
		dataPath:  vs.dataPath,
		dirty:     true,
		store:     vs,
		wal:       wal,
		saveQueue: make(chan bool, 100),
		stopSave:   make(chan bool),
	}

	// Start async save goroutine
	go index.asyncSaveWorker()

	vs.indexes[name] = index

	// Persist to disk
	if err := index.save(); err != nil {
		delete(vs.indexes, name)
		return fmt.Errorf("failed to save index: %w", err)
	}

	return nil
}

// DeleteIndex deletes an index
func (vs *VectorStore) DeleteIndex(name string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	index, exists := vs.indexes[name]
	if !exists {
		return fmt.Errorf("index %s not found", name)
	}

	// Close WAL if open
	if index.wal != nil {
		if err := index.wal.Close(); err != nil {
			log.Printf("⚠️  Failed to close WAL for %s: %v", name, err)
		}
	}

	// Remove binary file
	binaryPath := filepath.Join(vs.dataPath, name+".bin")
	if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete binary file: %w", err)
	}

	// Remove metadata file
	metaPath := filepath.Join(vs.dataPath, name+".meta")
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete metadata file: %w", err)
	}

	// Remove WAL file
	walPath := filepath.Join(vs.dataPath, name+".wal")
	if err := os.Remove(walPath); err != nil && !os.IsNotExist(err) {
		log.Printf("⚠️  Failed to delete WAL file for %s: %v", name, err)
	}

	delete(vs.indexes, name)
	return nil
}

// GetIndex returns an index by name
func (vs *VectorStore) GetIndex(name string) (*VectorIndex, error) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	index, exists := vs.indexes[name]
	if !exists {
		return nil, fmt.Errorf("index %s not found", name)
	}

	return index, nil
}

// ListIndexes returns all index names with basic stats
func (vs *VectorStore) ListIndexes() []map[string]interface{} {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(vs.indexes))
	for _, index := range vs.indexes {
		index.mu.RLock()
		result = append(result, map[string]interface{}{
			"name":       index.Name,
			"dimensions": index.Dimensions,
			"count":      len(index.vectors),
		})
		index.mu.RUnlock()
	}

	return result
}

// AddVector adds a vector to the index with optional metadata
func (vi *VectorIndex) AddVector(id string, vector []float32, metadata *VectorMetadata) error {
	if len(vector) != vi.Dimensions {
		return fmt.Errorf("vector dimension mismatch: expected %d, got %d", vi.Dimensions, len(vector))
	}

	// Normalize vector for cosine similarity
	vector = normalizeVector(vector)

	vi.mu.Lock()
	defer vi.mu.Unlock()

	// Check if vector already exists
	isNew := vi.vectors[id] == nil

	// Check memory limit for new vectors only
	if isNew && vi.store != nil {
		// Accurate size calculation using unsafe.Sizeof
		vectorSize := uint64(len(vector) * 4) // 4 bytes per float32
		idSize := uint64(len(id))

		// Calculate metadata size accurately
		metadataSize := uint64(unsafe.Sizeof(VectorMetadata{}))
		if metadata != nil {
			metadataSize += uint64(len(metadata.PublishedDate) + len(metadata.ModifiedDate))
			if metadata.Custom != nil {
				for k, v := range metadata.Custom {
					metadataSize += uint64(len(k))
					metadataSize += uint64(unsafe.Sizeof(v))
				}
			}
		}

		// Add map/slice overhead (approximate)
		totalSize := vectorSize + idSize + metadataSize + 64

		if err := vi.store.checkMemoryLimit(totalSize); err != nil {
			return err
		}
	}

	// Log to WAL before making changes
	if vi.wal != nil {
		if err := vi.wal.LogAdd(id, vector, metadata); err != nil {
			return fmt.Errorf("WAL write failed: %w", err)
		}
	}

	vi.vectors[id] = vector
	if metadata != nil {
		vi.metadata[id] = metadata
	} else {
		// Explicitly remove metadata when nil is passed
		delete(vi.metadata, id)
	}

	// Remove from deleted set if re-adding
	delete(vi.deletedIDs, id)

	// Maintain sorted order for deterministic block hashing
	if isNew {
		// Binary search for insertion point
		insertIdx := sort.SearchStrings(vi.sortedIDs, id)

		// Insert at correct position to maintain sorted order
		vi.sortedIDs = append(vi.sortedIDs, "")
		copy(vi.sortedIDs[insertIdx+1:], vi.sortedIDs[insertIdx:])
		vi.sortedIDs[insertIdx] = id
	}

	vi.dirty = true

	// Trigger async save (non-blocking)
	select {
	case vi.saveQueue <- true:
	default:
		// Queue full, skip this signal
	}

	return nil
}

// UpdateVector updates an existing vector
func (vi *VectorIndex) UpdateVector(id string, vector []float32, metadata *VectorMetadata) error {
	if len(vector) != vi.Dimensions {
		return fmt.Errorf("vector dimension mismatch: expected %d, got %d", vi.Dimensions, len(vector))
	}

	vi.mu.Lock()
	defer vi.mu.Unlock()

	if _, exists := vi.vectors[id]; !exists {
		return fmt.Errorf("vector %s not found", id)
	}

	// Log to WAL before making changes
	if vi.wal != nil {
		if err := vi.wal.LogUpdate(id, vector, metadata); err != nil {
			return fmt.Errorf("WAL write failed: %w", err)
		}
	}

	vi.vectors[id] = vector
	if metadata != nil {
		vi.metadata[id] = metadata
	} else {
		// Explicitly remove metadata when nil is passed
		delete(vi.metadata, id)
	}
	vi.dirty = true

	return nil
}

// DeleteVector deletes a vector by ID
func (vi *VectorIndex) DeleteVector(id string) error {
	vi.mu.Lock()
	defer vi.mu.Unlock()

	if _, exists := vi.vectors[id]; !exists {
		return fmt.Errorf("vector %s not found", id)
	}

	// Log to WAL before making changes
	if vi.wal != nil {
		if err := vi.wal.LogDelete(id); err != nil {
			return fmt.Errorf("WAL write failed: %w", err)
		}
	}

	delete(vi.vectors, id)
	delete(vi.metadata, id)

	// Track deleted ID for compaction
	vi.deletedIDs[id] = true

	// Remove from sorted IDs using binary search
	idx := sort.SearchStrings(vi.sortedIDs, id)
	if idx < len(vi.sortedIDs) && vi.sortedIDs[idx] == id {
		vi.sortedIDs = append(vi.sortedIDs[:idx], vi.sortedIDs[idx+1:]...)
	}

	vi.dirty = true

	// Trigger compaction if too many deletes
	if len(vi.deletedIDs) > 1000 {
		go vi.compact()
	}

	return nil
}

// compact performs index compaction to clean up deleted vectors
func (vi *VectorIndex) compact() {
	vi.mu.Lock()
	defer vi.mu.Unlock()

	// Clear deleted IDs tracking
	vi.deletedIDs = make(map[string]bool)

	// Rebuild sortedIDs from current vectors (already clean from DeleteVector)
	// This is already maintained correctly, just reset the deleted tracker

	vi.dirty = true
}

// Search performs cosine similarity search with metadata
func (vi *VectorIndex) Search(query []float32, k int) ([]SearchResult, error) {
	if len(query) != vi.Dimensions {
		return nil, fmt.Errorf("query dimension mismatch: expected %d, got %d", vi.Dimensions, len(query))
	}

	// Normalize query vector for cosine similarity
	query = normalizeVector(query)

	vi.mu.RLock()
	defer vi.mu.RUnlock()

	if len(vi.vectors) == 0 {
		return []SearchResult{}, nil
	}

	// Brute force search - exact nearest neighbor
	results := make([]searchResult, 0, len(vi.vectors))
	for _, id := range vi.sortedIDs {
		vector := vi.vectors[id]
		similarity := fastCosineSimilarity(query, vector)
		results = append(results, searchResult{
			id:   id,
			dist: similarity,
		})
	}

	// Sort by similarity (highest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].dist > results[j].dist
	})

	// Limit to top-k
	if k < len(results) {
		results = results[:k]
	}

	// Assemble final results with metadata (optimized)
	searchResults := make([]SearchResult, len(results))
	for i, r := range results {
		searchResults[i] = SearchResult{
			ID:    r.id,
			Score: float64(r.dist),
		}

		// Optimize metadata assembly - only create map if metadata exists
		if meta, exists := vi.metadata[r.id]; exists && meta != nil {
			// Pre-calculate map size to avoid reallocations
			mapSize := 0
			if meta.PublishedDate != "" {
				mapSize++
			}
			if meta.ModifiedDate != "" {
				mapSize++
			}
			if meta.Custom != nil {
				mapSize += len(meta.Custom)
			}

			if mapSize > 0 {
				searchResults[i].Metadata = make(map[string]interface{}, mapSize)
				if meta.PublishedDate != "" {
					searchResults[i].Metadata["published_date"] = meta.PublishedDate
				}
				if meta.ModifiedDate != "" {
					searchResults[i].Metadata["modified_date"] = meta.ModifiedDate
				}
				if meta.Custom != nil {
					for k, v := range meta.Custom {
						searchResults[i].Metadata[k] = v
					}
				}
			}
		}
	}

	return searchResults, nil
}

// GetVector retrieves a vector and its metadata by ID
func (vi *VectorIndex) GetVector(id string) (*VectorEntry, error) {
	vi.mu.RLock()
	defer vi.mu.RUnlock()

	vector, exists := vi.vectors[id]
	if !exists {
		return nil, fmt.Errorf("vector %s not found", id)
	}

	entry := &VectorEntry{
		ID:     id,
		Vector: vector,
	}

	if meta, exists := vi.metadata[id]; exists {
		entry.Metadata = meta
	}

	return entry, nil
}

// Stats returns index statistics
func (vi *VectorIndex) Stats() map[string]interface{} {
	vi.mu.RLock()
	defer vi.mu.RUnlock()

	vectorCount := len(vi.vectors)
	memoryUsage := vi.getMemoryUsage()
	diskSize := vi.getDiskSize()

	return map[string]interface{}{
		"name":         vi.Name,
		"dimensions":   vi.Dimensions,
		"vector_count": vectorCount,
		"memory_bytes": memoryUsage,
		"disk_size":    diskSize,
		"dirty":        vi.dirty,
	}
}

// Flush forces a save to disk
func (vi *VectorIndex) Flush() error {
	vi.mu.Lock()
	defer vi.mu.Unlock()
	return vi.save()
}

// save persists the index to disk using binary format
func (vi *VectorIndex) save() error {
	if !vi.dirty {
		return nil
	}

	binaryPath := filepath.Join(vi.dataPath, vi.Name+".bin")
	tmpPath := binaryPath + ".tmp"

	// Create temporary file
	file, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer file.Close()

	// Write binary header
	header := BinaryHeader{
		Magic:       [8]byte{'S', 'P', 'R', 'I', 'N', 'G', 'G', '1'},
		Version:     1,
		Dimensions:  uint32(vi.Dimensions),
		VectorCount: uint32(len(vi.vectors)),
	}

	if err := binary.Write(file, binary.LittleEndian, &header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	// Write vectors in sorted order for deterministic block hashing
	for _, key := range vi.sortedIDs {
		vector := vi.vectors[key]

		// Write key length and key
		keyBytes := []byte(key)
		keyLen := uint32(len(keyBytes))
		if err := binary.Write(file, binary.LittleEndian, keyLen); err != nil {
			return fmt.Errorf("failed to write key length: %w", err)
		}
		if _, err := file.Write(keyBytes); err != nil {
			return fmt.Errorf("failed to write key: %w", err)
		}

		// Write vector data
		if err := binary.Write(file, binary.LittleEndian, vector); err != nil {
			return fmt.Errorf("failed to write vector: %w", err)
		}
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Prepare metadata file (write to temp location first)
	metaPath := filepath.Join(vi.dataPath, vi.Name+".meta")
	tmpMetaPath := metaPath + ".tmp"
	var hasMetadata bool

	if len(vi.metadata) > 0 {
		metaData, err := json.MarshalIndent(vi.metadata, "", "  ")
		if err != nil {
			os.Remove(tmpPath) // Clean up binary temp file
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}

		if err := os.WriteFile(tmpMetaPath, metaData, 0644); err != nil {
			os.Remove(tmpPath) // Clean up binary temp file
			return fmt.Errorf("failed to write metadata: %w", err)
		}
		hasMetadata = true
	}

	// Atomic replacement: commit both files together
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		os.Remove(tmpPath)
		if hasMetadata {
			os.Remove(tmpMetaPath)
		}
		return fmt.Errorf("failed to replace binary file: %w", err)
	}

	if hasMetadata {
		if err := os.Rename(tmpMetaPath, metaPath); err != nil {
			os.Remove(tmpMetaPath)
			return fmt.Errorf("failed to replace metadata: %w", err)
		}
	} else {
		// Clean up orphaned .meta file when metadata becomes empty
		os.Remove(metaPath)
	}

	vi.dirty = false

	// Truncate WAL after successful checkpoint
	if vi.wal != nil {
		if err := vi.wal.Truncate(); err != nil {
			// Log but don't fail
			fmt.Printf("Warning: failed to truncate WAL: %v\n", err)
		}
	}

	return nil
}

// asyncSaveWorker handles async persistence
func (vi *VectorIndex) asyncSaveWorker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-vi.stopSave:
			// Final save before shutdown
			vi.mu.Lock()
			vi.save()
			vi.mu.Unlock()
			return
		case <-vi.saveQueue:
			// Wait a bit to batch multiple signals
			time.Sleep(100 * time.Millisecond)

			// Drain queue
			for len(vi.saveQueue) > 0 {
				<-vi.saveQueue
			}

			vi.mu.Lock()
			if vi.dirty {
				vi.save()
			}
			vi.mu.Unlock()
		case <-ticker.C:
			// Periodic save
			vi.mu.Lock()
			if vi.dirty {
				vi.save()
			}
			vi.mu.Unlock()
		}
	}
}

// loadFromDisk loads vectors from binary file
func (vi *VectorIndex) loadFromDisk() error {
	binaryPath := filepath.Join(vi.dataPath, vi.Name+".bin")
	file, err := os.Open(binaryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No data yet
		}
		return fmt.Errorf("failed to open binary file: %w", err)
	}
	defer file.Close()

	// Read header
	var header BinaryHeader
	if err := binary.Read(file, binary.LittleEndian, &header); err != nil {
		return fmt.Errorf("failed to read header: %w", err)
	}

	// Validate magic
	expectedMagic := [8]byte{'S', 'P', 'R', 'I', 'N', 'G', 'G', '1'}
	if header.Magic != expectedMagic {
		return fmt.Errorf("invalid file format")
	}

	// Validate dimensions
	if int(header.Dimensions) != vi.Dimensions {
		return fmt.Errorf("dimension mismatch: expected %d, got %d", vi.Dimensions, header.Dimensions)
	}

	vi.vectors = make(map[string][]float32, header.VectorCount)
	vi.sortedIDs = make([]string, 0, header.VectorCount)

	// Read vectors (already in sorted order from save)
	for i := uint32(0); i < header.VectorCount; i++ {
		// Read key length
		var keyLen uint32
		if err := binary.Read(file, binary.LittleEndian, &keyLen); err != nil {
			return fmt.Errorf("failed to read key length: %w", err)
		}

		// Read key
		keyBytes := make([]byte, keyLen)
		if _, err := file.Read(keyBytes); err != nil {
			return fmt.Errorf("failed to read key: %w", err)
		}
		key := string(keyBytes)

		// Read vector data
		vector := make([]float32, header.Dimensions)
		if err := binary.Read(file, binary.LittleEndian, vector); err != nil {
			return fmt.Errorf("failed to read vector: %w", err)
		}

		vi.vectors[key] = vector
		vi.sortedIDs = append(vi.sortedIDs, key)
	}

	// Ensure sortedIDs are actually sorted (defensive)
	sort.Strings(vi.sortedIDs)

	// Load metadata if exists
	metaPath := filepath.Join(vi.dataPath, vi.Name+".meta")
	if metaData, err := os.ReadFile(metaPath); err == nil {
		var metadata map[string]*VectorMetadata
		if err := json.Unmarshal(metaData, &metadata); err == nil {
			vi.metadata = metadata
		}
	}

	vi.dirty = false
	return nil
}

// loadIndexes loads all indexes from disk
func (vs *VectorStore) loadIndexes() error {
	entries, err := os.ReadDir(vs.dataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Find all .bin files
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".bin" {
			continue
		}

		indexName := entry.Name()[:len(entry.Name())-4] // Remove .bin extension

		// Determine dimensions from file (read header)
		binaryPath := filepath.Join(vs.dataPath, entry.Name())
		file, err := os.Open(binaryPath)
		if err != nil {
			return fmt.Errorf("failed to open binary file %s: %w", entry.Name(), err)
		}

		var header BinaryHeader
		if err := binary.Read(file, binary.LittleEndian, &header); err != nil {
			file.Close()
			return fmt.Errorf("failed to read header from %s: %w", entry.Name(), err)
		}
		file.Close()

		// Create WAL
		wal, err := NewWAL(indexName, vs.dataPath)
		if err != nil {
			return fmt.Errorf("failed to create WAL for %s: %w", indexName, err)
		}

		// Create index structure
		index := &VectorIndex{
			Name:       indexName,
			Dimensions: int(header.Dimensions),
			vectors:    make(map[string][]float32),
			metadata:   make(map[string]*VectorMetadata),
			sortedIDs:  make([]string, 0),
			deletedIDs: make(map[string]bool),
			dataPath:   vs.dataPath,
			dirty:      false,
			store:      vs,
			wal:        wal,
			saveQueue:  make(chan bool, 100),
			stopSave:   make(chan bool),
		}

		// Load data from disk
		if err := index.loadFromDisk(); err != nil {
			return fmt.Errorf("failed to load index %s: %w", indexName, err)
		}

		// Replay WAL for crash recovery
		if err := ReplayWAL(indexName, vs.dataPath, index); err != nil {
			return fmt.Errorf("failed to replay WAL for %s: %w", indexName, err)
		}

		// Start async save worker
		go index.asyncSaveWorker()

		vs.indexes[indexName] = index
	}

	return nil
}

// getMemoryUsage returns approximate memory usage in bytes
func (vi *VectorIndex) getMemoryUsage() uint64 {
	var total uint64

	// Vector data
	total += uint64(len(vi.vectors)) * uint64(vi.Dimensions) * uint64(unsafe.Sizeof(float32(0)))

	// Keys
	for _, key := range vi.sortedIDs {
		total += uint64(len(key))
	}

	// Map overhead (approximate)
	total += uint64(len(vi.vectors)) * 50

	// Metadata (approximate)
	total += uint64(len(vi.metadata)) * 100

	return total
}

// getDiskSize returns the size of the data file on disk in bytes
func (vi *VectorIndex) getDiskSize() uint64 {
	binaryPath := filepath.Join(vi.dataPath, vi.Name+".bin")
	if fileInfo, err := os.Stat(binaryPath); err == nil {
		size := uint64(fileInfo.Size())

		// Add metadata file size
		metaPath := filepath.Join(vi.dataPath, vi.Name+".meta")
		if metaInfo, err := os.Stat(metaPath); err == nil {
			size += uint64(metaInfo.Size())
		}

		return size
	}
	return 0
}

// fastCosineSimilarity optimized cosine similarity calculation with loop unrolling
func fastCosineSimilarity(a, b []float32) float32 {
	var dotProduct, normA, normB float32

	// Unrolled loop for better performance (process 4 elements at once)
	i := 0
	for i < len(a)-3 {
		dotProduct += a[i]*b[i] + a[i+1]*b[i+1] + a[i+2]*b[i+2] + a[i+3]*b[i+3]
		normA += a[i]*a[i] + a[i+1]*a[i+1] + a[i+2]*a[i+2] + a[i+3]*a[i+3]
		normB += b[i]*b[i] + b[i+1]*b[i+1] + b[i+2]*b[i+2] + b[i+3]*b[i+3]
		i += 4
	}

	// Handle remaining elements
	for i < len(a) {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
		i++
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

// normalizeVector normalizes a vector to unit length for cosine similarity
// Returns a new normalized vector (does not modify the input)
func normalizeVector(v []float32) []float32 {
	// Calculate L2 norm (magnitude)
	var norm float32
	for i := 0; i < len(v); i++ {
		norm += v[i] * v[i]
	}
	norm = float32(math.Sqrt(float64(norm)))

	// If norm is zero or very small, return original vector
	if norm < 1e-10 {
		result := make([]float32, len(v))
		copy(result, v)
		return result
	}

	// Normalize to unit length
	result := make([]float32, len(v))
	for i := 0; i < len(v); i++ {
		result[i] = v[i] / norm
	}
	return result
}

// GenerateManifest creates a manifest with block checksums for the index
func (vi *VectorIndex) GenerateManifest() *IndexManifest {
	vi.mu.RLock()
	defer vi.mu.RUnlock()

	const blockSize = 1000 // 1000 vectors per block
	numBlocks := (len(vi.sortedIDs) + blockSize - 1) / blockSize

	blocks := make([]BlockChecksum, 0, numBlocks)

	for blockIdx := 0; blockIdx < numBlocks; blockIdx++ {
		start := blockIdx * blockSize
		end := start + blockSize
		if end > len(vi.sortedIDs) {
			end = len(vi.sortedIDs)
		}

		blockIDs := vi.sortedIDs[start:end]
		hash := vi.hashBlock(blockIDs)

		block := BlockChecksum{
			BlockIndex: blockIdx,
			StartID:    start,
			EndID:      end,
			Hash:       hash,
		}

		if len(blockIDs) > 0 {
			block.FirstKey = blockIDs[0]
			block.LastKey = blockIDs[len(blockIDs)-1]
		}

		blocks = append(blocks, block)
	}

	return &IndexManifest{
		IndexName:  vi.Name,
		TotalCount: len(vi.sortedIDs),
		BlockSize:  blockSize,
		Blocks:     blocks,
	}
}

// hashBlock computes SHA256 hash of a block of vectors
func (vi *VectorIndex) hashBlock(ids []string) string {
	h := sha256.New()

	// Hash vectors in order
	for _, id := range ids {
		vector := vi.vectors[id]

		// Hash the ID
		h.Write([]byte(id))

		// Hash the vector data
		for _, val := range vector {
			binary.Write(h, binary.LittleEndian, val)
		}

		// Hash metadata if present
		if meta, exists := vi.metadata[id]; exists {
			metaJSON, _ := json.Marshal(meta)
			h.Write(metaJSON)
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// GetBlockVectors returns all vectors in a specific block
func (vi *VectorIndex) GetBlockVectors(blockIndex int) ([]VectorEntry, error) {
	vi.mu.RLock()
	defer vi.mu.RUnlock()

	const blockSize = 1000
	start := blockIndex * blockSize
	end := start + blockSize

	if start >= len(vi.sortedIDs) {
		return nil, fmt.Errorf("block index out of range")
	}

	if end > len(vi.sortedIDs) {
		end = len(vi.sortedIDs)
	}

	blockIDs := vi.sortedIDs[start:end]
	vectors := make([]VectorEntry, len(blockIDs))

	for i, id := range blockIDs {
		vectors[i] = VectorEntry{
			ID:       id,
			Vector:   vi.vectors[id],
			Metadata: vi.metadata[id],
		}
	}

	return vectors, nil
}

// GetTotalMemoryUsage returns total memory usage across all indexes in MB
func (vs *VectorStore) GetTotalMemoryUsage() uint64 {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var total uint64
	for _, index := range vs.indexes {
		total += index.getMemoryUsage()
	}

	return total / (1024 * 1024) // Convert to MB
}

// GetMemoryStats returns detailed memory statistics
func (vs *VectorStore) GetMemoryStats() map[string]interface{} {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	totalBytes := uint64(0)
	indexStats := make(map[string]uint64)

	for name, index := range vs.indexes {
		usage := index.getMemoryUsage()
		indexStats[name] = usage / (1024 * 1024) // MB
		totalBytes += usage
	}

	totalMB := totalBytes / (1024 * 1024)
	limitMB := uint64(vs.maxMemoryMB)

	stats := map[string]interface{}{
		"total_mb":      totalMB,
		"limit_mb":      limitMB,
		"usage_percent": float64(0),
		"per_index_mb":  indexStats,
	}

	if limitMB > 0 {
		stats["usage_percent"] = float64(totalMB) / float64(limitMB) * 100
	}

	return stats
}

// checkMemoryLimit checks if adding a vector would exceed memory limit
func (vs *VectorStore) checkMemoryLimit(additionalBytes uint64) error {
	if vs.maxMemoryMB == 0 {
		return nil // Unlimited
	}

	currentMB := vs.GetTotalMemoryUsage()
	additionalMB := additionalBytes / (1024 * 1024)
	projectedMB := currentMB + additionalMB

	if projectedMB > uint64(vs.maxMemoryMB) {
		return fmt.Errorf("memory limit exceeded: %d MB used + %d MB new = %d MB total (limit: %d MB)",
			currentMB, additionalMB, projectedMB, vs.maxMemoryMB)
	}

	return nil
}

// Shutdown gracefully shuts down an index
func (vi *VectorIndex) Shutdown() error {
	// Stop async save worker
	close(vi.stopSave)

	// Final flush
	vi.mu.Lock()
	if vi.dirty {
		vi.save()
	}
	vi.mu.Unlock()

	// Close WAL
	if vi.wal != nil {
		return vi.wal.Close()
	}

	return nil
}

// Shutdown gracefully shuts down the vector store
func (vs *VectorStore) Shutdown() error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	for name, index := range vs.indexes {
		if err := index.Shutdown(); err != nil {
			return fmt.Errorf("failed to shutdown index %s: %w", name, err)
		}
	}

	return nil
}
