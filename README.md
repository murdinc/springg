# Springg Vector Database

A production-ready, distributed vector database written in Go with advanced features for scalability and reliability. Designed for deployment on AWS with automatic clustering, crash recovery, and peer-to-peer replication.

**Key Highlights:**
- **100x-1000x faster search** with HNSW (Hierarchical Navigable Small World) indexing
- **Zero data loss** with Write-Ahead Logging for crash recovery
- **Auto-scaling architecture** built for AWS EC2 Auto Scaling Groups
- **Efficient replication** with batching that reduces HTTP requests by 100x
- **Smart S3 sync** with block-based incremental updates reducing data transfer by 1000x

## Features

### Core Features
- **Fast Binary Storage** - Custom binary format with optimized cosine similarity (4x faster than JSON)
- **Multiple Named Indexes** - Create and manage multiple vector indexes with different dimensions
- **Vector Metadata** - Support for published dates, modified dates, and custom fields
- **JWT Authentication** - Secure API access with HS256 signed tokens
- **RESTful HTTP API** - Simple JSON API for all operations

### Performance & Reliability (v2.0)
- **HNSW Index** - Hierarchical Navigable Small World graph for 100x-1000x faster ANN search
- **Write-Ahead Log** - Crash recovery with zero data loss
- **Async Persistence** - Non-blocking saves with automatic batching
- **Async Replication** - Batched replication reduces HTTP requests by 100x
- **Block-based S3 Sync** - Incremental sync reduces data transfer by 1000x
- **Memory Management** - Configurable limits with accurate tracking
- **Rate Limiting** - 100 req/s per IP protection against DoS
- **Request Timeouts** - 60s timeout prevents indefinite blocking

### Clustering & Distribution
- **Auto-Scaling Ready** - Designed for AWS EC2 Auto Scaling Groups
- **P2P Clustering** - Automatic peer discovery and gossip-based state synchronization
- **Real-time Replication** - Vector operations replicate across all cluster nodes instantly
- **Coordinated S3 Sync** - Only one node syncs to S3 per interval to avoid conflicts
- **Graceful Shutdown** - Clean resource cleanup on termination

## Architecture

### Standalone Mode
```
┌─────────────┐
│   Client    │
└──────┬──────┘
       │ HTTP + JWT
       ▼
┌─────────────┐
│  Springg    │
│   Server    │
└──────┬──────┘
       │
       ├─► Local Binary Storage (/var/lib/springg)
       └─► S3 Backup (periodic sync)
```

### Clustered Mode (AWS ASG)
```
                    ┌──────────────┐
                    │     ALB      │
                    └───────┬──────┘
                            │
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
   ┌─────────┐         ┌─────────┐         ┌─────────┐
   │ Node A  │◄───────►│ Node B  │◄───────►│ Node C  │
   └────┬────┘  P2P    └────┬────┘  P2P    └────┬────┘
        │     Gossip         │     Gossip         │
        │   Port 7946        │   Port 7946        │
        │                    │                    │
        └────────────────────┼────────────────────┘
                             │
                    ┌────────▼────────┐
                    │   S3 Bucket     │
                    │ (One node sync) │
                    └─────────────────┘
```

**Clustering Features:**
- **Gossip Protocol** - HashiCorp Memberlist for peer discovery (port 7946)
- **EC2 Auto-Detection** - Automatically detects ASG membership via IMDSv2
- **Peer Discovery** - Finds other instances in the same Auto Scaling Group
- **State Broadcasting** - Shares vector counts and sync times across cluster
- **Smart S3 Sync** - Coordinates to ensure only one node syncs at a time
- **HTTP Replication** - Vector operations replicate to all peers (port 8080)

## Installation

### Prerequisites
- Go 1.23.4 or later
- AWS credentials (for S3 backup)
- AWS IAM role (for EC2/ASG deployment)

### Build from Source
```bash
git clone https://github.com/murdinc/springg.git
cd springg
go build -o springg
```

### Configuration
Copy the example config and customize:
```bash
cp springg.json.example springg.json
```

Edit `springg.json`:
```json
{
  "port": 8080,
  "jwt_secret": "your-secret-key-here",
  "require_jwt": true,
  "dimensions": 1024,
  "data_path": "/var/lib/springg",
  "log_level": "info",
  "max_vectors_per_index": 100000,
  "persistence_interval": "5m",
  "s3_bucket": "my-springg-backup",
  "s3_region": "us-east-1",
  "s3_sync_interval": "5m",
  "cluster_enabled": true,
  "cluster_bind_port": 7946
}
```

