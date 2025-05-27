package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/cenkalti/backoff/v4"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const (
	defaultChromaURL = "http://localhost:8000"
	defaultOllamaURL = "http://localhost:11434/api/generate"
	defaultDataPath  = "_data"
	maxHistorySize   = 1000
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Time    string `json:"time"`
}

type ChatHistory struct {
	Messages []Message
	mu       sync.Mutex
}

type ChromaDocument struct {
	IDs       []string                 `json:"ids"`
	Documents []string                 `json:"documents"`
	Metadatas []map[string]interface{} `json:"metadatas"`
}

type TenantContext struct {
	TenantID string
}

type ProgressStatus struct {
	FilesDiscovered      int      `json:"files_discovered"`
	FilesProcessed       int      `json:"files_processed"`
	DocumentsProcessed   int      `json:"documents_processed"`
	BatchesCompleted     int      `json:"batches_completed"`
	BatchesAttempted     int      `json:"batches_attempted"`
	QueriesProcessed     int      `json:"queries_processed"`
	CollectionsCreated   int      `json:"collections_created"`
	CollectionsAttempted int      `json:"collections_attempted"`
	CollectionsChecked   int      `json:"collections_checked"`
	CollectionConflicts  int      `json:"collection_conflicts"`
	Errors               []string `json:"errors"`
	ElapsedTime          string   `json:"elapsed_time"`
}

type ProgressReporter func(status ProgressStatus)

var (
	history         ChatHistory
	codeRegex       = regexp.MustCompile(`\b(center|align|style|layout|div|css|html|js|javascript)\b`)
	codeKeywords    = []string{"example", "how to", "implement", "code", "snippet", "center", "style", "layout", "div", "align", "position", "css", "html", "javascript"}
	summaryKeywords = []string{"what is", "explain", "overview", "describe", "definition"}
	upgrader        = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	formatter *html.Formatter
)

type Client struct {
	httpClient      *http.Client
	baseURL         string
	databaseName    string
	ingestedDocs    map[string]bool
	ingestedMu      sync.RWMutex
	progress        ProgressStatus
	progressMu      sync.Mutex
	progressCb      ProgressReporter
	startTime       time.Time
	collectionUUIDs map[string]string
	collectionMu    sync.RWMutex
}

func NewClient(baseURL, databaseName string, progressCb ProgressReporter) *Client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     30 * time.Second,
		MaxConnsPerHost:     10,
		MaxIdleConnsPerHost: 10,
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL:         baseURL,
		databaseName:    databaseName,
		ingestedDocs:    make(map[string]bool),
		progressCb:      progressCb,
		startTime:       time.Now(),
		collectionUUIDs: make(map[string]string),
	}
}

func (c *Client) updateProgress(update func(*ProgressStatus)) {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	update(&c.progress)
	c.progress.ElapsedTime = time.Since(c.startTime).Round(time.Millisecond).String()
	if c.progressCb != nil {
		c.progressCb(c.progress)
	}
	slog.Info("Progress update", "status", c.progress)
}

func (c *Client) handleError(err error, context string) error {
	slog.Error("Error occurred", "context", context, "error", err)
	c.updateProgress(func(p *ProgressStatus) {
		p.Errors = append(p.Errors, fmt.Sprintf("%s: %v", context, err))
	})
	return err
}

