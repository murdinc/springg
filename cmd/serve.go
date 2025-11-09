package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/murdinc/springg/internal/springg"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/spf13/cobra"
)

var (
	port               int
	vectorStore        *springg.VectorStore
	config             *springg.Config
	s3Syncer           *springg.S3Syncer
	clusterManager     *springg.ClusterManager
	replicationManager *springg.ReplicationManager
	requestMetrics     *RequestMetrics
	cpuMonitor         *CPUMonitor
	bulkRequestSem     chan struct{} // Semaphore for limiting concurrent bulk requests
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

		// Initialize request metrics
		requestMetrics = NewRequestMetrics()
		requestMetrics.Start()

		// Initialize CPU monitor
		cpuMonitor = NewCPUMonitor()
		cpuMonitor.Start()

		// Initialize bulk request semaphore
		if config.MaxConcurrentBulkRequests > 0 {
			bulkRequestSem = make(chan struct{}, config.MaxConcurrentBulkRequests)
			log.Printf("🚦 Bulk request concurrency limit: %d", config.MaxConcurrentBulkRequests)
		}

		// Log loaded indexes
		indexes := vectorStore.ListIndexes()
		log.Printf("Loaded %d indexes from disk", len(indexes))
		for _, idx := range indexes {
			log.Printf("  - %s: %d vectors, %d dimensions",
				idx["name"], idx["count"], idx["dimensions"])
		}

		// Initialize cluster manager if enabled
		var cluster *springg.ClusterManager
		var nodeID string
		if config.ClusterEnabled {
			cluster, err = springg.NewClusterManager(context.Background(), vectorStore, config.ClusterBindPort)
			if err != nil {
				log.Printf("⚠️  Failed to initialize cluster manager: %v", err)
			} else {
				clusterManager = cluster
				nodeID = cluster.GetNodeID()
				log.Printf("🌐 Cluster manager initialized")

				// Update vector count in gossip state for loaded indexes
				cluster.UpdateVectorCount(vectorStore)
				// Write initial S3 heartbeat to confirm connectivity
				if s3Syncer != nil {
					if err := s3Syncer.WriteHeartbeat(context.Background(), nodeID); err != nil {
						log.Printf("⚠️  Failed to write S3 heartbeat: %v", err)
					}
				}
			}
		}

		// Initialize replication manager if clustering is enabled
		if cluster != nil {
			replicationManager = springg.NewReplicationManager(cluster, vectorStore)
			log.Printf("🔄 Replication manager initialized")

			// Initial reconciliation attempt if peers are already available
			go func() {
				time.Sleep(10 * time.Second)
				if cluster.GetClusterSize() > 1 {
					log.Printf("🔄 Initial reconciliation with %d peers", cluster.GetClusterSize()-1)
					cluster.ReconcileAllIndexes(context.Background(), vectorStore)
				} else {
					log.Printf("ℹ️  No peers at startup (reconciliation will auto-trigger when nodes join)")
				}
			}()
		}

		// Create router
		r := chi.NewRouter()

		// Middleware
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(loggerMiddleware)
		r.Use(requestCounterMiddleware) // Track request metrics
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

			// Protected routes group with JWT middleware
			r.Group(func(r chi.Router) {
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
		})

		// Internal endpoints (no auth, for cluster replication)
		r.Route("/internal", func(r chi.Router) {
			r.Post("/replicate", handleReplication)
			r.Get("/cluster/status", handleClusterStatus)

			// Index list for reconciliation (no auth)
			r.Get("/indexes", handleListIndexes)

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

// RequestMetrics tracks request rates using a sliding window
type RequestMetrics struct {
	buckets    [60]uint64 // One bucket per second for last 60 seconds
	currentSec int64      // Current second index
	mu         sync.RWMutex
}

func NewRequestMetrics() *RequestMetrics {
	return &RequestMetrics{}
}

func (rm *RequestMetrics) Start() {
	// Background goroutine to rotate buckets every second
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			rm.mu.Lock()
			now := time.Now().Unix()
			rm.currentSec = now
			// Clear the bucket for this second (wraps around every 60 seconds)
			rm.buckets[now%60] = 0
			rm.mu.Unlock()
		}
	}()
}

func (rm *RequestMetrics) Increment() {
	rm.mu.Lock()
	now := time.Now().Unix()
	rm.buckets[now%60]++
	rm.mu.Unlock()
}

func (rm *RequestMetrics) GetQPS() map[string]float64 {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	// Sum all 60 buckets for requests per minute
	var totalLastMinute uint64
	for _, count := range rm.buckets {
		totalLastMinute += count
	}

	// Calculate QPS (average over last 60 seconds)
	qps := float64(totalLastMinute) / 60.0

	return map[string]float64{
		"last_minute": qps,
		"total":       float64(totalLastMinute),
	}
}

// CPUMonitor tracks CPU usage percentage using gopsutil
type CPUMonitor struct {
	currentCPU float64
	mu         sync.RWMutex
}

func NewCPUMonitor() *CPUMonitor {
	return &CPUMonitor{}
}

func (cm *CPUMonitor) Start() {
	// Update CPU stats every 2 seconds
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cm.update()
		}
	}()
}