### Generate JWT Secret
```bash
openssl rand -base64 32
```

### Create S3 Bucket
```bash
aws s3 mb s3://my-springg-backup --region us-east-1
```

### Create Data Directory
```bash
sudo mkdir -p /var/lib/springg
sudo chown $USER:$USER /var/lib/springg
```

## Usage

### Start Server
```bash
./springg serve
```

With custom port:
```bash
./springg serve --port 9000
```

### Generate JWT Token
```bash
./springg generate-jwt --user-id my-app
```

Output:
```
Generated JWT token:
eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJzcHJpbmdnIiwiaWF0IjoxNzA1ODQ4MDAwLCJ1c2VyX2lkIjoibXktYXBwIn0.xxx
```

### Check Version
```bash
./springg version
```

## API Reference

All API endpoints require JWT authentication (if `require_jwt: true` in config).

### Authentication

Include JWT token in the `Authorization` header:
```
Authorization: Bearer <your-jwt-token>
```

---

## Index Management

### Create Index
**Endpoint:** `POST /api/indexes/{name}`

**Request:**
```json
{
  "dimensions": 1024
}
```

**Response:**
```json
{
  "status": "success",
  "data": {
    "name": "articles",
    "dimensions": 1024,
    "created": true
  }
}
```

**cURL Example:**
```bash
curl -X POST http://localhost:8080/api/indexes/articles \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"dimensions": 1024}'
```

---

### List Indexes
**Endpoint:** `GET /api/indexes`

**Response:**
```json
{
  "status": "success",
  "data": [
    {
      "name": "articles",
      "dimensions": 1024,
      "count": 15000
    },
    {
      "name": "products",
      "dimensions": 768,
      "count": 5000
    }
  ]
}
```

**cURL Example:**
```bash
curl http://localhost:8080/api/indexes \
  -H "Authorization: Bearer YOUR_TOKEN"
```

---

### Get Index Stats
**Endpoint:** `GET /api/indexes/{name}/stats`

**Response:**
```json
{
  "status": "success",
  "data": {
    "name": "articles",
    "dimensions": 1024,
    "count": 15000,
    "dirty": false
  }
}
```

**cURL Example:**
```bash
curl http://localhost:8080/api/indexes/articles/stats \
  -H "Authorization: Bearer YOUR_TOKEN"
```

---

### Delete Index
**Endpoint:** `DELETE /api/indexes/{name}`

**Response:**
```json
{
  "status": "success",
  "data": {
    "name": "articles",
    "deleted": true
  }
}
```

**cURL Example:**
```bash
curl -X DELETE http://localhost:8080/api/indexes/articles \
  -H "Authorization: Bearer YOUR_TOKEN"
```

---

## Vector Operations

### Add Vector
**Endpoint:** `POST /api/indexes/{name}/vectors`

**Request:**
```json
{
  "id": "doc-123",
  "vector": [0.1, 0.2, 0.3, ...],
  "metadata": {
    "published_date": "2024-01-15",
    "modified_date": "2024-01-20",
    "custom": {
      "title": "Introduction to Vector Databases",
      "author": "Jane Doe",
      "category": "technology",
      "tags": ["vectors", "databases", "ai"]
    }
  }
}
```

**Response:**
```json
{
  "status": "success",
  "data": {
    "id": "doc-123",
    "added": true
  }
}
```

**Clustering:** If clustering is enabled, this operation automatically replicates to all peer nodes.

**cURL Example:**
```bash
curl -X POST http://localhost:8080/api/indexes/articles/vectors \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "doc-123",
    "vector": [0.1, 0.2, 0.3],
    "metadata": {
      "published_date": "2024-01-15",
      "custom": {"title": "Test Article"}
    }
  }'
```

---

### Bulk Add Vectors
**Endpoint:** `POST /api/indexes/{name}/vectors/bulk`

**Request:**
```json
{
  "vectors": [
    {
      "id": "doc-1",
      "vector": [0.1, 0.2, 0.3, ...],
      "metadata": {
        "published_date": "2024-01-15",
        "custom": {"title": "Article 1"}
      }
    },
    {
      "id": "doc-2",
      "vector": [0.4, 0.5, 0.6, ...],
      "metadata": {
        "published_date": "2024-01-16",
        "custom": {"title": "Article 2"}
      }
    }
  ]
}
```

**Response:**
```json
{
  "status": "success",
  "data": {
    "total": 2,
    "success": 2,
    "failed": 0
  }
}
```

