package springg

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// ClusterManager handles peer-to-peer clustering with gossip
type ClusterManager struct {
	memberlist *memberlist.Memberlist
	delegate   *gossipDelegate
	ec2Info    *EC2Info
	localIP    string
	nodeID     string
}

// PeerState represents the state of a peer in the cluster
type PeerState struct {
	NodeID      string    `json:"node_id"`
	IP          string    `json:"ip"`
	LastS3Sync  time.Time `json:"last_s3_sync"`
	VectorCount int       `json:"vector_count"`
	FirstSeen   time.Time `json:"first_seen"` // When this peer was first seen
	LastSeen    time.Time `json:"last_seen"`  // Tracked locally; ignored when receiving gossip
}

// gossipDelegate implements memberlist.Delegate for custom gossip behavior
type gossipDelegate struct {
	localState  *PeerState
	peerStates  map[string]*PeerState
	mu          sync.RWMutex
	broadcasts  *memberlist.TransmitLimitedQueue
	vectorStore *VectorStore
}

// NewClusterManager creates a new cluster manager
func NewClusterManager(ctx context.Context, vectorStore *VectorStore, bindPort int) (*ClusterManager, error) {
	// Detect if running on EC2
	ec2Info, err := DetectEC2(ctx)
	if err != nil {
		log.Printf("⚠️  Failed to detect EC2: %v", err)
	}

	var localIP, nodeID string
	if ec2Info != nil && ec2Info.IsInASG {
		localIP = ec2Info.LocalIP
		nodeID = ec2Info.InstanceID
		log.Printf("🌐 Detected EC2 instance: %s (ASG: %s)", nodeID, ec2Info.ASGName)
	} else {
		// Not in EC2/ASG, use local IP
		localIP, err = getLocalIP()
		if err != nil {
			return nil, fmt.Errorf("failed to get local IP: %w", err)
		}
		nodeID = fmt.Sprintf("local-%s", localIP)
		log.Printf("🌐 Running standalone on %s", localIP)
	}

	// Create gossip delegate
	now := time.Now()
	delegate := &gossipDelegate{
		localState: &PeerState{
			NodeID:      nodeID,
			IP:          localIP,
			LastS3Sync:  time.Time{}, // Never synced yet
			VectorCount: 0,
			FirstSeen:   now,
			LastSeen:    now,
		},
		peerStates:  make(map[string]*PeerState),
		vectorStore: vectorStore,
	}

	// Create memberlist config
	config := memberlist.DefaultLANConfig()
	config.Name = nodeID
	config.BindAddr = localIP
	config.BindPort = bindPort
	config.Delegate = delegate
	// Note: Events delegate will be updated after ClusterManager is created
	config.Events = &clusterEventDelegate{delegate: delegate}

	// Create memberlist
	list, err := memberlist.Create(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %w", err)
	}

	// Initialize broadcast queue
	delegate.broadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes: func() int {
			return list.NumMembers()
		},
		RetransmitMult: 3,
	}

	cm := &ClusterManager{
		memberlist: list,
		delegate:   delegate,
		ec2Info:    ec2Info,
		localIP:    localIP,
		nodeID:     nodeID,
	}

	// Update event delegate with cluster manager and vector store references
	if eventDelegate, ok := config.Events.(*clusterEventDelegate); ok {
		eventDelegate.cm = cm
		eventDelegate.vectorStore = vectorStore
	}

	// Join cluster if in ASG
	if ec2Info != nil && ec2Info.IsInASG {
		go cm.joinCluster(ctx)
	}

	return cm, nil
}

// joinCluster attempts to join the cluster by discovering peers
func (cm *ClusterManager) joinCluster(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Only try to join if we're not already in a cluster (only ourselves)
			if cm.memberlist.NumMembers() > 1 {
				continue
			}

			if err := cm.ec2Info.RefreshPeerIPs(ctx); err != nil {
				log.Printf("⚠️  Failed to refresh peer IPs: %v", err)
				continue
			}

			if len(cm.ec2Info.PeerIPs) == 0 {
				continue
			}

			// Try to join using seed peers
			seedPeers := cm.ec2Info.GetSeedPeers()
			if len(seedPeers) > 0 {
				_, err := cm.memberlist.Join(seedPeers)
				if err != nil {
					log.Printf("⚠️  Failed to join cluster: %v", err)
				} else {
					log.Printf("✅ Joined cluster with %d members", cm.memberlist.NumMembers())
				}
			}
		}
	}
}