func generateUUIDv4() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func (c *Client) cacheCollectionUUID(collectionName, uuid string) {
	c.collectionMu.Lock()
	c.collectionUUIDs[collectionName] = uuid
	c.collectionMu.Unlock()
}
func (c *Client) EnsureCollectionExists(ctx context.Context, tenantID, collectionName string) error {
	slog.Info("Ensuring collection exists", "tenant_id", tenantID, "database_name", c.databaseName, "collection_name", collectionName)
	c.updateProgress(func(p *ProgressStatus) { p.CollectionsAttempted++ })

	// List all collections to find the UUID
	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections", c.baseURL, tenantID, c.databaseName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to create GET request: %w", err), "EnsureCollectionExists")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.handleError(fmt.Errorf("GET request failed: %w", err), "EnsureCollectionExists")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to read response body: %w", err), "EnsureCollectionExists")
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("Unexpected response from list collections", "status", resp.StatusCode, "response", string(body))
		return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "EnsureCollectionExists")
	}

	var collections []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &collections); err != nil {
		slog.Error("Failed to parse collections response", "response", string(body), "error", err)
		return c.handleError(fmt.Errorf("failed to decode response: %w", err), "EnsureCollectionExists")
	}

	for _, collection := range collections {
		if collection.Name == collectionName {
			if collection.ID == "" {
				slog.Error("Collection response missing ID", "collection_name", collectionName, "response", string(body))
				return c.handleError(fmt.Errorf("collection response missing ID for %s", collectionName), "EnsureCollectionExists")
			}
			c.cacheCollectionUUID(collectionName, collection.ID)
			slog.Info("Found existing collection", "collection_uuid", collection.ID, "collection_name", collectionName)
			c.updateProgress(func(p *ProgressStatus) { p.CollectionsChecked++ })
			return nil
		}
	}

	// Collection not found, create it
	return c.createCollection(ctx, tenantID, collectionName)
}
func (c *Client) createCollection(ctx context.Context, tenantID, collectionName string) error {
	slog.Info("Creating collection", "tenant_id", tenantID, "database_name", c.databaseName, "collection_name", collectionName)
	c.updateProgress(func(p *ProgressStatus) { p.CollectionsAttempted++ })

	payload := map[string]interface{}{
		"name":          collectionName,
		"get_or_create": true, // Use get_or_create to avoid conflicts
		"metadata": map[string]interface{}{
			"name":       collectionName,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to marshal payload: %w", err), "createCollection")
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections", c.baseURL, tenantID, c.databaseName)
	operation := func() error {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create POST request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("POST request failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			var result struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(respBody, &result); err != nil {
				slog.Error("Failed to parse create collection response", "response", string(respBody), "error", err)
				return fmt.Errorf("failed to decode response: %w", err)
			}
			if result.ID == "" {
				slog.Error("Create collection response missing ID", "response", string(respBody))
				return fmt.Errorf("create collection response missing ID")
			}
			c.cacheCollectionUUID(collectionName, result.ID)
			c.updateProgress(func(p *ProgressStatus) { p.CollectionsCreated++ })
			slog.Info("Successfully created collection", "collection_uuid", result.ID, "collection_name", collectionName)
			return nil
		}

		slog.Error("Unexpected response from create collection", "status", resp.StatusCode, "response", string(respBody))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	backoffPolicy := backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3)
	if err := backoff.Retry(operation, backoff.WithContext(backoffPolicy, ctx)); err != nil {
		return c.handleError(fmt.Errorf("failed to create collection: %w", err), "createCollection")
	}
	return nil
}
func (c *Client) EnsureTenantExists(ctx context.Context, tenantID string) error {
	slog.Info("Ensuring tenant exists", "tenant_id", tenantID)
	url := fmt.Sprintf("%s/api/v2/tenants/%s", c.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to create GET request: %w", err), "EnsureTenantExists")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.handleError(fmt.Errorf("GET request failed: %w", err), "EnsureTenantExists")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		slog.Info("Tenant exists", "tenant_id", tenantID)
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		slog.Info("Tenant not found, attempting to create", "tenant_id", tenantID)
		payload := map[string]interface{}{
			"name": tenantID,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return c.handleError(fmt.Errorf("failed to marshal payload: %w", err), "EnsureTenantExists")
		}

		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v2/tenants", c.baseURL), bytes.NewReader(body))
		if err != nil {
			return c.handleError(fmt.Errorf("failed to create POST request: %w", err), "EnsureTenantExists")
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return c.handleError(fmt.Errorf("POST request failed: %w", err), "EnsureTenantExists")
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			slog.Info("Successfully created tenant", "tenant_id", tenantID)
			return nil
		}

		body, _ = io.ReadAll(resp.Body)

		return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "EnsureTenantExists")
	}

	body, _ := io.ReadAll(resp.Body)
	return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "EnsureTenantExists")
}

