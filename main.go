package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileInfo struct represents information about a file
type FileInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ModifiedDate string `json:"modified_date"`
	SHA256Hash   string `json:"sha256_hash"`
}

// --- Define files/dirs to skip ---
var skipNames = map[string]bool{
	"Docker.raw":                true,
	".Trash":                    true,
	".Trash-1000":               true,
	"$RECYCLE.BIN":              true,
	"System Volume Information": true,
	".DocumentRevisions-V100":   true,
	".fseventsd":                true,
	".Spotlight-V100":           true,
	".TemporaryItems":           true,
	".Trashes":                  true,
	".git":                      true,
}

// --- Define paths to skip (prefixes) ---
var skipPaths = []string{
	"/dev",
	"/proc",
	"/sys",
}

// PathToProcess struct to send paths AND info to workers
type PathToProcess struct {
	Path string
	Info os.FileInfo
}

// calculateSHA256 function calculates the SHA256 hash of a file
func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}

	hash := sha256.New()

	buf := make([]byte, 1024*1024) // 1MB buffer size
	_, err = io.CopyBuffer(hash, file, buf)

	closeErr := file.Close()

	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}

	hashInBytes := hash.Sum(nil)
	return hex.EncodeToString(hashInBytes), nil
}

// saveToJSON function saves the file information to a JSON file
func saveToJSON(files []FileInfo, outputPath string) error {
	jsonData, err := json.Marshal(files) // Simply Marshal the output
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, jsonData, 0644)
}

// run function contains the main application logic
func run() int {
	// --- Flag Parsing ---
	defaultWorkers := runtime.NumCPU() * 2 // More workers often better for I/O bound
	if defaultWorkers < 4 {
		defaultWorkers = 4
	}
	rootDir := flag.String("dir", ".", "Directory to scan")
	outputFile := flag.String("output", "file_inventory.json", "Output JSON file")
	workerCount := flag.Int("workers", defaultWorkers, "Number of concurrent hashing workers")
	flag.Parse()

	fmt.Printf("Starting scan in %s with %d workers (no pre-count)...\n", *rootDir, *workerCount)

	// --- Setup Channels ---
	pathsChan := make(chan PathToProcess, *workerCount)
	resultsChan := make(chan FileInfo, *workerCount)
	errChan := make(chan error, *workerCount*2) // Larger buffer for errors
	var workersWg sync.WaitGroup

	// --- Setup Progress and Error Counters ---
	var processedCount int64
	var errorCount int64
	var foundCount int64 // Track files found by walk

	// --- Results Collector Goroutine ---
	var files = make([]FileInfo, 0) // Don't pre-allocate
	doneCollecting := make(chan struct{})
	go func() {
		defer close(doneCollecting)
		for res := range resultsChan {
			files = append(files, res)
			processed := atomic.AddInt64(&processedCount, 1)
			if processed%1000 == 0 { // Print progress every 1000 files
				fmt.Printf("... Processed %d files ...\n", processed)
			}
		}
	}()

	// --- Error Collector Goroutine ---
	doneErrors := make(chan struct{})
	go func() {
		defer close(doneErrors)
		for err := range errChan {
			fmt.Printf("ERROR: %v\n", err) // Print errors as they happen
			atomic.AddInt64(&errorCount, 1)
		}
	}()

	// --- Worker Goroutines ---
	for i := 0; i < *workerCount; i++ {
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			for item := range pathsChan {
				hash, hashErr := calculateSHA256(item.Path)
				if hashErr != nil {
					errChan <- fmt.Errorf("hashing %s: %w", item.Path, hashErr)
					continue
				}

				dir, fileName := filepath.Split(item.Path)
				formattedModTime := item.Info.ModTime().UTC().Format("2006-01-02T15:04:05Z")

				resultsChan <- FileInfo{
					Name:         fileName,
					Path:         dir,
					ModifiedDate: formattedModTime,
					SHA256Hash:   hash,
				}
			}
		}()
	}

	// --- Walk the directory tree ---
	fmt.Println("Starting directory walk...")
	walkErr := filepath.Walk(*rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			errChan <- fmt.Errorf("walk access error at %s: %w", path, err)
			return nil // Try to continue
		}

		baseName := filepath.Base(path)

		// Check skip names (files or dirs)
		if skipNames[baseName] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Check skip paths (prefixes)
		for _, skip := range skipPaths {
			if strings.HasPrefix(path, skip) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// Skip directories (after checking if they should be skipped entirely)
		if info.IsDir() {
			return nil
		}

		// Skip non-regular files
		if !info.Mode().IsRegular() {
			return nil
		}

		// It's a file to process!
		atomic.AddInt64(&foundCount, 1)
		pathsChan <- PathToProcess{Path: path, Info: info}

		return nil
	})

	// --- Close pathsChan ONLY when Walk is completely done ---
	close(pathsChan)
	fmt.Printf("Directory walk finished. Found %d potential files. Waiting for workers...\n", atomic.LoadInt64(&foundCount))

	// --- Wait for workers to finish processing everything in pathsChan ---
	workersWg.Wait()
	fmt.Println("Workers finished.")

	// --- Close other channels to signal collectors ---
	close(resultsChan)
	close(errChan)

	// --- Wait for collectors to finish ---
	<-doneCollecting
	<-doneErrors

	// --- Final Report ---
	if walkErr != nil {
		fmt.Printf("Warning: Directory walk encountered a persistent error: %v\n", walkErr)
	}

	finalProcessed := atomic.LoadInt64(&processedCount)
	finalErrors := atomic.LoadInt64(&errorCount)
	fmt.Printf("\nFinished. Processed %d files with %d errors.\n", finalProcessed, finalErrors)

	// --- Save results ---
	fmt.Printf("Saving %d results to %s...\n", len(files), *outputFile)
	err := saveToJSON(files, *outputFile)
	if err != nil {
		fmt.Printf("FATAL: Error saving to JSON: %v\n", err)
		return 1
	}

	fmt.Printf("Results saved successfully.\n")
	return 0
}

// main function - sets up pprof, timer and calls run
func main() {
	// Start pprof server (useful for debugging hangs/performance)
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	startTime := time.Now() // Record start time

	// Defer the final time print - this will run just before os.Exit
	defer func() {
		duration := time.Since(startTime)
		fmt.Printf("\n-------------------------------\n")
		fmt.Printf("Total execution time: %v\n", duration)
		fmt.Printf("-------------------------------\n")
	}()

	// Call the run function and get the exit code
	exitCode := run()

	// Exit with the code from run()
	os.Exit(exitCode)
}
