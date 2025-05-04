// main.go
package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileInfo represents information about a file
type FileInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ModifiedDate string `json:"modified_date"`
	MD5Hash      string `json:"md5_hash"`
}

// scanFiles scans directories recursively and returns file information
func scanFiles(root string, workerCount int) ([]FileInfo, error) {
	var files []FileInfo
	var wg sync.WaitGroup
	fileChan := make(chan FileInfo)
	errChan := make(chan error)

	// Start worker goroutines
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range fileChan {
				files = append(files, file)
			}
		}()
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Printf("Error accessing path %s: %v\n", path, err)
			return nil // Continue walking despite errors
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Calculate MD5 hash
		hash, err := calculateMD5(path)
		if err != nil {
			fmt.Printf("Error calculating MD5 for %s: %v\n", path, err)
			errChan <- err
			return nil // Continue despite errors
		}

		// Get file path without the filename
		dir, fileName := filepath.Split(path)

		// Get file stats for creation time
		fileInfo, err := os.Stat(path)
		if err != nil {
			fmt.Printf("Error getting file stats for %s: %v\n", path, err)
			errChan <- err
			return nil // Continue despite errors
		}

		formattedModTime := fileInfo.ModTime().UTC().Format("2006-01-02T15:04:05Z")

		// Create FileInfo object
		file := FileInfo{
			Name:         fileName,
			Path:         dir,
			ModifiedDate: formattedModTime,
			MD5Hash:      hash,
		}

		fileChan <- file
		return nil
	})

	close(fileChan)
	wg.Wait()
	close(errChan)

	if err != nil {
		fmt.Printf("Error scanning files: %v\n", err)
		os.Exit(1)
	}

	for err := range errChan {
		fmt.Printf("Worker error: %v\n", err)
	}

	return files, nil
}

// calculateMD5 calculates the MD5 hash of a file
func calculateMD5(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Printf("Error when closing file: %v\n", err)
		}
	}()

	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	hashInBytes := hash.Sum(nil)
	return hex.EncodeToString(hashInBytes), nil
}

// saveToJSON saves the file information to a JSON file
func saveToJSON(files []FileInfo, outputPath string) error {
	jsonData, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(outputPath, jsonData, 0644)
}

func main() {
	// Define command line flags
	rootDir := flag.String("dir", ".", "Directory to scan")
	outputFile := flag.String("output", "file_inventory.json", "Output JSON file")
	workerCount := flag.Int("workers", 10, "Number of worker goroutines")
	flag.Parse()

	fmt.Printf("Scanning directory: %s\n", *rootDir)

	// Scan files recursively with multiple workers
	files, err := scanFiles(*rootDir, *workerCount)
	if err != nil {
		fmt.Printf("Error scanning files: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d files\n", len(files))

	// Save results to JSON
	err = saveToJSON(files, *outputFile)
	if err != nil {
		fmt.Printf("Error saving to JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Results saved to %s\n", *outputFile)
}