func (c *Client) EnsureDatabaseExists(ctx context.Context, tenantID, databaseName string) error {
	slog.Info("Ensuring database exists", "tenant_id", tenantID, "database_name", databaseName)
	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s", c.baseURL, tenantID, databaseName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to create GET request: %w", err), "EnsureDatabaseExists")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.handleError(fmt.Errorf("GET request failed: %w", err), "EnsureDatabaseExists")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		slog.Info("Database exists", "database_name", databaseName, "tenant_id", tenantID)
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		slog.Info("Database not found, attempting to create", "database_name", databaseName)
		payload := map[string]interface{}{
			"name": databaseName,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return c.handleError(fmt.Errorf("failed to marshal payload: %w", err), "EnsureDatabaseExists")
		}

		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v2/tenants/%s/databases", c.baseURL, tenantID), bytes.NewReader(body))
		if err != nil {
			return c.handleError(fmt.Errorf("failed to create POST request: %w", err), "EnsureDatabaseExists")
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return c.handleError(fmt.Errorf("POST request failed: %w", err), "EnsureDatabaseExists")
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
			slog.Info("Successfully created database", "database_name", databaseName, "tenant_id", tenantID)
			return nil
		}

		body, _ = io.ReadAll(resp.Body)
		return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "EnsureDatabaseExists")
	}

	body, _ := io.ReadAll(resp.Body)
	return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "EnsureDatabaseExists")
}

