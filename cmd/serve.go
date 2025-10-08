package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/murdinc/springg/internal/springg"
	"github.com/spf13/cobra"
)

var (
	port               int
	vectorStore        *springg.VectorStore
	config             *springg.Config
	s3Syncer           *springg.S3Syncer
	clusterManager     *springg.ClusterManager
	replicationManager *springg.ReplicationManager
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP server",
	Long:  "Start the Springg HTTP server to handle vector database operations.",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load configuration
		cfg, err := springg.LoadConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		config = cfg

		// Override port if flag is set
		if port > 0 {
			config.Port = port
		}

		// Initialize S3 syncer
		syncer, err := springg.NewS3Syncer(cfg)
		if err != nil {
			return fmt.Errorf("failed to initialize S3 syncer: %w", err)
		}
		s3Syncer = syncer

		// Download indexes from S3 if enabled
		if s3Syncer != nil {
			ctx := context.Background()
			if err := s3Syncer.DownloadAllIndexes(ctx); err != nil {
				log.Printf("⚠️  Failed to download indexes from S3: %v", err)
			}
		}

		// Create vector store (will load downloaded indexes)
		store, err := springg.NewVectorStore(config.DataPath, config.MaxMemoryMB)
		if err != nil {
			return fmt.Errorf("failed to create vector store: %w", err)
		}
		vectorStore = store
		log.Printf("Memory limit: %d MB", config.MaxMemoryMB)

		// Log loaded indexes
		indexes := vectorStore.ListIndexes()
		log.Printf("Loaded %d indexes from disk", len(indexes))
		for _, idx := range indexes {
			log.Printf("  - %s: %d vectors, %d dimensions",
				idx["name"], idx["count"], idx["dimensions"])
		}

		// Initialize cluster manager if enabled
		var cluster *springg.ClusterManager
		if config.ClusterEnabled {
			cluster, err = springg.NewClusterManager(context.Background(), vectorStore, config.ClusterBindPort)
			if err != nil {
				log.Printf("⚠️  Failed to initialize cluster manager: %v", err)
			} else {
				clusterManager = cluster
				log.Printf("🌐 Cluster manager initialized")
			}
		}

		// Initialize replication manager if clustering is enabled
		if cluster != nil {
			replicationManager = springg.NewReplicationManager(cluster, vectorStore)
			log.Printf("🔄 Replication manager initialized")

			// Reconcile indexes with peers after a brief delay (let cluster stabilize)
			go func() {
				time.Sleep(10 * time.Second)
				cluster.ReconcileAllIndexes(context.Background(), vectorStore)
			}()
		}

		// Create router
		r := chi.NewRouter()

		// Middleware
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(loggerMiddleware)
		r.Use(middleware.Recoverer)
		r.Use(middleware.Timeout(60 * time.Second)) // 60s request timeout
		r.Use(middleware.SetHeader("Content-Type", "application/json"))
		r.Use(rateLimitMiddleware(100)) // 100 req/sec per IP

		// Request size limit (100MB max for bulk uploads)
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024) // 100MB
				next.ServeHTTP(w, r)
			})
		})

		// CORS middleware
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   []string{"*"},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
			ExposedHeaders:   []string{"Link"},
			AllowCredentials: false,
			MaxAge:           300,
		}))

		// Routes
		r.Route("/api", func(r chi.Router) {
			// Health check (no auth required)
			r.Get("/health", handleHealth)

			// Apply JWT middleware if required
			if config.RequireJWT {
				r.Use(jwtMiddleware(config.JWTSecret))
			}

			// Index management
			r.Post("/indexes/{name}", handleCreateIndex)
			r.Delete("/indexes/{name}", handleDeleteIndex)
			r.Get("/indexes", handleListIndexes)
			r.Get("/indexes/{name}/stats", handleIndexStats)

			// Vector operations
			r.Post("/indexes/{name}/vectors", handleAddVector)
			r.Post("/indexes/{name}/vectors/bulk", handleBulkAddVectors)
			r.Get("/indexes/{name}/vectors/{id}", handleGetVector)
			r.Put("/indexes/{name}/vectors/{id}", handleUpdateVector)
			r.Delete("/indexes/{name}/vectors/{id}", handleDeleteVector)
			r.Post("/indexes/{name}/search", handleSearch)
		})

		// Internal endpoints (no auth, for cluster replication)
		r.Route("/internal", func(r chi.Router) {
			r.Post("/replicate", handleReplication)
			r.Get("/cluster/status", handleClusterStatus)

			// Block-based sync endpoints
			r.Get("/sync/{index}/manifest", handleGetManifest)
			r.Get("/sync/{index}/block/{blockIndex}", handleGetBlock)
		})

		// Create server
		srv := &http.Server{
			Addr:         fmt.Sprintf(":%d", config.Port),
			Handler:      r,
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 15 * time.Second,
			IdleTimeout:  60 * time.Second,
		}

		// Start periodic S3 sync with cluster coordination
		syncCtx, syncCancel := context.WithCancel(context.Background())
		if s3Syncer != nil {
			s3Syncer.StartPeriodicSync(syncCtx, vectorStore, cluster)
		}

		// Start server in goroutine
		go func() {
			log.Printf("Starting Springg server on port %d", config.Port)
			log.Printf("JWT authentication: %v", config.RequireJWT)
			if s3Syncer != nil {
				log.Printf("S3 sync enabled: bucket=%s region=%s", config.S3Bucket, config.S3Region)
			}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Server error: %v", err)
			}
		}()

		// Wait for interrupt signal
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit

		log.Println("Shutting down server...")

		// Stop S3 sync
		syncCancel()

		// Shutdown replication manager first
		if replicationManager != nil {
			log.Println("Shutting down replication manager...")
			replicationManager.Shutdown()
		}

		// Shutdown cluster manager
		if clusterManager != nil {
			log.Println("Shutting down cluster manager...")
			if err := clusterManager.Shutdown(); err != nil {
				log.Printf("⚠️  Failed to shutdown cluster: %v", err)
			}
		}

		// Shutdown vector store (flushes all indexes, closes WALs, stops workers)
		log.Println("Shutting down vector store...")
		if err := vectorStore.Shutdown(); err != nil {
			log.Printf("⚠️  Failed to shutdown vector store: %v", err)
		}

		// Final sync to S3
		if s3Syncer != nil {
			log.Println("Final S3 sync...")
			ctx := context.Background()
			if err := s3Syncer.UploadAllIndexes(ctx, vectorStore); err != nil {
				log.Printf("⚠️  Failed to sync to S3: %v", err)
			}
		}

		// Graceful shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("server forced to shutdown: %w", err)
		}

		log.Println("Server exited")
		return nil
	},
}

