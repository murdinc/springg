package springg

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// WAL implements Write-Ahead Logging for crash recovery
type WAL struct {
	file     *os.File
	writer   *bufio.Writer
	mu       sync.Mutex
	filePath string
}

// WALEntry represents a single log entry
type WALEntry struct {
	Operation string          `json:"op"` // "add", "update", "delete"
	ID        string          `json:"id"`
	Vector    []float32       `json:"vector,omitempty"`
	Metadata  *VectorMetadata `json:"metadata,omitempty"`
}

// NewWAL creates a new Write-Ahead Log
func NewWAL(indexName, dataPath string) (*WAL, error) {
	filePath := filepath.Join(dataPath, indexName+".wal")

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open WAL file: %w", err)
	}

	return &WAL{
		file:     file,
		writer:   bufio.NewWriter(file),
		filePath: filePath,
	}, nil
}

// LogAdd logs an add operation
func (w *WAL) LogAdd(id string, vector []float32, metadata *VectorMetadata) error {
	return w.log(WALEntry{
		Operation: "add",
		ID:        id,
		Vector:    vector,
		Metadata:  metadata,
	})
}

// LogUpdate logs an update operation
func (w *WAL) LogUpdate(id string, vector []float32, metadata *VectorMetadata) error {
	return w.log(WALEntry{
		Operation: "update",
		ID:        id,
		Vector:    vector,
		Metadata:  metadata,
	})
}

// LogDelete logs a delete operation
func (w *WAL) LogDelete(id string) error {
	return w.log(WALEntry{
		Operation: "delete",
		ID:        id,
	})
}

// log writes an entry to the WAL
func (w *WAL) log(entry WALEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	if _, err := w.writer.Write(append(data, '\n')); err != nil {
		return err
	}

	// Flush to disk for durability
	return w.writer.Flush()
}

// Replay replays the WAL to recover state
func ReplayWAL(indexName, dataPath string, index *VectorIndex) error {
	filePath := filepath.Join(dataPath, indexName+".wal")

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No WAL file, nothing to replay
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry WALEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue // Skip corrupted entries
		}

		switch entry.Operation {
		case "add", "update":
			index.AddVector(entry.ID, entry.Vector, entry.Metadata)
		case "delete":
			index.DeleteVector(entry.ID)
		}
	}

	return scanner.Err()
}

// Truncate clears the WAL (after successful checkpoint)
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.writer.Reset(w.file)
	w.file.Close()

	// Reopen with truncate
	file, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	w.file = file
	w.writer = bufio.NewWriter(file)
	return nil
}

// Close closes the WAL
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.writer.Flush(); err != nil {
		return err
	}
	return w.file.Close()
}