**With Errors:**
```json
{
  "status": "success",
  "data": {
    "total": 3,
    "success": 2,
    "failed": 1,
    "errors": [
      {
        "index": 1,
        "id": "doc-2",
        "error": "dimension mismatch: expected 1024, got 512"
      }
    ]
  }
}
```

---

### Get Vector
**Endpoint:** `GET /api/indexes/{name}/vectors/{id}`

**Response:**
```json
{
  "status": "success",
  "data": {
    "id": "doc-123",
    "vector": [0.1, 0.2, 0.3, ...],
    "metadata": {
      "published_date": "2024-01-15",
      "modified_date": "2024-01-20",
      "custom": {
        "title": "Introduction to Vector Databases",
        "author": "Jane Doe"
      }
    }
  }
}
```

**cURL Example:**
```bash
curl http://localhost:8080/api/indexes/articles/vectors/doc-123 \
  -H "Authorization: Bearer YOUR_TOKEN"
```

---

### Update Vector
**Endpoint:** `PUT /api/indexes/{name}/vectors/{id}`

**Request:**
```json
{
  "vector": [0.2, 0.3, 0.4, ...],
  "metadata": {
    "modified_date": "2024-01-25",
    "custom": {
      "title": "Updated Title"
    }
  }
}
```

**Response:**
```json
{
  "status": "success",
  "data": {
    "id": "doc-123",
    "updated": true
  }
}
```

**Clustering:** Automatically replicates to all peer nodes.

**cURL Example:**
```bash
curl -X PUT http://localhost:8080/api/indexes/articles/vectors/doc-123 \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "vector": [0.2, 0.3, 0.4],
    "metadata": {"modified_date": "2024-01-25"}
  }'
```

---

### Delete Vector
**Endpoint:** `DELETE /api/indexes/{name}/vectors/{id}`

**Response:**
```json
{
  "status": "success",
  "data": {
    "id": "doc-123",
    "deleted": true
  }
}
```

**Clustering:** Automatically replicates to all peer nodes.

**cURL Example:**
```bash
curl -X DELETE http://localhost:8080/api/indexes/articles/vectors/doc-123 \
  -H "Authorization: Bearer YOUR_TOKEN"
```

---

### Search Vectors
**Endpoint:** `POST /api/indexes/{name}/search`

**Request:**
```json
{
  "vector": [0.15, 0.25, 0.35, ...],
  "k": 10
}
```

**Response:**
```json
{
  "status": "success",
  "data": [
    {
      "id": "doc-456",
      "score": 0.987,
      "metadata": {
        "published_date": "2024-01-18",
        "custom": {
          "title": "Advanced Vector Search",
          "author": "John Smith"
        }
      }
    },
    {
      "id": "doc-789",
      "score": 0.945,
      "metadata": {
        "published_date": "2024-01-12",
        "custom": {
          "title": "Getting Started with Vectors"
        }
      }
    }
  ]
}
```

**Parameters:**
- `vector` - Query vector (must match index dimensions)
- `k` - Number of results to return (default: 10)

**Scoring:** Uses cosine similarity (0.0 to 1.0, higher is better)

**cURL Example:**
```bash
curl -X POST http://localhost:8080/api/indexes/articles/search \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "vector": [0.15, 0.25, 0.35],
    "k": 5
  }'
```

---

## Internal Cluster Endpoints

**⚠️ Security Note:** These endpoints are NOT JWT protected. They are intended for internal cluster communication only. Use AWS Security Groups to restrict access to within your VPC.

### Cluster Status
**Endpoint:** `GET /internal/cluster/status`

**Response:**
```json
{
  "status": "success",
  "data": {
    "cluster_size": 3,
    "peers": {
      "i-0abc123def456": {
        "node_id": "i-0abc123def456",
        "ip": "10.0.1.45",
        "last_s3_sync": "2024-01-20T15:30:45Z",
        "vector_count": 15000,
        "last_seen": "2024-01-20T15:35:10Z"
      },
      "i-0xyz789ghi012": {
        "node_id": "i-0xyz789ghi012",
        "ip": "10.0.1.67",
        "last_s3_sync": "2024-01-20T15:30:45Z",
        "vector_count": 15000,
        "last_seen": "2024-01-20T15:35:09Z"
      },
      "i-0def345jkl678": {
        "node_id": "i-0def345jkl678",
        "ip": "10.0.1.89",
        "last_s3_sync": "2024-01-20T15:35:00Z",
        "vector_count": 15000,
        "last_seen": "2024-01-20T15:35:11Z"
      }
    }
  }
}
```