func init() {
	serveCmd.Flags().IntVar(&port, "port", 0, "Port to listen on (overrides config file)")
}

// loggerMiddleware logs HTTP requests
func loggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, ww.Status(), time.Since(start))
	})
}

// jwtMiddleware validates JWT tokens
func jwtMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				respondError(w, "Authorization header required", http.StatusUnauthorized)
				return
			}

			// Extract token
			parts := strings.Split(authHeader, " ")
			if len(parts) != 2 || parts[0] != "Bearer" {
				respondError(w, "Invalid authorization header format", http.StatusUnauthorized)
				return
			}

			tokenString := parts[1]

			// Parse and validate token
			token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
				// Verify signing method
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
				}
				return []byte(secret), nil
			})

			if err != nil {
				respondError(w, "Invalid or expired token", http.StatusUnauthorized)
				return
			}

			if !token.Valid {
				respondError(w, "Invalid token", http.StatusUnauthorized)
				return
			}

			// Verify issuer
			if claims, ok := token.Claims.(jwt.MapClaims); ok {
				if iss, ok := claims["iss"].(string); !ok || iss != "springg" {
					respondError(w, "Invalid token issuer", http.StatusUnauthorized)
					return
				}

				// Add claims to context for use in handlers
				ctx := context.WithValue(r.Context(), "claims", claims)
				next.ServeHTTP(w, r.WithContext(ctx))
			} else {
				respondError(w, "Invalid token claims", http.StatusUnauthorized)
				return
			}
		})
	}
}

// API Response helpers
type apiResponse struct {
	Status string      `json:"status"`
	Data   interface{} `json:"data,omitempty"`
	Error  string      `json:"error,omitempty"`
}

func respondSuccess(w http.ResponseWriter, data interface{}) {
	respond(w, http.StatusOK, apiResponse{
		Status: "success",
		Data:   data,
	})
}

func respondError(w http.ResponseWriter, message string, code int) {
	respond(w, code, apiResponse{
		Status: "error",
		Error:  message,
	})
}

