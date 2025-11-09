package springg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// ReplicationManager handles peer-to-peer vector replication
type ReplicationManager struct {
	cluster     *ClusterManager
	vectorStore *VectorStore
	httpClient  *http.Client

	// Async batching queue
	queue     chan ReplicationMessage
	stopQueue chan bool
}

// ReplicationMessage represents a vector operation to replicate
type ReplicationMessage struct {
	Operation  string          `json:"operation"` // "add", "update", "delete", "create_index", "delete_index"
	IndexName  string          `json:"index_name"`
	VectorID   string          `json:"vector_id,omitempty"`
	Vector     []float32       `json:"vector,omitempty"`
	Metadata   *VectorMetadata `json:"metadata,omitempty"`
	Dimensions int             `json:"dimensions,omitempty"` // For create_index
}

// ReplicationBatch represents a batch of replication messages
type ReplicationBatch struct {
	Messages []ReplicationMessage `json:"messages"`
}

// NewReplicationManager creates a new replication manager
func NewReplicationManager(cluster *ClusterManager, vectorStore *VectorStore) *ReplicationManager {
	rm := &ReplicationManager{
		cluster:     cluster,
		vectorStore: vectorStore,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		queue:     make(chan ReplicationMessage, 1000),
		stopQueue: make(chan bool),
	}

	// Start async batch worker
	go rm.batchWorker()

	return rm
}

// ReplicateAdd replicates a vector addition to all peers (async, batched)
func (r *ReplicationManager) ReplicateAdd(ctx context.Context, indexName, vectorID string, vector []float32, metadata *VectorMetadata) {
	msg := ReplicationMessage{
		Operation: "add",
		IndexName: indexName,
		VectorID:  vectorID,
		Vector:    vector,
		Metadata:  metadata,
	}

	// Enqueue message (non-blocking)
	select {
	case r.queue <- msg:
	default:
		// Queue full, log warning
		log.Printf("⚠️  Replication queue full, dropping message")
	}
}

// ReplicateUpdate replicates a vector update to all peers (async, batched)
func (r *ReplicationManager) ReplicateUpdate(ctx context.Context, indexName, vectorID string, vector []float32, metadata *VectorMetadata) {
	msg := ReplicationMessage{
		Operation: "update",
		IndexName: indexName,
		VectorID:  vectorID,
		Vector:    vector,
		Metadata:  metadata,
	}

	select {
	case r.queue <- msg:
	default:
		log.Printf("⚠️  Replication queue full, dropping message")
	}
}

// ReplicateDelete replicates a vector deletion to all peers (async, batched)
func (r *ReplicationManager) ReplicateDelete(ctx context.Context, indexName, vectorID string) {
	msg := ReplicationMessage{
		Operation: "delete",
		IndexName: indexName,
		VectorID:  vectorID,
	}

	select {
	case r.queue <- msg:
	default:
		log.Printf("⚠️  Replication queue full, dropping message")
	}
}

// ReplicateCreateIndex replicates index creation to all peers (async)
func (r *ReplicationManager) ReplicateCreateIndex(ctx context.Context, indexName string, dimensions int) {
	msg := ReplicationMessage{
		Operation:  "create_index",
		IndexName:  indexName,
		Dimensions: dimensions,
	}

	select {
	case r.queue <- msg:
	default:
		log.Printf("⚠️  Replication queue full, dropping message")
	}
}

// ReplicateDeleteIndex replicates index deletion to all peers (async)
func (r *ReplicationManager) ReplicateDeleteIndex(ctx context.Context, indexName string) {
	msg := ReplicationMessage{
		Operation: "delete_index",
		IndexName: indexName,
	}

	select {
	case r.queue <- msg:
	default:
		log.Printf("⚠️  Replication queue full, dropping message")
	}
}

// batchWorker processes replication queue in batches
func (r *ReplicationManager) batchWorker() {
	ticker := time.NewTicker(50 * time.Millisecond) // Batch every 50ms
	defer ticker.Stop()

	batch := make([]ReplicationMessage, 0, 100)

	for {
		select {
		case <-r.stopQueue:
			// Flush remaining batch
			if len(batch) > 0 {
				r.broadcastBatch(context.Background(), batch)
			}
			return

		case msg := <-r.queue:
			batch = append(batch, msg)

			// Flush if batch is full
			if len(batch) >= 100 {
				r.broadcastBatch(context.Background(), batch)
				batch = make([]ReplicationMessage, 0, 100)
			}

		case <-ticker.C:
			// Periodic flush
			if len(batch) > 0 {
				r.broadcastBatch(context.Background(), batch)
				batch = make([]ReplicationMessage, 0, 100)
			}
		}
	}
}

// broadcastBatch sends a batch of messages to all peers
func (r *ReplicationManager) broadcastBatch(ctx context.Context, batch []ReplicationMessage) {
	if r.cluster == nil || len(batch) == 0 {
		return
	}

	peerStates := r.cluster.GetPeerStates()
	myNodeID := r.cluster.nodeID
	delete(peerStates, myNodeID)

	if len(peerStates) == 0 {
		return
	}

	// Replicate to each peer asynchronously
	for _, peer := range peerStates {
		go r.replicateBatchToPeer(ctx, peer.IP, batch)
	}
}

// replicateBatchToPeer sends a batch of replication messages to a single peer
func (r *ReplicationManager) replicateBatchToPeer(ctx context.Context, peerIP string, batch []ReplicationMessage) {
	url := fmt.Sprintf("http://%s:%d/internal/replicate", peerIP, 8080)

	payload, err := json.Marshal(ReplicationBatch{Messages: batch})
	if err != nil {
		log.Printf("⚠️  Failed to marshal replication batch: %v", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		log.Printf("⚠️  Failed to create replication request: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Batch-Size", fmt.Sprintf("%d", len(batch)))

	resp, err := r.httpClient.Do(req)
	if err != nil {
		log.Printf("⚠️  Failed to replicate batch to %s: %v", peerIP, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("⚠️  Batch replication to %s failed with status: %d", peerIP, resp.StatusCode)
	}
}

// Shutdown gracefully shuts down the replication manager
func (r *ReplicationManager) Shutdown() {
	close(r.stopQueue)
}

// HandleReplicationMessage processes an incoming replication message
func (r *ReplicationManager) HandleReplicationMessage(msg ReplicationMessage) error {
	switch msg.Operation {
	case "create_index":
		// Create index locally (without triggering replication)
		err := r.vectorStore.CreateIndex(msg.IndexName, msg.Dimensions)
		if err != nil && !contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create index: %w", err)
		}
		// Ignore "already exists" errors during replication
		return nil

	case "delete_index":
		// Delete index locally (without triggering replication)
		err := r.vectorStore.DeleteIndex(msg.IndexName)
		if err != nil && !contains(err.Error(), "not found") {
			return fmt.Errorf("failed to delete index: %w", err)
		}
		// Ignore "not found" errors during replication
		return nil

	case "add", "update", "delete":
		// Vector operations require the index to exist
		index, err := r.vectorStore.GetIndex(msg.IndexName)
		if err != nil {
			return fmt.Errorf("index not found: %w", err)
		}

		switch msg.Operation {
		case "add":
			// Apply vector addition locally (without triggering replication)
			return index.AddVector(msg.VectorID, msg.Vector, msg.Metadata)

		case "update":
			// Apply vector update locally
			return index.UpdateVector(msg.VectorID, msg.Vector, msg.Metadata)

		case "delete":
			// Apply vector deletion locally
			return index.DeleteVector(msg.VectorID)
		}

	default:
		return fmt.Errorf("unknown operation: %s", msg.Operation)
	}

	return nil
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