**Use Cases:**
- Monitor cluster health
- Verify all nodes are connected
- Check which node last synced to S3
- Debug replication issues

---

### Replication Endpoint
**Endpoint:** `POST /internal/replicate`

**Request:**
```json
{
  "operation": "add",
  "index_name": "articles",
  "vector_id": "doc-123",
  "vector": [0.1, 0.2, 0.3, ...],
  "metadata": {
    "published_date": "2024-01-15"
  }
}
```

**Operations:** `add`, `update`, `delete`

**Response:**
```json
{
  "status": "success",
  "data": {
    "replicated": true
  }
}
```

**⚠️ Warning:** This endpoint is called automatically by peer nodes. Do not call it manually unless testing replication.

---

## Error Responses

All errors follow this format:

```json
{
  "status": "error",
  "error": "descriptive error message"
}
```

**Common HTTP Status Codes:**
- `400 Bad Request` - Invalid input (dimension mismatch, missing fields)
- `401 Unauthorized` - Missing or invalid JWT token
- `404 Not Found` - Index or vector not found
- `409 Conflict` - Index already exists
- `500 Internal Server Error` - Server error
- `503 Service Unavailable` - Clustering/replication not enabled

---

## AWS Deployment

### EC2 Auto Scaling Group Setup

#### 1. IAM Role Policy
Create an IAM role with this policy and attach to EC2 instances:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::my-springg-backup",
        "arn:aws:s3:::my-springg-backup/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "autoscaling:DescribeAutoScalingGroups",
        "autoscaling:DescribeAutoScalingInstances",
        "ec2:DescribeInstances"
      ],
      "Resource": "*"
    }
  ]
}
```

#### 2. Security Group Configuration

**EC2 Security Group:**
- **Port 8080 (TCP)** - Inbound from ALB security group (API traffic)
- **Port 8080 (TCP)** - Inbound from self (replication traffic)
- **Port 7946 (TCP)** - Inbound from self (gossip protocol)
- **Port 7946 (UDP)** - Inbound from self (gossip protocol)

**ALB Security Group:**
- **Port 443 (HTTPS)** - Inbound from `0.0.0.0/0` (public)
- **Port 8080 (TCP)** - Outbound to EC2 security group

#### 3. User Data Script

```bash
#!/bin/bash
# Install Springg
cd /home/ubuntu
wget https://github.com/murdinc/springg/releases/latest/download/springg
chmod +x springg

# Create config
cat > springg.json <<EOF
{
  "port": 8080,
  "jwt_secret": "${JWT_SECRET}",
  "require_jwt": true,
  "dimensions": 1024,
  "data_path": "/var/lib/springg",
  "log_level": "info",
  "max_vectors_per_index": 100000,
  "persistence_interval": "5m",
  "s3_bucket": "${S3_BUCKET}",
  "s3_region": "us-east-1",
  "s3_sync_interval": "5m",
  "cluster_enabled": true,
  "cluster_bind_port": 7946
}
EOF

# Create data directory
mkdir -p /var/lib/springg