func (c *Client) QueryOllama(ctx context.Context, prompt string) (string, error) {
	ollamaURL := getEnv("OLLAMA_URL", defaultOllamaURL)
	slog.Info("Querying Ollama", "url", ollamaURL, "prompt", prompt)
	c.updateProgress(func(p *ProgressStatus) { p.QueriesProcessed++ })

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":  "smollm2:1.7b",
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return "", c.handleError(fmt.Errorf("failed to marshal request: %w", err), "QueryOllama")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", ollamaURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", c.handleError(fmt.Errorf("failed to create POST request: %w", err), "QueryOllama")
	}
	req.Header.Set("Content-Type", "application/json")

	startTime := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", c.handleError(fmt.Errorf("POST request failed: %w", err), "QueryOllama")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "QueryOllama")
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", c.handleError(fmt.Errorf("failed to parse response: %w", err), "QueryOllama")
	}

	slog.Info("Ollama query successful", "elapsed_time", time.Since(startTime))
	return result.Response, nil
}
func (c *Client) AddDocuments(ctx context.Context, tenantID, collectionName string, ids []string, documents []string, metadatas []map[string]interface{}) error {
	c.collectionMu.RLock()
	collectionUUID, exists := c.collectionUUIDs[collectionName]
	c.collectionMu.RUnlock()

	if !exists {
		if err := c.EnsureCollectionExists(ctx, tenantID, collectionName); err != nil {
			return c.handleError(fmt.Errorf("failed to ensure collection exists: %w", err), "AddDocuments")
		}
		c.collectionMu.RLock()
		collectionUUID, exists = c.collectionUUIDs[collectionName]
		c.collectionMu.RUnlock()
		if !exists {
			return c.handleError(fmt.Errorf("collection UUID not found for %s after ensuring collection", collectionName), "AddDocuments")
		}
	}

	payload := map[string]interface{}{
		"ids":       ids,
		"documents": documents,
		"metadatas": metadatas,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return c.handleError(fmt.Errorf("failed to marshal payload: %w", err), "AddDocuments")
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections/%s/add", c.baseURL, tenantID, c.databaseName, collectionUUID)
	slog.Info("Adding documents", "url", url, "payload_size_bytes", buf.Len(), "document_count", len(documents))

	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to create POST request: %w", err), "AddDocuments")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to execute request: %w", err), "AddDocuments")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to read response body: %w", err), "AddDocuments")
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return c.handleError(fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body)), "AddDocuments")
	}

	slog.Info("Documents added successfully", "document_count", len(documents))
	return nil
}
func (c *Client) IngestData(tenantID, dataPath, collectionName string) error {
	c.startTime = time.Now()
	slog.Info("Starting data ingestion", "tenant_id", tenantID, "data_path", dataPath, "collection_name", collectionName)
	c.updateProgress(func(p *ProgressStatus) { p.ElapsedTime = time.Since(c.startTime).String() })

	fileCount, err := estimateFileCount(dataPath)
	if err != nil {
		return c.handleError(fmt.Errorf("failed to estimate file count: %w", err), "IngestData")
	}
	documents := make([]string, 0, fileCount*10)
	metadatas := make([]map[string]interface{}, 0, fileCount*10)
	ids := make([]string, 0, fileCount*10)

	type docResult struct {
		documents []string
		metadatas []map[string]interface{}
		ids       []string
		err       error
	}
	docChan := make(chan docResult, fileCount)
	var wg sync.WaitGroup

	const maxWorkers = 2 // Reduced for testing
	workerChan := make(chan struct{}, maxWorkers)
	for i := 0; i < maxWorkers; i++ {
		workerChan <- struct{}{}
	}

	err = filepath.Walk(dataPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		c.updateProgress(func(p *ProgressStatus) { p.FilesDiscovered++ })
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-workerChan
			defer func() { workerChan <- struct{}{} }()

			slog.Info("Processing file", "path", path)
			file, err := os.Open(path)
			if err != nil {
				c.updateProgress(func(p *ProgressStatus) {
					p.Errors = append(p.Errors, fmt.Sprintf("Failed to open %s: %v", path, err))
				})
				docChan <- docResult{err: err}
				return
			}
			defer file.Close()

			var jsonData map[string]interface{}
			if err := json.NewDecoder(file).Decode(&jsonData); err != nil {
				c.updateProgress(func(p *ProgressStatus) {
					p.Errors = append(p.Errors, fmt.Sprintf("Failed to parse %s: %v", path, err))
				})
				docChan <- docResult{err: err}
				return
			}

			var localDocs []string
			var localMetas []map[string]interface{}
			var localIDs []string
			filename := info.Name()

			for key, value := range jsonData {
				id := fmt.Sprintf("%s:%s:0", filename, key)
				c.ingestedMu.RLock()
				if c.ingestedDocs[id] {
					c.ingestedMu.RUnlock()
					continue
				}
				c.ingestedMu.RUnlock()

				c.ingestedMu.Lock()
				c.ingestedDocs[id] = true
				c.ingestedMu.Unlock()

				content, _ := json.Marshal(map[string]interface{}{key: value})
				metadata := map[string]interface{}{
					"id":          id,
					"source":      filename,
					"current_key": key,
				}

				localDocs = append(localDocs, string(content))
				localMetas = append(localMetas, metadata)
				localIDs = append(localIDs, id)
			}

			c.updateProgress(func(p *ProgressStatus) {
				p.FilesProcessed++
				p.DocumentsProcessed += len(localDocs)
			})
			slog.Info("File processed", "path", path, "doc_count", len(localDocs))
			docChan <- docResult{documents: localDocs, metadatas: localMetas, ids: localIDs}
		}()
		return nil
	})

	if err != nil {
		return c.handleError(fmt.Errorf("failed to walk data directory: %w", err), "IngestData")
	}

	go func() {
		wg.Wait()
		close(docChan)
	}()

	for result := range docChan {
		if result.err != nil {
			continue
		}
		documents = append(documents, result.documents...)
		metadatas = append(metadatas, result.metadatas...)
		ids = append(ids, result.ids...)
	}

	if len(documents) == 0 {
		slog.Info("No new documents to ingest")
		return nil
	}

	const maxBatchSize = 5         // Reduced for testing
	const maxConcurrentBatches = 1 // Reduced for testing
	batchChan := make(chan struct{}, maxConcurrentBatches)
	for i := 0; i < maxConcurrentBatches; i++ {
		batchChan <- struct{}{}
	}

	var batchWg sync.WaitGroup
	for i := 0; i < len(documents); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(documents) {
			end = len(documents)
		}

		batchIDs := ids[i:end]
		batchDocs := documents[i:end]
		batchMetas := metadatas[i:end]

		batchWg.Add(1)
		go func(start, end int) {
			defer batchWg.Done()
			<-batchChan
			defer func() { batchChan <- struct{}{} }()

			slog.Info("Processing batch", "start", start, "end", end, "doc_count", len(batchDocs))
			operation := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				return c.AddDocuments(ctx, tenantID, collectionName, batchIDs, batchDocs, batchMetas)
			}

			backoffPolicy := backoff.NewExponentialBackOff()
			backoffPolicy.MaxElapsedTime = 2 * time.Minute
			backoffPolicy.InitialInterval = 500 * time.Millisecond
			backoffPolicy.RandomizationFactor = 0.5

			c.updateProgress(func(p *ProgressStatus) { p.BatchesAttempted++ })
			if err := backoff.Retry(operation, backoffPolicy); err != nil {
				c.updateProgress(func(p *ProgressStatus) {
					p.Errors = append(p.Errors, fmt.Sprintf("Failed batch %d-%d: %v", start, end, err))
				})
				slog.Error("Batch failed", "start", start, "end", end, "error", err)
				return
			}

			c.updateProgress(func(p *ProgressStatus) {
				p.BatchesCompleted++
				p.ElapsedTime = time.Since(c.startTime).String()
			})
			slog.Info("Successfully ingested batch", "start", start, "end", end)
		}(i, end)
	}

	batchWg.Wait()

	slog.Info("Data ingestion complete", "elapsed_time", time.Since(c.startTime))
	c.updateProgress(func(p *ProgressStatus) { p.ElapsedTime = time.Since(c.startTime).String() })

	c.ingestedMu.Lock()
	c.ingestedDocs = make(map[string]bool)
	c.ingestedMu.Unlock()

	return nil
}

type EmbeddingServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
}

func NewEmbeddingServer(modelPath string) (*EmbeddingServer, error) {

	// TODO: FIXA DEN HÄR SKIT PATHEN
	relPath := "./llama.cpp/build/bin/llama-embedding"

	absPath, err := filepath.Abs(relPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}
	fmt.Println("Using executable absolute path:", absPath)

	cmd := exec.Command(absPath,
		"-m", modelPath,
		"--embedding",
		"--interactive",
		"--n_predict", "0",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &EmbeddingServer{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
	}, nil
}

func (s *EmbeddingServer) Close() error {
	return s.cmd.Process.Kill()
}

func (s *EmbeddingServer) GetEmbedding(prompt string) ([]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.stdin.Write([]byte(prompt + "\n"))
	if err != nil {
		return nil, err
	}

	var embeddingLine string
	for {
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			embeddingLine = line
			break
		}
	}

	vecRaw := embeddingLine[1 : len(embeddingLine)-1]
	tokens := strings.Split(vecRaw, ",")
	embedding := make([]float64, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		val, err := strconv.ParseFloat(t, 64)
		if err != nil {
			continue
		}
		embedding = append(embedding, val)
	}
	return embedding, nil
}
func main() {
	if err := godotenv.Load(); err != nil {
		slog.Warn("No .env file found - using system environment variables")
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	chromaURL := getEnv("CHROMA_URL", defaultChromaURL)
	databaseName := getEnv("CHROMA_DATABASE", "default")
	collectionName := getEnv("COLLECTION_NAME", "mycollection") // Use mycollection
	// dataPath := getEnv("DATA_PATH", defaultDataPath)

	progressCb := func(status ProgressStatus) {
		slog.Info("Main progress callback", "status", status)
	}
	chromaClient := NewClient(chromaURL, databaseName, progressCb)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantID := "customer1"

	if err := chromaClient.EnsureTenantExists(ctx, tenantID); err != nil {
		slog.Error("Failed to ensure tenant exists", "tenant_id", tenantID, "error", err)
		return
	}

	if err := chromaClient.EnsureDatabaseExists(ctx, tenantID, databaseName); err != nil {
		slog.Error("Failed to ensure database exists", "tenant_id", tenantID, "database_name", databaseName, "error", err)
		return
	}

	if err := chromaClient.EnsureCollectionExists(ctx, tenantID, collectionName); err != nil {
		slog.Error("Failed to initialize collection", "tenant_id", tenantID, "error", err)
		return
	}

	// TODO: SPARA HASH PÅ DOCS ANNAN COLLECTION FÖR TRACKING VAD SOM INGESTAT

	// if err := chromaClient.IngestData(tenantID, dataPath, collectionName); err != nil {
	// 	slog.Error("Failed to ingest data", "tenant_id", tenantID, "error", err)
	// 	return
	// }

	response, err := chromaClient.QueryOllama(ctx, "Test prompt for Ollama")
	if err != nil {
		slog.Error("Ollama test query failed", "error", err)
	} else {
		slog.Info("Ollama test response", "response", response)
	}

	formatter = html.New(html.WithClasses(true))

	modelPath := getEnv("LLAMA_MODEL_PATH", "nomic-embed-text-v1.5.Q4_K_M.gguf")
	embeddingServer, err := NewEmbeddingServer(modelPath)
	if err != nil {
		slog.Error("Failed to start embedding server", "error", err)
		return
	}
	defer embeddingServer.Close()

	router := http.NewServeMux()
	router.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	})
	router.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWebSocket(w, r, chromaClient, tenantID, collectionName)
	})
	router.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		vec, err := embeddingServer.GetEmbedding(req.Text)
		if err != nil {
			http.Error(w, "Embedding error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		resp := struct {
			Embedding []float64 `json:"embedding"`
		}{Embedding: vec}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	server := &http.Server{
		Addr:         ":8080",
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	slog.Info("Server starting on :8080")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("Server failed", "error", err)
	}
}
func handleWebSocket(w http.ResponseWriter, r *http.Request, client *Client, tenantID, collectionName string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	tenantCtx := TenantContext{TenantID: tenantID}
	const maxMessages = 100
	msgChan := make(chan Message, maxMessages)
	idleTimeout := 5 * time.Minute

	go func() {
		for {
			select {
			case <-time.After(idleTimeout):
				slog.Info("WebSocket connection timed out", "tenant_id", tenantID)
				conn.Close()
				return
			default:
				_, msg, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						slog.Error("WebSocket error", "error", err)
					}
					close(msgChan)
					return
				}

				query := strings.TrimSpace(string(msg))
				if query == "" {
					continue
				}

				userMsg := Message{
					Role:    "user",
					Content: query,
					Time:    time.Now().Format("15:04:05"),
				}
				history.mu.Lock()
				history.Messages = append(history.Messages, userMsg)
				if len(history.Messages) > maxHistorySize {
					history.Messages = history.Messages[len(history.Messages)-maxHistorySize:]
				}
				history.mu.Unlock()

				if err := conn.WriteJSON(userMsg); err != nil {
					slog.Error("WebSocket write error", "error", err)
					return
				}

				msgChan <- userMsg
			}
		}
	}()

	const maxWorkers = 4
	workerChan := make(chan struct{}, maxWorkers)
	for i := 0; i < maxWorkers; i++ {
		workerChan <- struct{}{}
	}

	for msg := range msgChan {
		<-workerChan
		go func(query string) {
			defer func() { workerChan <- struct{}{} }()

			typingMsg := Message{
				Role:    "bot",
				Content: "Typing...",
				Time:    time.Now().Format("15:04:05"),
			}
			if err := conn.WriteJSON(typingMsg); err != nil {
				slog.Error("WebSocket write error", "error", err)
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			startTime := time.Now()
			output, sources, reasoning := processQuery(ctx, tenantCtx, query, client, collectionName)
			elapsed := time.Since(startTime)

			response := fmt.Sprintf("%s\n\n⏱️ Generated in %s\n\nSources:\n- %s\n\nReasoning:\n%s",
				output,
				elapsed.Round(time.Millisecond),
				strings.Join(sources, "\n- "),
				reasoning)

			botMsg := Message{
				Role:    "bot",
				Content: response,
				Time:    time.Now().Format("15:04:05"),
			}

			history.mu.Lock()
			history.Messages = append(history.Messages, botMsg)
			if len(history.Messages) > maxHistorySize {
				history.Messages = history.Messages[len(history.Messages)-maxHistorySize:]
			}
			history.mu.Unlock()

			if err := conn.WriteJSON(botMsg); err != nil {
				slog.Error("WebSocket write error", "error", err)
			}
		}(msg.Content)
	}
}
func processQuery(ctx context.Context, tenantCtx TenantContext, query string, client *Client, collectionName string) (string, []string, string) {
	slog.Info("Processing query", "query", query)
	refinedQuery, refinerReasoning := refineQuery(ctx, query)
	slog.Info("Refined query", "refined_query", refinedQuery)

	isCodeQuery := codeRegex.MatchString(strings.ToLower(query)) || containsAny(strings.ToLower(query), codeKeywords)
	isSummaryQuery := containsAny(strings.ToLower(query), summaryKeywords)

	var output string
	var sources []string
	var reasoning string

	if isCodeQuery && !isSummaryQuery {
		output, sources, reasoning = runCodeAgent(ctx, tenantCtx, refinedQuery, client, collectionName)
	} else if isSummaryQuery {
		output, sources, reasoning = runSummaryAgent(ctx, tenantCtx, refinedQuery, client, collectionName)
	} else {
		output, sources, reasoning = runDefaultPipeline(ctx, tenantCtx, refinedQuery, client, collectionName)
	}

	if strings.Contains(output, "'''") {
		output = highlightCode(output)
	}

	return output, sources, fmt.Sprintf("%s\n%s", refinerReasoning, reasoning)
}