// UpdateS3SyncTime updates the last S3 sync timestamp
func (cm *ClusterManager) UpdateS3SyncTime(syncTime time.Time) {
	cm.delegate.mu.Lock()
	cm.delegate.localState.LastS3Sync = syncTime
	cm.delegate.mu.Unlock()

	// Broadcast state update
	cm.broadcastState()
}

// UpdateVectorCount updates the total vector count across all indexes
func (cm *ClusterManager) UpdateVectorCount(store *VectorStore) {
	if cm == nil || store == nil {
		return
	}

	// Calculate total vector count
	totalCount := 0
	store.mu.RLock()
	for _, index := range store.indexes {
		index.mu.RLock()
		totalCount += len(index.vectors)
		index.mu.RUnlock()
	}
	store.mu.RUnlock()

	// Update local state
	cm.delegate.mu.Lock()
	cm.delegate.localState.VectorCount = totalCount
	cm.delegate.mu.Unlock()

	// Broadcast state update
	cm.broadcastState()
}

// ShouldSyncToS3 determines if this node should sync to S3
// Returns true if this node has the oldest sync time (or lowest ID if tied)
func (cm *ClusterManager) ShouldSyncToS3() bool {
	cm.delegate.mu.RLock()
	defer cm.delegate.mu.RUnlock()

	myState := cm.delegate.localState
	oldestSync := myState.LastS3Sync
	winnerId := myState.NodeID

	// Find peer with oldest sync time
	for _, peer := range cm.delegate.peerStates {
		if peer.LastS3Sync.Before(oldestSync) {
			oldestSync = peer.LastS3Sync
			winnerId = peer.NodeID
		} else if peer.LastS3Sync.Equal(oldestSync) {
			// Tie-breaker: lowest node ID
			if peer.NodeID < winnerId {
				winnerId = peer.NodeID
			}
		}
	}

	return winnerId == myState.NodeID
}

// broadcastState broadcasts local state to all peers
func (cm *ClusterManager) broadcastState() {
	cm.delegate.mu.Lock()
	// Update LastSeen for local state before broadcasting
	cm.delegate.localState.LastSeen = time.Now()
	stateJSON, err := json.Marshal(cm.delegate.localState)
	cm.delegate.mu.Unlock()

	if err != nil {
		log.Printf("⚠️  Failed to marshal state: %v", err)
		return
	}

	broadcast := &stateBroadcast{
		msg: stateJSON,
	}

	cm.delegate.broadcasts.QueueBroadcast(broadcast)
}

// GetClusterSize returns the number of nodes in the cluster
func (cm *ClusterManager) GetClusterSize() int {
	return cm.memberlist.NumMembers()
}

// GetNodeID returns the node ID
func (cm *ClusterManager) GetNodeID() string {
	return cm.nodeID
}

// GetPeerStates returns a copy of all peer states
func (cm *ClusterManager) GetPeerStates() map[string]*PeerState {
	cm.delegate.mu.RLock()
	defer cm.delegate.mu.RUnlock()

	states := make(map[string]*PeerState)
	for id, state := range cm.delegate.peerStates {
		stateCopy := *state
		states[id] = &stateCopy
	}

	// Include local state
	localCopy := *cm.delegate.localState
	states[cm.nodeID] = &localCopy

	return states
}

// Shutdown gracefully shuts down the cluster manager
func (cm *ClusterManager) Shutdown() error {
	return cm.memberlist.Shutdown()
}

// ReconcileAllIndexes reconciles all indexes with peers
func (cm *ClusterManager) ReconcileAllIndexes(ctx context.Context, store *VectorStore) {
	if cm == nil {
		return
	}

	peers := cm.GetPeerStates()
	delete(peers, cm.nodeID)

	if len(peers) == 0 {
		log.Printf("ℹ️  No peers available for reconciliation")
		return
	}

	log.Printf("🔄 Starting index reconciliation with cluster...")

	// Select first peer to reconcile with
	var peerIP string
	for _, peer := range peers {
		peerIP = peer.IP
		break
	}

	// First, fetch list of indexes from peer and create any missing ones locally
	if err := cm.syncMissingIndexes(ctx, peerIP, store); err != nil {
		log.Printf("⚠️  Failed to sync missing indexes: %v", err)
	}

	// Now reconcile vectors in all indexes
	store.mu.RLock()
	indexNames := make([]string, 0, len(store.indexes))
	for name := range store.indexes {
		indexNames = append(indexNames, name)
	}
	store.mu.RUnlock()

	for _, indexName := range indexNames {
		index, err := store.GetIndex(indexName)
		if err != nil {
			continue
		}

		if err := cm.ReconcileIndex(ctx, indexName, index); err != nil {
			log.Printf("⚠️  Failed to reconcile index %s: %v", indexName, err)
		}
	}

	log.Printf("✅ Index reconciliation complete")
}