func respond(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// Health check handler
func handleHealth(w http.ResponseWriter, r *http.Request) {
	respondSuccess(w, map[string]string{
		"status":  "healthy",
		"service": "springg",
	})
}

// Create index handler
func handleCreateIndex(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req struct {
		Dimensions int `json:"dimensions"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Dimensions <= 0 {
		respondError(w, "Dimensions must be positive", http.StatusBadRequest)
		return
	}

	if err := vectorStore.CreateIndex(name, req.Dimensions); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			respondError(w, err.Error(), http.StatusConflict)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	respondSuccess(w, map[string]interface{}{
		"name":       name,
		"dimensions": req.Dimensions,
		"created":    true,
	})
}

// Delete index handler
func handleDeleteIndex(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	if err := vectorStore.DeleteIndex(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	respondSuccess(w, map[string]interface{}{
		"name":    name,
		"deleted": true,
	})
}

// List indexes handler
func handleListIndexes(w http.ResponseWriter, r *http.Request) {
	indexes := vectorStore.ListIndexes()
	respondSuccess(w, indexes)
}

// Index stats handler
func handleIndexStats(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	stats := index.Stats()

	// Add memory usage for this index
	memoryStats := vectorStore.GetMemoryStats()
	if perIndexStats, ok := memoryStats["per_index_mb"].(map[string]uint64); ok {
		if indexMemory, exists := perIndexStats[name]; exists {
			stats["memory_mb"] = indexMemory
		}
	}

	respondSuccess(w, stats)
}

// Add vector handler
func handleAddVector(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req struct {
		ID       string                  `json:"id"`
		Vector   []float32               `json:"vector"`
		Metadata *springg.VectorMetadata `json:"metadata,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		respondError(w, "ID is required", http.StatusBadRequest)
		return
	}

	if len(req.Vector) == 0 {
		respondError(w, "Vector is required", http.StatusBadRequest)
		return
	}

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if err := index.AddVector(req.ID, req.Vector, req.Metadata); err != nil {
		if strings.Contains(err.Error(), "dimension mismatch") {
			respondError(w, err.Error(), http.StatusBadRequest)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Replicate to peers if clustering enabled
	if replicationManager != nil {
		replicationManager.ReplicateAdd(context.Background(), name, req.ID, req.Vector, req.Metadata)
	}

	respondSuccess(w, map[string]interface{}{
		"id":    req.ID,
		"added": true,
	})
}

// Update vector handler
func handleUpdateVector(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	id := chi.URLParam(r, "id")

	var req struct {
		Vector   []float32               `json:"vector"`
		Metadata *springg.VectorMetadata `json:"metadata,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Vector) == 0 {
		respondError(w, "Vector is required", http.StatusBadRequest)
		return
	}

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if err := index.UpdateVector(id, req.Vector, req.Metadata); err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else if strings.Contains(err.Error(), "dimension mismatch") {
			respondError(w, err.Error(), http.StatusBadRequest)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Replicate to peers if clustering enabled
	if replicationManager != nil {
		replicationManager.ReplicateUpdate(context.Background(), name, id, req.Vector, req.Metadata)
	}

	respondSuccess(w, map[string]interface{}{
		"id":      id,
		"updated": true,
	})
}

// Delete vector handler
func handleDeleteVector(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	id := chi.URLParam(r, "id")

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if err := index.DeleteVector(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Replicate to peers if clustering enabled
	if replicationManager != nil {
		replicationManager.ReplicateDelete(context.Background(), name, id)
	}

	respondSuccess(w, map[string]interface{}{
		"id":      id,
		"deleted": true,
	})
}

// Search handler
func handleSearch(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req struct {
		Vector []float32 `json:"vector"`
		K      int       `json:"k"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Vector) == 0 {
		respondError(w, "Vector is required", http.StatusBadRequest)
		return
	}

	if req.K <= 0 {
		req.K = 10 // Default to 10 results
	}

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	results, err := index.Search(req.Vector, req.K)
	if err != nil {
		if strings.Contains(err.Error(), "dimension mismatch") {
			respondError(w, err.Error(), http.StatusBadRequest)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	respondSuccess(w, results)
}

// Get vector handler
func handleGetVector(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	id := chi.URLParam(r, "id")

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	entry, err := index.GetVector(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	respondSuccess(w, entry)
}

// Bulk add vectors handler
func handleBulkAddVectors(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req struct {
		Vectors []springg.VectorEntry `json:"vectors"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Vectors) == 0 {
		respondError(w, "At least one vector is required", http.StatusBadRequest)
		return
	}

	index, err := vectorStore.GetIndex(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Process bulk additions
	successCount := 0
	errors := make([]map[string]interface{}, 0)

	for i, entry := range req.Vectors {
		if entry.ID == "" {
			errors = append(errors, map[string]interface{}{
				"index": i,
				"error": "ID is required",
			})
			continue
		}

		if len(entry.Vector) == 0 {
			errors = append(errors, map[string]interface{}{
				"index": i,
				"id":    entry.ID,
				"error": "Vector is required",
			})
			continue
		}

		if err := index.AddVector(entry.ID, entry.Vector, entry.Metadata); err != nil {
			errors = append(errors, map[string]interface{}{
				"index": i,
				"id":    entry.ID,
				"error": err.Error(),
			})
			continue
		}

		successCount++
	}

	// Force flush after bulk operation
	if err := index.Flush(); err != nil {
		log.Printf("Failed to flush index after bulk add: %v", err)
	}

	result := map[string]interface{}{
		"total":   len(req.Vectors),
		"success": successCount,
		"failed":  len(errors),
	}

	if len(errors) > 0 {
		result["errors"] = errors
	}

	respondSuccess(w, result)
}

// Internal replication handler (supports both single and batch messages)
func handleReplication(w http.ResponseWriter, r *http.Request) {
	if replicationManager == nil {
		respondError(w, "Replication not enabled", http.StatusServiceUnavailable)
		return
	}

	// Check if this is a batch request
	batchSize := r.Header.Get("X-Batch-Size")

	if batchSize != "" {
		// Handle batch
		var batch springg.ReplicationBatch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			respondError(w, "Invalid batch request body", http.StatusBadRequest)
			return
		}

		errors := []string{}
		for _, msg := range batch.Messages {
			if err := replicationManager.HandleReplicationMessage(msg); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", msg.VectorID, err))
			}
		}

		if len(errors) > 0 {
			log.Printf("⚠️  Batch replication had %d errors", len(errors))
			respondSuccess(w, map[string]interface{}{
				"replicated": len(batch.Messages) - len(errors),
				"errors":     errors,
			})
		} else {
			respondSuccess(w, map[string]interface{}{
				"replicated": len(batch.Messages),
			})
		}
	} else {
		// Handle single message (backward compatibility)
		var msg springg.ReplicationMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			respondError(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if err := replicationManager.HandleReplicationMessage(msg); err != nil {
			log.Printf("⚠️  Replication error: %v", err)
			respondError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		respondSuccess(w, map[string]bool{"replicated": true})
	}
}

// Cluster status handler
func handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	if clusterManager == nil {
		respondError(w, "Clustering not enabled", http.StatusServiceUnavailable)
		return
	}

	peerStates := clusterManager.GetPeerStates()

	respondSuccess(w, map[string]interface{}{
		"cluster_size": clusterManager.GetClusterSize(),
		"peers":        peerStates,
		"memory":       vectorStore.GetMemoryStats(),
	})
}

// Get index manifest handler
func handleGetManifest(w http.ResponseWriter, r *http.Request) {
	indexName := chi.URLParam(r, "index")

	index, err := vectorStore.GetIndex(indexName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	manifest := index.GenerateManifest()
	respondSuccess(w, manifest)
}

// Get block vectors handler
func handleGetBlock(w http.ResponseWriter, r *http.Request) {
	indexName := chi.URLParam(r, "index")
	blockIndexStr := chi.URLParam(r, "blockIndex")

	// Parse block index
	var blockIndex int
	if _, err := fmt.Sscanf(blockIndexStr, "%d", &blockIndex); err != nil {
		respondError(w, "Invalid block index", http.StatusBadRequest)
		return
	}

	index, err := vectorStore.GetIndex(indexName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			respondError(w, err.Error(), http.StatusNotFound)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	vectors, err := index.GetBlockVectors(blockIndex)
	if err != nil {
		respondError(w, err.Error(), http.StatusBadRequest)
		return
	}

	respondSuccess(w, map[string]interface{}{
		"block_index": blockIndex,
		"vectors":     vectors,
	})
}

// rateLimitMiddleware implements simple rate limiting per IP
func rateLimitMiddleware(requestsPerSecond int) func(http.Handler) http.Handler {
	type client struct {
		lastSeen time.Time
		tokens   float64
	}

	clients := make(map[string]*client)
	var mu sync.Mutex

	// Cleanup old clients every minute
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			for ip, c := range clients {
				if now.Sub(c.lastSeen) > 5*time.Minute {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.RemoteAddr

			mu.Lock()
			c, exists := clients[ip]
			if !exists {
				c = &client{
					lastSeen: time.Now(),
					tokens:   float64(requestsPerSecond),
				}
				clients[ip] = c
			}

			// Token bucket algorithm
			now := time.Now()
			elapsed := now.Sub(c.lastSeen).Seconds()
			c.tokens += elapsed * float64(requestsPerSecond)
			if c.tokens > float64(requestsPerSecond) {
				c.tokens = float64(requestsPerSecond)
			}
			c.lastSeen = now

			if c.tokens >= 1 {
				c.tokens -= 1
				mu.Unlock()
				next.ServeHTTP(w, r)
			} else {
				mu.Unlock()
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			}
		})
	}
}
