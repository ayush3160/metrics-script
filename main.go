package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

type GenerateTestRequest struct {
	SrcFilePath       string  `json:"sourceFilePath"`
	RootDir           string  `json:"rootDir"`
	AdditionalPrompt  string  `json:"additionalPrompt"`
	MaxIterations     int     `json:"maxIterations"`
	Flakiness         bool    `json:"flakiness"`
	FunctionUnderTest string  `json:"functionUnderTest"`
	ExpectedCoverage  float64 `json:"expectedCoverage"`
}

type Metrics struct {
	InitialCoverage float64
	FinalCoverage   float64
	LinesCovered    float64
	TotalLines      float64
	TestAdded       float64
}

func metricsToInterfaceSlice(m Metrics) []interface{} {
	return []interface{}{m.InitialCoverage, m.FinalCoverage, m.LinesCovered, m.TotalLines, m.TestAdded}
}

const apiURL = "http://localhost:4407/api/generate"

func main() {
	// Start tracking total execution time
	globalStartTime := time.Now()
	fmt.Printf("Execution started at: %s\n", globalStartTime.Format(time.RFC3339))

	rootDir, err := os.Getwd()
	if err != nil {
		fmt.Println("Error getting root directory:", err)
		return
	}

	var goFiles []string
	err = filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == "venv" || info.Name() == "migrations" || info.Name() == "__pycache__") {
			return filepath.SkipDir
		}

		if !info.IsDir() && filepath.Ext(path) == ".py" && !isTestFile(path) && info.Name() != "__init__.py" {
			goFiles = append(goFiles, path)
		}
		return nil
	})

	if err != nil {
		fmt.Println("Error walking through project files:", err)
		return
	}

	// Create an Excel file
	excelFile := excelize.NewFile()
	sheetName := "Execution Log"
	excelFile.SetSheetName("Sheet1", sheetName)

	// Set header row
	headers := []string{"Filepath", "Initial Coverage", "Final Coverage", "Lines Covered", "Total Lines", "Tests Added", "Time Duration", "Start Time", "End Time"}
	for col, header := range headers {
		cell := fmt.Sprintf("%s1", string(rune(65+col))) // Column letters start from 'A'
		excelFile.SetCellValue(sheetName, cell, header)
	}

	row := 2 // Start filling data from the second row

	// Prepare Excel filename with timestamp
	excelFilename := "execution_log_2.xlsx"

	// Iterate through files
	for _, file := range goFiles {
		requestBody := GenerateTestRequest{
			SrcFilePath:       file,
			RootDir:           rootDir,
			AdditionalPrompt:  "",
			MaxIterations:     0,
			Flakiness:         false,
			FunctionUnderTest: "",
			ExpectedCoverage:  0.0,
		}

		// Measure execution time of sendRequest and get coverage values
		duration, metrics, startTime, endTime, err := measureDuration(requestBody)
		if err != nil {
			fmt.Printf("Failed to send request for %s: %v\n", file, err)
			continue
		}

		relativeName, err := filepath.Rel(rootDir, file)
		if err != nil {
			fmt.Printf("Failed to get relative path for %s: %v\n", file, err)
			relativeName = file
		}

		// Store data in Excel
		data := []interface{}{relativeName, metrics.InitialCoverage, metrics.FinalCoverage, metrics.LinesCovered, metrics.TotalLines, metrics.TestAdded, duration.String(), startTime.Format(time.RFC3339), endTime.Format(time.RFC3339)}
		for col, value := range data {
			cell := fmt.Sprintf("%s%d", string(rune(65+col)), row)
			excelFile.SetCellValue(sheetName, cell, value)
		}

		// Save the Excel file after each iteration
		if err := excelFile.SaveAs(excelFilename); err != nil {
			fmt.Printf("Failed to save Excel file after processing %s: %v\n", file, err)
			// Continue processing even if save fails
		} else {
			fmt.Printf("Saved progress after processing %s\n", file)
		}

		row++
	}

	// Compute and log total execution time
	globalEndTime := time.Now()
	globalDuration := globalEndTime.Sub(globalStartTime)
	fmt.Printf("Execution completed at: %s\nTotal Execution Time: %s\nExcel file saved as %s\n",
		globalEndTime.Format(time.RFC3339), globalDuration, excelFilename)
}

func isTestFile(path string) bool {
	return strings.Contains(path, "_test.go") || strings.Contains(path, "test_") // Proper Go test file naming convention
}