// syncMissingIndexes fetches peer's index list and creates any missing indexes locally
func (cm *ClusterManager) syncMissingIndexes(ctx context.Context, peerIP string, store *VectorStore) error {
	url := fmt.Sprintf("http://%s:8080/internal/indexes", peerIP)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned status %d", resp.StatusCode)
	}

	var apiResp struct {
		Status string                   `json:"status"`
		Data   []map[string]interface{} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}

	if apiResp.Status != "success" {
		return fmt.Errorf("peer returned error response")
	}

	// Check each peer index and create if missing
	for _, peerIndex := range apiResp.Data {
		indexName, ok := peerIndex["name"].(string)
		if !ok {
			continue
		}

		// Check if we have this index
		if _, err := store.GetIndex(indexName); err == nil {
			// Index already exists, skip
			continue
		}

		// Get dimensions
		dimensions := 0
		if dim, ok := peerIndex["dimensions"].(float64); ok {
			dimensions = int(dim)
		}

		if dimensions <= 0 {
			log.Printf("⚠️  Invalid dimensions for index %s", indexName)
			continue
		}

		// Create missing index
		log.Printf("📥 Creating missing index: %s (%d dimensions)", indexName, dimensions)
		if err := store.CreateIndex(indexName, dimensions); err != nil {
			log.Printf("⚠️  Failed to create index %s: %v", indexName, err)
			continue
		}
		log.Printf("✅ Created index: %s", indexName)
	}

	return nil
}

// ReconcileIndex reconciles an index with a peer using block-based sync
func (cm *ClusterManager) ReconcileIndex(ctx context.Context, indexName string, localIndex *VectorIndex) error {
	peers := cm.GetPeerStates()

	// Remove ourselves
	delete(peers, cm.nodeID)

	if len(peers) == 0 {
		return nil // No peers to sync with
	}

	// Select first available peer
	var peerIP string
	for _, peer := range peers {
		peerIP = peer.IP
		break
	}

	// Get peer's manifest
	peerManifest, err := cm.fetchPeerManifest(ctx, peerIP, indexName)
	if err != nil {
		// If peer doesn't have this index (404), skip silently
		if strings.Contains(err.Error(), "status 404") {
			log.Printf("ℹ️  Peer doesn't have index %s, skipping", indexName)
			return nil
		}
		return fmt.Errorf("failed to fetch peer manifest: %w", err)
	}

	// Generate local manifest
	localManifest := localIndex.GenerateManifest()

	// Find mismatched blocks
	missingBlocks := []int{}
	for _, peerBlock := range peerManifest.Blocks {
		if peerBlock.BlockIndex >= len(localManifest.Blocks) {
			// We don't have this block at all
			missingBlocks = append(missingBlocks, peerBlock.BlockIndex)
		} else {
			localBlock := localManifest.Blocks[peerBlock.BlockIndex]
			if localBlock.Hash != peerBlock.Hash {
				// Block exists but hash doesn't match
				missingBlocks = append(missingBlocks, peerBlock.BlockIndex)
			}
		}
	}

	if len(missingBlocks) == 0 {
		log.Printf("✅ Index %s is in sync with peer", indexName)
		return nil
	}

	log.Printf("📥 Syncing %d blocks for index %s from peer %s", len(missingBlocks), indexName, peerIP)

	// Fetch and apply missing blocks
	for _, blockIdx := range missingBlocks {
		if err := cm.fetchAndApplyBlock(ctx, peerIP, indexName, blockIdx, localIndex); err != nil {
			log.Printf("⚠️  Failed to sync block %d: %v", blockIdx, err)
			continue
		}
	}

	log.Printf("✅ Synced %d blocks for index %s", len(missingBlocks), indexName)

	// Flush to disk after sync
	return localIndex.Flush()
}