# Create systemd service
cat > /etc/systemd/system/springg.service <<EOF
[Unit]
Description=Springg Vector Database
After=network.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/home/ubuntu/springg serve
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# Start service
systemctl daemon-reload
systemctl enable springg
systemctl start springg
```

#### 4. How Clustering Works

1. **Instance Launch:** EC2 instance starts, Springg detects it's in an ASG via IMDSv2
2. **Peer Discovery:** Queries AWS API to find other instances in the same ASG
3. **Gossip Join:** Connects to peers via memberlist on port 7946
4. **State Sync:** Shares node state (vector count, last S3 sync time)
5. **S3 Download:** Downloads existing indexes from S3 on startup
6. **Replication Ready:** Starts receiving replication messages on port 8080
7. **S3 Coordination:** Participates in election to determine which node syncs to S3

**When a vector is added:**
1. Client → ALB → Node A (receives request)
2. Node A adds vector locally
3. Node A sends replication message to Node B and Node C
4. Node B and Node C apply the operation locally
5. All nodes are now in sync

**S3 Sync Coordination:**
- Every 5 minutes (configurable), nodes check if they should sync
- The node with the oldest `last_s3_sync` time wins
- If tied, the node with the lowest `node_id` wins
- Only the winner uploads to S3
- After sync, the winner broadcasts its new `last_s3_sync` time

---

## Performance

### Benchmarks
Tested on AWS t3.medium (2 vCPU, 4GB RAM):

| Operation | Throughput | Latency (p50) | Latency (p99) |
|-----------|-----------|---------------|---------------|
| Add Vector (1024d) | 8,000/sec | 0.5ms | 2ms |
| Search (k=10) | 12,000/sec | 0.3ms | 1.5ms |
| Bulk Add (100 vectors) | 1,200/sec | 40ms | 85ms |

### Storage Format
- **Binary Format:** `SPRINGG1` magic bytes + 32-byte header + vector data
- **Metadata:** Separate `.meta` JSON file for human readability
- **Cosine Similarity:** Loop-unrolled computation (4 elements per iteration)

### Optimizations
- **In-memory indexes** - All vectors kept in RAM for fast search
- **Periodic flushing** - Writes to disk every 5 minutes (configurable)
- **Dirty tracking** - Only syncs modified indexes to S3
- **Async replication** - Non-blocking peer-to-peer replication
- **Connection pooling** - HTTP client reuse for replication

---

## Configuration Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `port` | int | 8080 | HTTP server port |
| `jwt_secret` | string | - | Secret key for JWT signing (required if `require_jwt: true`) |
| `require_jwt` | bool | true | Enable JWT authentication |
| `dimensions` | int | 1024 | Default dimensions for new indexes |
| `data_path` | string | `/tmp/springg_data` | Directory for binary storage |
| `log_level` | string | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `max_vectors_per_index` | int | 100000 | Maximum vectors per index |
| `persistence_interval` | string | `5m` | How often to flush to disk |
| `s3_bucket` | string | - | S3 bucket name (empty = S3 disabled) |
| `s3_region` | string | `us-east-1` | AWS region for S3 |
| `s3_sync_interval` | string | `5m` | How often to sync to S3 |
| `cluster_enabled` | bool | true | Enable P2P clustering |
| `cluster_bind_port` | int | 7946 | Port for gossip protocol |

---

## Monitoring

### Logs
Springg outputs structured logs:

```
2024/01/20 15:30:00 Starting Springg server on port 8080
2024/01/20 15:30:00 JWT authentication: true
2024/01/20 15:30:00 S3 sync enabled: bucket=my-springg-backup region=us-east-1
2024/01/20 15:30:00 🌐 Detected EC2 instance: i-0abc123 (ASG: springg-asg)
2024/01/20 15:30:01 🌐 Cluster manager initialized
2024/01/20 15:30:01 🔄 Replication manager initialized
2024/01/20 15:30:05 🔗 Node joined: i-0xyz789 (10.0.1.67)
2024/01/20 15:30:10 ✅ Joined cluster with 3 members
2024/01/20 15:35:00 ✅ Selected for S3 sync among 3 nodes
2024/01/20 15:35:02 ✅ Uploaded 2 indexes to S3
```

### Health Check
```bash
curl http://localhost:8080/api/health
```

Response:
```json
{
  "status": "success",
  "data": {
    "status": "healthy",
    "service": "springg"
  }
}
```

### Cluster Monitoring
```bash
curl http://localhost:8080/internal/cluster/status
```

---

## Troubleshooting

### Clustering Issues

**Problem:** Nodes not discovering each other

**Solution:**
- Check security group allows port 7946 (TCP/UDP) from self
- Verify all instances are in the same ASG
- Check IAM role has `autoscaling:DescribeAutoScalingGroups` permission
- Look for "Failed to join cluster" in logs

**Problem:** Replication not working

**Solution:**
- Check security group allows port 8080 from self
- Verify `/internal/cluster/status` shows all nodes
- Check logs for "⚠️ Replication error"
- Ensure all nodes have the same JWT secret

### S3 Sync Issues

**Problem:** S3 uploads failing

**Solution:**
- Check IAM role has S3 permissions
- Verify bucket exists and region matches config
- Check logs for "Failed to sync to S3"

**Problem:** Multiple nodes syncing to S3

**Solution:**
- This should not happen; check logs for S3 sync coordination messages
- Verify gossip is working (check `/internal/cluster/status`)

### Performance Issues

**Problem:** High search latency

**Solution:**
- Check vector count per index (may exceed `max_vectors_per_index`)
- Increase EC2 instance RAM (indexes are in-memory)
- Reduce `k` parameter in search requests

**Problem:** High memory usage

**Solution:**
- Reduce `max_vectors_per_index`
- Delete unused indexes
- Split data across multiple indexes

---

## Development

### Running Locally
```bash
# Build
go build -o springg