func (cm *CPUMonitor) update() {
	// Get CPU usage percentage averaged over 1 second
	// percpu=false means total CPU usage across all cores
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil {
		log.Printf("⚠️  Failed to get CPU usage: %v", err)
		return
	}

	if len(percentages) > 0 {
		cm.mu.Lock()
		cm.currentCPU = percentages[0]
		cm.mu.Unlock()
	}
}

func (cm *CPUMonitor) GetCPUPercent() float64 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.currentCPU
}

// requestCounterMiddleware increments the request counter
func requestCounterMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestMetrics != nil {
			requestMetrics.Increment()
		}
		next.ServeHTTP(w, r)
	})
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

	// Replicate index creation to peers if clustering enabled
	if replicationManager != nil {
		replicationManager.ReplicateCreateIndex(context.Background(), name, req.Dimensions)
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

	// Delete from S3 if enabled
	if s3Syncer != nil {
		ctx := context.Background()
		if err := s3Syncer.DeleteIndex(ctx, name); err != nil {
			log.Printf("⚠️  Failed to delete index from S3: %v", err)
		}
	}

	// Replicate index deletion to peers if clustering enabled
	if replicationManager != nil {
		replicationManager.ReplicateDeleteIndex(context.Background(), name)
	}

	// Update vector count in cluster state
	if clusterManager != nil {
		clusterManager.UpdateVectorCount(vectorStore)
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

	// Update vector count in cluster state
	if clusterManager != nil {
		clusterManager.UpdateVectorCount(vectorStore)
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

	// Update vector count in cluster state
	if clusterManager != nil {
		clusterManager.UpdateVectorCount(vectorStore)
	}

	respondSuccess(w, map[string]interface{}{
		"id":      id,
		"deleted": true,
	})
}

// RecencyBoost defines parameters for date-based score boosting
type RecencyBoost struct {
	Field            string  `json:"field"`              // "published_date" or "modified_date"
	RecentDays       int     `json:"recent_days"`        // Tier 1 threshold (e.g., 90 days)
	RecentMultiplier float64 `json:"recent_multiplier"`  // Boost for tier 1 (e.g., 1.5)
	MediumDays       int     `json:"medium_days"`        // Tier 2 threshold (e.g., 365 days)
	MediumMultiplier float64 `json:"medium_multiplier"`  // Boost for tier 2 (e.g., 1.2)
}

// applyRecencyBoost applies date-based score boosting and re-sorts results
func applyRecencyBoost(results []springg.SearchResult, boost *RecencyBoost) {
	now := time.Now()
	recentDuration := time.Duration(boost.RecentDays) * 24 * time.Hour
	mediumDuration := time.Duration(boost.MediumDays) * 24 * time.Hour

	// Apply multipliers to each result
	for i := range results {
		multiplier := 1.0

		// Extract date from metadata
		if results[i].Metadata != nil {
			var dateStr string
			if boost.Field == "published_date" {
				if val, ok := results[i].Metadata["published_date"].(string); ok {
					dateStr = val
				}
			} else if boost.Field == "modified_date" {
				if val, ok := results[i].Metadata["modified_date"].(string); ok {
					dateStr = val
				}
			}

			// Parse date and calculate age
			if dateStr != "" {
				// Try multiple date formats
				var docDate time.Time
				var err error

				// Try RFC3339 first (ISO 8601)
				docDate, err = time.Parse(time.RFC3339, dateStr)
				if err != nil {
					// Try common MySQL datetime format
					docDate, err = time.Parse("2006-01-02 15:04:05", dateStr)
				}
				if err != nil {
					// Try date-only format
					docDate, err = time.Parse("2006-01-02", dateStr)
				}

				if err == nil {
					age := now.Sub(docDate)

					// Apply two-tier multiplier
					if age < recentDuration {
						multiplier = boost.RecentMultiplier
					} else if age < mediumDuration {
						multiplier = boost.MediumMultiplier
					}
				}
			}
		}

		// Apply multiplier to score
		results[i].Score = results[i].Score * multiplier
	}

	// Re-sort by boosted scores (highest first)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
}

// Search handler
func handleSearch(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req struct {
		Vector       []float32      `json:"vector"`
		K            int            `json:"k"`
		RecencyBoost *RecencyBoost  `json:"recency_boost,omitempty"`
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

	// If recency boost is enabled, fetch more results to allow proper re-ranking
	searchK := req.K
	if req.RecencyBoost != nil {
		// Fetch 10x more results to ensure recent articles have a chance to be boosted into top-K
		searchK = req.K * 10
		if searchK > 1000 {
			searchK = 1000 // Cap at 1000 to avoid performance issues
		}
	}

	results, err := index.Search(req.Vector, searchK)
	if err != nil {
		if strings.Contains(err.Error(), "dimension mismatch") {
			respondError(w, err.Error(), http.StatusBadRequest)
		} else {
			respondError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Apply recency boosting if requested
	if req.RecencyBoost != nil {
		applyRecencyBoost(results, req.RecencyBoost)
		// Limit to requested K after boosting and re-sorting
		if len(results) > req.K {
			results = results[:req.K]
		}
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
	// CPU backpressure: reject request if CPU is too high
	if config.BulkCPUThresholdPercent > 0 && cpuMonitor != nil {
		currentCPU := cpuMonitor.GetCPUPercent()
		if currentCPU > config.BulkCPUThresholdPercent {
			w.Header().Set("Retry-After", "5")
			respondError(w, fmt.Sprintf("Server overloaded (CPU: %.1f%%), please retry", currentCPU), http.StatusTooManyRequests)
			return
		}
	}

	// Semaphore: limit concurrent bulk requests
	if bulkRequestSem != nil {
		select {
		case bulkRequestSem <- struct{}{}:
			defer func() { <-bulkRequestSem }()
		default:
			w.Header().Set("Retry-After", "2")
			respondError(w, "Too many concurrent bulk requests, please retry", http.StatusTooManyRequests)
			return
		}
	}

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

		// Replicate successful adds to peers if clustering enabled
		if replicationManager != nil {
			replicationManager.ReplicateAdd(context.Background(), name, entry.ID, entry.Vector, entry.Metadata)
		}

		successCount++
	}

	// Force flush after bulk operation
	if err := index.Flush(); err != nil {
		log.Printf("Failed to flush index after bulk add: %v", err)
	}

	// Update vector count in cluster state
	if clusterManager != nil && successCount > 0 {
		clusterManager.UpdateVectorCount(vectorStore)
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

		// Update vector count after replication
		if clusterManager != nil && len(batch.Messages) > len(errors) {
			clusterManager.UpdateVectorCount(vectorStore)
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

		// Update vector count after replication
		if clusterManager != nil {
			clusterManager.UpdateVectorCount(vectorStore)
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

	// Update the responding node's last_seen to current time since it's actively responding
	nodeID := clusterManager.GetNodeID()
	if localState, exists := peerStates[nodeID]; exists {
		localState.LastSeen = time.Now()
	}

	status := map[string]interface{}{
		"responding_node": nodeID,
		"cluster_size":    clusterManager.GetClusterSize(),
		"peers":           peerStates,
		"memory":          vectorStore.GetMemoryStats(),
		"qps":             requestMetrics.GetQPS(),
	}

	// Pretty print the JSON response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(apiResponse{
		Status: "success",
		Data:   status,
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