// fetchPeerManifest fetches the manifest from a peer
func (cm *ClusterManager) fetchPeerManifest(ctx context.Context, peerIP, indexName string) (*IndexManifest, error) {
	url := fmt.Sprintf("http://%s:8080/internal/sync/%s/manifest", peerIP, indexName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer returned status %d", resp.StatusCode)
	}

	var apiResp struct {
		Status string         `json:"status"`
		Data   *IndexManifest `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}

	if apiResp.Status != "success" {
		return nil, fmt.Errorf("peer returned error response")
	}

	return apiResp.Data, nil
}

// fetchAndApplyBlock fetches a block from a peer and applies it to local index
func (cm *ClusterManager) fetchAndApplyBlock(ctx context.Context, peerIP, indexName string, blockIdx int, localIndex *VectorIndex) error {
	url := fmt.Sprintf("http://%s:8080/internal/sync/%s/block/%d", peerIP, indexName, blockIdx)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned status %d", resp.StatusCode)
	}

	var apiResp struct {
		Status string `json:"status"`
		Data   struct {
			BlockIndex int           `json:"block_index"`
			Vectors    []VectorEntry `json:"vectors"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return err
	}

	if apiResp.Status != "success" {
		return fmt.Errorf("peer returned error response")
	}

	// Apply vectors from block
	for _, entry := range apiResp.Data.Vectors {
		if err := localIndex.AddVector(entry.ID, entry.Vector, entry.Metadata); err != nil {
			log.Printf("⚠️  Failed to add vector %s: %v", entry.ID, err)
		}
	}

	return nil
}

// getLocalIP gets the local non-loopback IP address
func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no non-loopback IP found")
}

// Delegate implementation
func (d *gossipDelegate) NodeMeta(limit int) []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()

	meta, _ := json.Marshal(d.localState)
	if len(meta) > limit {
		return meta[:limit]
	}
	return meta
}

func (d *gossipDelegate) NotifyMsg(msg []byte) {
	var state PeerState
	if err := json.Unmarshal(msg, &state); err != nil {
		return
	}

	d.mu.Lock()
	now := time.Now()

	// Preserve FirstSeen if peer already exists, otherwise set to now
	if existingState, exists := d.peerStates[state.NodeID]; exists {
		state.FirstSeen = existingState.FirstSeen
	} else {
		state.FirstSeen = now
	}

	// Update LastSeen to current time (when we received this message)
	state.LastSeen = now

	d.peerStates[state.NodeID] = &state
	d.mu.Unlock()
}

func (d *gossipDelegate) GetBroadcasts(overhead, limit int) [][]byte {
	return d.broadcasts.GetBroadcasts(overhead, limit)
}

func (d *gossipDelegate) LocalState(join bool) []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()

	state, _ := json.Marshal(d.localState)
	return state
}

func (d *gossipDelegate) MergeRemoteState(buf []byte, join bool) {
	var state PeerState
	if err := json.Unmarshal(buf, &state); err != nil {
		return
	}

	d.mu.Lock()
	now := time.Now()

	// Preserve FirstSeen if peer already exists, otherwise set to now
	if existingState, exists := d.peerStates[state.NodeID]; exists {
		state.FirstSeen = existingState.FirstSeen
	} else {
		state.FirstSeen = now
	}

	// Update LastSeen to current time (when we received this state)
	state.LastSeen = now

	d.peerStates[state.NodeID] = &state
	d.mu.Unlock()
}

// stateBroadcast implements memberlist.Broadcast
type stateBroadcast struct {
	msg []byte
}

func (b *stateBroadcast) Invalidates(other memberlist.Broadcast) bool {
	return false
}

func (b *stateBroadcast) Message() []byte {
	return b.msg
}

func (b *stateBroadcast) Finished() {
}

// clusterEventDelegate handles memberlist events
type clusterEventDelegate struct {
	delegate    *gossipDelegate
	cm          *ClusterManager
	vectorStore *VectorStore
}

func (e *clusterEventDelegate) NotifyJoin(node *memberlist.Node) {
	log.Printf("🔗 Node joined: %s (%s)", node.Name, node.Addr)

	// Trigger reconciliation when a new node joins (after brief delay for stabilization)
	if e.cm != nil && e.vectorStore != nil {
		go func() {
			time.Sleep(2 * time.Second)
			log.Printf("🔄 Triggering reconciliation after node join: %s", node.Name)
			e.cm.ReconcileAllIndexes(context.Background(), e.vectorStore)
		}()
	}
}

func (e *clusterEventDelegate) NotifyLeave(node *memberlist.Node) {
	log.Printf("🔌 Node left: %s", node.Name)

	// Remove from peer states
	e.delegate.mu.Lock()
	delete(e.delegate.peerStates, node.Name)
	e.delegate.mu.Unlock()
}

func (e *clusterEventDelegate) NotifyUpdate(node *memberlist.Node) {
	// Node metadata updated
}