func refineQuery(ctx context.Context, query string) (string, string) {
	prompt := fmt.Sprintf(`Rewrite the query to be clear and specific for web development documentation. If vague, infer the most likely intent (e.g., CSS, HTML, JavaScript). Return in JSON:
{
  "refined_query": "[refined query]",
  "reasoning": "[reasoning]"
}
Examples:
Query: "center a div" -> {"refined_query": "How to center a div using CSS", "reasoning": "Inferred CSS intent for centering a div."}
Query: "1234567890" -> {"refined_query": "How to use unique identifiers in HTML", "reasoning": "Assumed query refers to HTML IDs."}
Query: %s`, query)

	resp := callOllama(ctx, prompt)
	if strings.HasPrefix(resp, "Error:") {
		return query, fmt.Sprintf("Error: Failed to refine query: %s", resp)
	}

	var result struct {
		RefinedQuery string `json:"refined_query"`
		Reasoning    string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		slog.Error("Failed to parse refiner response", "response", resp, "error", err)
		return query, fmt.Sprintf("Error: Failed to refine query: %v", err)
	}
	return result.RefinedQuery, result.Reasoning
}
func (c *Client) QueryChroma(ctx context.Context, tenantID, collectionName, query string) (string, []string) {
	// Ensure tenant exists
	if err := c.EnsureTenantExists(ctx, tenantID); err != nil {
		c.handleError(fmt.Errorf("failed to ensure tenant exists: %w", err), "QueryChroma")
		return "", nil
	}

	// Ensure database exists
	if err := c.EnsureDatabaseExists(ctx, tenantID, c.databaseName); err != nil {
		c.handleError(fmt.Errorf("failed to ensure database exists: %w", err), "QueryChroma")
		return "", nil
	}

	c.collectionMu.RLock()
	collectionUUID, exists := c.collectionUUIDs[collectionName]
	c.collectionMu.RUnlock()

	if !exists {
		if err := c.EnsureCollectionExists(ctx, tenantID, collectionName); err != nil {
			c.handleError(fmt.Errorf("failed to ensure collection exists: %w", err), "QueryChroma")
			return "", nil
		}
		c.collectionMu.RLock()
		collectionUUID, exists = c.collectionUUIDs[collectionName]
		c.collectionMu.RUnlock()
		if !exists {
			c.handleError(fmt.Errorf("collection UUID not found for %s", collectionName), "QueryChroma")
			return "", nil
		}
	}

	slog.Info("Querying Chroma", "tenant_id", tenantID, "collection_uuid", collectionUUID, "query", query)
	c.updateProgress(func(p *ProgressStatus) { p.QueriesProcessed++ })

	payload := map[string]interface{}{
		"query_texts": []string{query},
		"n_results":   5,
		"include":     []string{"documents", "metadatas"},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		c.handleError(fmt.Errorf("failed to marshal query: %w", err), "QueryChroma")
		return "", nil
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections/%s/query", c.baseURL, tenantID, c.databaseName, collectionUUID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		c.handleError(fmt.Errorf("failed to create request: %w", err), "QueryChroma")
		return "", nil
	}
	req.Header.Set("Content-Type", "application/json")

	startTime := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.handleError(fmt.Errorf("query failed: %w", err), "QueryChroma")
		return "", nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		c.handleError(fmt.Errorf("query returned status %d: %s", resp.StatusCode, string(body)), "QueryChroma")
		return "", nil
	}

	var result struct {
		Documents [][]string                 `json:"documents"`
		Metadatas [][]map[string]interface{} `json:"metadatas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.handleError(fmt.Errorf("failed to decode response: %w", err), "QueryChroma")
		return "", nil
	}

	if len(result.Documents) == 0 || len(result.Documents[0]) == 0 {
		slog.Info("No documents found for query")
		return "", nil
	}

	var context strings.Builder
	var sources []string
	for i, doc := range result.Documents[0] {
		context.WriteString(doc)
		if i < len(result.Documents[0])-1 {
			context.WriteString("\n\n---\n\n")
		}
	}
	for _, meta := range result.Metadatas[0] {
		if id, ok := meta["id"].(string); ok {
			sources = append(sources, id)
		}
	}

	slog.Info("Chroma query completed", "documents_found", len(result.Documents[0]), "elapsed_time", time.Since(startTime))
	return context.String(), sources
}
func runCodeAgent(ctx context.Context, tenantCtx TenantContext, query string, client *Client, collectionName string) (string, []string, string) {
	contextStr, sources := client.QueryChroma(ctx, tenantCtx.TenantID, collectionName, query)
	if contextStr == "" {
		return "No relevant documentation found.", sources, "No context retrieved from Chroma."
	}

	prompt := fmt.Sprintf(`Given the context, generate a code example. Return JSON:
{
  "output": "**Explanation**: [explanation]\n**Code**:\n'''[language]\n[code]\n'''",
  "reasoning": "[reasoning]"
}
Context: %s
Query: %s`, contextStr, query)

	resp := callOllama(ctx, prompt)
	var result struct {
		Output    string `json:"output"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		slog.Error("Failed to parse code agent response", "error", err)
		return fmt.Sprintf("Error: %v", err), sources, "Failed to parse response."
	}
	return result.Output, sources, result.Reasoning
}
func runSummaryAgent(ctx context.Context, tenantCtx TenantContext, query string, client *Client, collectionName string) (string, []string, string) {
	contextStr, sources := client.QueryChroma(ctx, tenantCtx.TenantID, collectionName, query)
	if contextStr == "" {
		return "No relevant documentation found.", sources, "No context retrieved from Chroma."
	}

	prompt := fmt.Sprintf(`Summarize the context into 3-5 bullet points. Return JSON:
{
  "output": "[bullet points, each starting with '-']",
  "reasoning": "[reasoning]"
}
Context: %s
Query: %s`, contextStr, query)

	resp := callOllama(ctx, prompt)
	var result struct {
		Output    string `json:"output"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		slog.Error("Failed to parse summary agent response", "error", err)
		return fmt.Sprintf("Error: %v", err), sources, "Failed to parse response."
	}
	return result.Output, sources, result.Reasoning
}
func runDefaultPipeline(ctx context.Context, tenantCtx TenantContext, query string, client *Client, collectionName string) (string, []string, string) {
	contextStr, sources := client.QueryChroma(ctx, tenantCtx.TenantID, collectionName, query)
	if contextStr == "" {
		return "No relevant documentation found.", sources, "No context retrieved from Chroma."
	}

	prompt := fmt.Sprintf(`Answer the query based on the context. Return JSON:
{
  "output": "[response]",
  "reasoning": "[reasoning]"
}
Context: %s
Query: %s`, contextStr, query)

	resp := callOllama(ctx, prompt)
	var result struct {
		Output    string `json:"output"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		slog.Error("Failed to parse default pipeline response", "error", err)
		return fmt.Sprintf("Error: %v", err), sources, "Failed to parse response."
	}
	return result.Output, sources, result.Reasoning
}
func callOllama(ctx context.Context, prompt string) string {
	ollamaURL := getEnv("OLLAMA_URL", defaultOllamaURL)
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":  "smollm2:1.7b",
		"prompt": prompt,
		"stream": false,
	})
	client := &http.Client{Timeout: 60 * time.Second} // Increase timeout
	req, err := http.NewRequestWithContext(ctx, "POST", ollamaURL, bytes.NewBuffer(reqBody))
	if err != nil {
		slog.Error("Failed to create Ollama request", "error", err)
		return fmt.Sprintf("Error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		slog.Error("Ollama request failed", "error", err)
		return fmt.Sprintf("Error: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("Failed to read Ollama response", "error", err)
		return fmt.Sprintf("Error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("Ollama returned error", "status", resp.StatusCode, "response", string(body))
		return fmt.Sprintf("Error: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		slog.Error("Failed to parse Ollama response", "response", string(body), "error", err)
		return fmt.Sprintf("Error: %v", err)
	}
	return result.Response
}
func highlightCode(output string) string {
	re := regexp.MustCompile("(?s)'''(\\w+)\\n(.*?)'''")
	matches := re.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		lang, code := match[1], match[2]

		lexer := lexers.Get(lang)
		if lexer == nil {
			lexer = lexers.Fallback
		}
		lexer = chroma.Coalesce(lexer)

		iterator, err := lexer.Tokenise(nil, code)
		if err != nil {
			continue
		}

		style := styles.Get("monokai")
		if style == nil {
			style = styles.Fallback
		}

		var buf bytes.Buffer
		err = formatter.Format(&buf, style, iterator)
		if err != nil {
			continue
		}

		highlighted := buf.String()
		output = strings.Replace(output, match[0], fmt.Sprintf("<div class='chroma'>%s</div>", highlighted), 1)
	}
	return output
}

func containsAny(s string, keywords []string) bool {
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func estimateFileCount(dataPath string) (int, error) {
	count := 0
	err := filepath.Walk(dataPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".json") {
			count++
		}
		return nil
	})
	return count, err
}

func (c *Client) getTenantCollectionName(tenantID, collectionName string) string {
	return collectionName
}