// measureDuration executes sendRequest, logs execution time, and returns coverage data
func measureDuration(requestBody GenerateTestRequest) (time.Duration, Metrics, time.Time, time.Time, error) {
	startTime := time.Now()
	fmt.Printf("Processing file: %s\nStart Time: %s\n", requestBody.SrcFilePath, startTime.Format(time.RFC3339))

	metrics, err := sendRequest(requestBody)
	if err != nil {
		return 0, Metrics{}, startTime, time.Time{}, err
	}

	endTime := time.Now()
	duration := endTime.Sub(startTime)

	fmt.Printf("Finished processing file: %s\nEnd Time: %s\nDuration: %s\n",
		requestBody.SrcFilePath, endTime.Format(time.RFC3339), duration)

	return duration, metrics, startTime, endTime, nil
}

func sendRequest(requestBody GenerateTestRequest) (Metrics, error) {
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return Metrics{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return Metrics{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 0, // No timeout to allow long-lived streaming
	}
	resp, err := client.Do(req)
	if err != nil {
		return Metrics{}, fmt.Errorf("failed to send POST request: %w", err)
	}
	defer resp.Body.Close()

	// Print response status for debugging
	fmt.Printf("Response Status: %d\n", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return Metrics{}, fmt.Errorf("received non-OK response: %d\nBody: %s", resp.StatusCode, string(bodyBytes))
	}

	// Read the response stream line by line
	reader := bufio.NewReader(resp.Body)
	fmt.Printf("Streaming response for %s:\n", requestBody.SrcFilePath)

	decoder := json.NewDecoder(reader)
	var initialCoverage, finalCoverage, linesCovered, totalLines, testAdded float64

	for {
		var event map[string]interface{}
		err := decoder.Decode(&event)
		if err == io.EOF {
			fmt.Println("\nStream ended.")
			break
		}
		if err != nil {
			return Metrics{}, fmt.Errorf("error reading JSON stream: %w", err)
		}

		if event["dataType"] == "calculatedCoverage" {
			fmt.Println("Calculated Coverage:", event["calculatedCoverage"])
			re := regexp.MustCompile(`\d+(\.\d+)?`) // Removed lookahead
			numbers := re.FindAllString(event["calculatedCoverage"].(string), -1)

			if len(numbers) > 0 {
				initialCoverage = toFloat(numbers[len(numbers)-1]) // Get last match
			} else {
				fmt.Println("Warning: calculatedCoverage value missing or invalid")
			}
		}

		if event["dataType"] == "summary" {
			fmt.Println("Final Coverage:", event["coverageIncreased"])

			coverageStr, ok := event["coverageIncreased"].(string)
			if !ok || coverageStr == "" {
				fmt.Println("Warning: coverageIncreased value missing or invalid")
				finalCoverage = 0
			}

			if event["coverageIncreased"] == "Coverage did not increase" {
				finalCoverage = initialCoverage
			}

			re := regexp.MustCompile(`\d+`)
			match := re.FindString(event["coverageIncreased"].(string))

			if match != "" {
				finalCoverage = toFloat(match)
			} else {
				fmt.Println("Warning: calculatedCoverage value missing or invalid")
			}

			coverageStr, ok = event["linesCovered"].(string)
			if !ok || coverageStr == "" {
				fmt.Println("Warning: linesCovered value missing or invalid")
				linesCovered = 0
			}

			match = re.FindString(event["linesCovered"].(string))

			if match != "" {
				linesCovered = toFloat(match)
			} else {
				fmt.Println("Warning: linesCovered value missing or invalid")
			}

			coverageStr, ok = event["totalLines"].(string)
			if !ok || coverageStr == "" {
				fmt.Println("Warning: totalLines value missing or invalid")
				totalLines = 0
			}

			match = re.FindString(event["totalLines"].(string))

			if match != "" {
				totalLines = toFloat(match)
			} else {
				fmt.Println("Warning: totalLines value missing or invalid")
			}

			coverageStr, ok = event["testAdded"].(string)
			if !ok || coverageStr == "" {
				fmt.Println("Warning: testAdded value missing or invalid")
				testAdded = 0
			}

			match = re.FindString(event["testAdded"].(string))

			if match != "" {
				testAdded = toFloat(match)
			} else {
				fmt.Println("Warning: testAdded value missing or invalid")
			}
		}
	}

	metrics := Metrics{
		InitialCoverage: initialCoverage,
		FinalCoverage:   finalCoverage,
		LinesCovered:    linesCovered,
		TotalLines:      totalLines,
		TestAdded:       testAdded,
	}

	fmt.Printf("\nSuccessfully processed events for: %s\n", requestBody.SrcFilePath)
	return metrics, nil
}

func toFloat(s string) float64 {
	num, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return num
}

func extractNumbers(s string) string {
	re := regexp.MustCompile(`\d+(\.\d+)?`)
	numbers := re.FindAllString(s, -1)
	if len(numbers) > 0 {
		return numbers[len(numbers)-1]
	}
	return ""
}