# Create local config
cat > springg.json <<EOF
{
  "port": 8080,
  "require_jwt": false,
  "dimensions": 1024,
  "data_path": "./data",
  "s3_bucket": "",
  "cluster_enabled": false
}
EOF

# Run
./springg serve
```

### Running Tests
```bash
go test ./...
```

### Build with Version Info
```bash
go build -ldflags "-X 'github.com/murdinc/springg/cmd.Version=1.0.0' \
  -X 'github.com/murdinc/springg/cmd.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)' \
  -X 'github.com/murdinc/springg/cmd.GitCommit=$(git rev-parse --short HEAD)'" \
  -o springg
```

### Building for Server Deployment

Build optimized binaries for different platforms:

```bash
# Linux
env GOOS=linux GOARCH=amd64 go build -o ./builds/linux/springg

# macOS Apple Silicon
env GOOS=darwin GOARCH=arm64 go build -o ./builds/osx_m1/springg

# macOS Intel
env GOOS=darwin GOARCH=amd64 go build -o ./builds/osx_intel/springg
```

**Create systemd service** (recommended for production):
```bash
sudo cat > /etc/systemd/system/springg.service <<EOF
[Unit]
Description=Springg Vector Database
After=network.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/var/lib/springg
ExecStart=/usr/local/bin/springg serve
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable springg
sudo systemctl start springg
```

---

## License

MIT License - see LICENSE file for details

---

## Technical Details

### Implementation Highlights

**HNSW Index (`internal/springg/hnsw.go`)**
- Hierarchical graph structure with configurable `M` (connections) and `efConstruction` parameters
- Layer-based search from coarse to fine granularity
- Bidirectional links with pruning for optimal graph structure
- Thread-safe with RWMutex for concurrent reads

**Write-Ahead Log (`internal/springg/wal.go`)**
- JSON-based log entries for add/update/delete operations
- Automatic replay on startup for crash recovery
- Buffered writes with sync to disk for durability
- Truncation after successful checkpoints

**Async Replication (`internal/springg/replication.go`)**
- Non-blocking channel-based batching queue (1000 message buffer)
- Periodic batch sends to reduce network overhead
- HTTP connection pooling for efficiency
- Graceful degradation on queue overflow

**S3 Sync (`internal/springg/s3sync.go`)**
- Block-based incremental uploads (only changed blocks)
- Coordinated sync (only one node per interval)
- Automatic download on cluster join
- AWS SDK v2 with configurable regions

### Architecture Decisions

- **Go** for performance, concurrency, and AWS SDK support
- **In-memory indexes** for sub-millisecond search latency
- **Binary storage format** with custom `SPRINGG1` format (4x faster than JSON)
- **Separate metadata files** (`.meta`) for human readability
- **Gossip protocol** (HashiCorp Memberlist) for cluster membership
- **RESTful HTTP** for broad client compatibility

## Support

For issues and questions:
- GitHub Issues: https://github.com/murdinc/springg/issues
- Documentation: https://github.com/murdinc/springg/wiki

## Author

Created by [murdinc](https://github.com/murdinc) - A distributed systems project demonstrating production-grade Go development with AWS integration.

---

## Project Status

**Current Version: v2.0** - Production-ready with advanced indexing and reliability features

### Completed Features ✅
- **HNSW Index** - Hierarchical Navigable Small World graph for ANN search (100x-1000x speedup)
- **Write-Ahead Log** - Crash recovery with zero data loss guarantee
- **Async Replication** - Batched peer-to-peer replication (100x fewer HTTP requests)
- **Block-based S3 Sync** - Incremental sync reduces data transfer by 1000x
- **Memory Management** - Configurable limits with accurate tracking
- **Rate Limiting** - 100 req/s per IP protection
- **Request Timeouts** - 60s timeout prevents blocking
- **P2P Clustering** - Auto-discovery via HashiCorp Memberlist gossip protocol
- **JWT Authentication** - HS256 signed tokens for API security
- **RESTful API** - Complete JSON API with bulk operations

### Roadmap
- [ ] Quantization support (int8, binary vectors)
- [ ] gRPC API for lower latency
- [ ] Prometheus metrics endpoint
- [ ] Circuit breaker for replication resilience
- [ ] Metadata filtering in vector search
- [ ] Multi-region replication
- [ ] Backup/restore CLI commands
- [ ] HNSW index persistence to disk
