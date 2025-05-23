package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/schollz/progressbar/v3"
)

// FileInfo struct represents information about a file
type FileInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ModifiedDate string `json:"modified_date"`
	SHA256Hash   string `json:"sha256_hash"`
}

// countFiles function counts the total number of files in a directory and its children
func countFiles(root string) (int, error) {
	var fileCount int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Handle permission errors gracefully during counting too
			if os.IsPermission(err) {
				fmt.Printf("Permission error (count): %s (skipping)\n", path) // Info
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil // Skip file
			}
			return err // Propagate other errors
		}
		if !info.IsDir() {
			fileCount++
		}
		return nil
	})
	return fileCount, err
}

// calculateSHA256 function calculates the SHA256 hash of a file
func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); err != nil {
			// Maybe log this, but don't make it a fatal error for the hash
			fmt.Printf("Warning: error closing file %s: %v\n", filePath, err)
		}
	}()

	hash := sha256.New()
	// Use a buffer potentially? io.Copy usually does this well internally.
	// buf := make([]byte, 32*1024) // Example buffer
	// if _, err := io.CopyBuffer(hash, file, buf); err != nil {
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
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

func main() {
	// Suggest default workers based on CPU count
	defaultWorkers := runtime.NumCPU()
	if defaultWorkers < 4 {
		defaultWorkers = 4 // Set a minimum if few cores
	}

	// Define command line flags
	rootDir := flag.String("dir", ".", "Directory to scan")
	outputFile := flag.String("output", "file_inventory.json", "Output JSON file")
	// Use the calculated default as the default flag value
	workerCount := flag.Int("workers", defaultWorkers, "Number of concurrent hashing workers")
	flag.Parse()

	// --- Count files first for progress bar and slice allocation ---
	fmt.Printf("Counting files in directory: %s...\n", *rootDir)
	totalFiles, err := countFiles(*rootDir)
	if err != nil {
		fmt.Printf("Error counting files: %v\n", err)
		os.Exit(1)
	}
	if totalFiles == 0 {
		fmt.Println("No files found to process.")
		// Create an empty JSON array?
		err := saveToJSON([]FileInfo{}, *outputFile) // Save empty results
		if err != nil {
			fmt.Printf("Error saving empty JSON: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Results saved to %s\n", *outputFile)
		os.Exit(0)
	}
	fmt.Printf("Found %d files. Starting scan with %d workers...\n", totalFiles, *workerCount)

	bar := progressbar.Default(int64(totalFiles))

	// --- Setup Channels and Semaphore ---
	// Use a semaphore to limit concurrent goroutines for hashing
	sem := make(chan struct{}, *workerCount)
	// Buffered channels are good practice here
	resultsChan := make(chan FileInfo, *workerCount)
	errChan := make(chan error, *workerCount) // Collect errors from goroutines
	var wg sync.WaitGroup                     // To wait for all hashing goroutines

	// --- Goroutine to collect results ---
	var files = make([]FileInfo, 0, totalFiles) // Pre-allocate slice
	doneCollecting := make(chan struct{})
	go func() {
		for res := range resultsChan {
			files = append(files, res)
		}
		close(doneCollecting) // Signal that collection is finished
	}()

	var errorCount int
	var errorMutex sync.Mutex
	errorWg := sync.WaitGroup{} // Use WaitGroup
	errorWg.Add(1)
	go func() {
		defer errorWg.Done()
		for procErr := range errChan {
			fmt.Printf("Processing error: %v\n", procErr)
			errorMutex.Lock()
			errorCount++
			errorMutex.Unlock()
		}
	}()

	// --- Walk the directory tree (still serial walk, but concurrent processing) ---
	walkErr := filepath.Walk(*rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Gracefully handle permission errors
			if os.IsPermission(err) {
				fmt.Printf("Permission error accessing: %s (skipping)\n", path)
				// If it's a directory we can't enter, skip its contents
				if info != nil && info.IsDir() {
					return filepath.SkipDir
				}
				return nil // Skip the file if permission error on the file itself
			}
			// Report other walk errors but continue if possible
			fmt.Printf("Error accessing %s: %v (skipping)\n", path, err)
			errChan <- fmt.Errorf("walk error accessing %s: %w", path, err) // Send to *concurrent* reader
			return nil                                                      // Returning nil tries to continue the walk
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Skip "special" files
		fileMode := info.Mode()
		if fileMode&os.ModeNamedPipe != 0 ||
			fileMode&os.ModeSocket != 0 ||
			fileMode&os.ModeDevice != 0 ||
			fileMode&os.ModeCharDevice != 0 ||
			fileMode&os.ModeSymlink != 0 { // Also good to skip symlinks or resolve them carefully

			fmt.Printf("Skipping special file or symlink: %s\n", path)
			// Increment bar since it was counted but won't be processed by a worker.
			// Ensure bar is thread-safe or handle this carefully.
			// Since bar.Add is called *inside* the worker's defer now,
			// we MUST call it here too, otherwise the count will be off.
			if err := bar.Add(1); err != nil {
				fmt.Printf("error updating progress bar for skipped file: %v\n", err)
			}
			return nil // Skip, don't wg.Add or launch goroutine
		}

		// --- Process the file concurrently ---
		wg.Add(1) // Increment counter before starting goroutine

		// Acquire semaphore - this blocks if workerCount goroutines are already running
		sem <- struct{}{}

		// Launch goroutine to process this file
		go func(filePath string, fileInfo os.FileInfo) {
			// Release semaphore and decrement counter when done
			defer func() {
				<-sem
				wg.Done()
			}()

			// Calculate SHA256 hash
			hash, hashErr := calculateSHA256(filePath)
			if hashErr != nil {
				// Report error calculating hash
				errChan <- fmt.Errorf("error hashing %s: %w", filePath, hashErr)
				// Update progress bar even if there's an error
				if err := bar.Add(1); err != nil {
					fmt.Printf("error updating progress bar: %v\n", err)
				}
				return // Don't send result if hashing failed
			}

			// Extract info (use fileInfo passed in, NO redundant os.Stat)
			dir, fileName := filepath.Split(filePath)
			formattedModTime := fileInfo.ModTime().UTC().Format("2006-01-02T15:04:05Z")

			// Create FileInfo object and send to results channel
			result := FileInfo{
				Name:         fileName,
				Path:         dir,
				ModifiedDate: formattedModTime,
				SHA256Hash:   hash,
			}
			resultsChan <- result

			// Update progress bar after processing is complete
			if err := bar.Add(1); err != nil {
				fmt.Printf("error updating progress bar: %v\n", err)
			}

		}(path, info) // Pass current path and info to the goroutine!

		return nil // Continue walk
	})

	// --- Wait for completion and cleanup ---

	// Wait for all file processing goroutines to finish
	wg.Wait()

	// Close channels: No more results or errors will be sent
	close(resultsChan)
	close(errChan)

	// Wait for the results collection goroutine to finish
	<-doneCollecting

	errorWg.Wait() // Wait for the error collection goroutine

	// Check for critical error during the walk itself
	if walkErr != nil {
		fmt.Printf("Critical error during directory walk: %v\n", walkErr)
		// Depending on the error, you might still want to save partial results
	}

	// --- Use errorCount directly ---
	if errorCount > 0 {
		fmt.Printf("Encountered %d errors during processing.\n", errorCount)
	}

	// --- Save results ---
	fmt.Printf("\nProcessed %d files.\n", len(files))
	err = saveToJSON(files, *outputFile)
	if err != nil {
		fmt.Printf("Error saving to JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Results saved to %s\n", *outputFile)
}
