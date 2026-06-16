package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StudentResult mirrors the struct in scraper.go for deserialization.
type StudentResult struct {
	USN       string           `json:"usn"`
	Name      string           `json:"name"`
	Branch    string           `json:"branch"`
	Semesters []SemesterResult `json:"semesters"`
}

type SemesterResult struct {
	ExamID   int      `json:"examId"`
	Semester string   `json:"semester"`
	SGPA     float64  `json:"sgpa"`
	CGPA     float64  `json:"cgpa"`
	Courses  []Course `json:"courses"`
}

type Course struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	CreditsReg    string `json:"creditsReg"`
	CreditsEarned string `json:"creditsEarned"`
	ISE           string `json:"ise"`
	ESE           string `json:"ese"`
	Total         string `json:"total"`
	Grade         string `json:"grade"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: merge <input_dir> <output_file>\n")
		fmt.Fprintf(os.Stderr, "  Merges all results_*.json files from input_dir into output_file\n")
		os.Exit(1)
	}

	inputDir := os.Args[1]
	outputFile := os.Args[2]

	// Find all results_*.json files
	matches, err := filepath.Glob(filepath.Join(inputDir, "results_*.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Glob error: %v\n", err)
		os.Exit(1)
	}

	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "⚠ No results_*.json files found in %s\n", inputDir)
		os.Exit(0)
	}

	fmt.Printf("📂 Found %d result file(s) to merge:\n", len(matches))
	for _, m := range matches {
		fmt.Printf("   • %s\n", filepath.Base(m))
	}

	// Merge all files, deduplicating by USN (keep the record with more semesters)
	usnMap := make(map[string]*StudentResult)

	totalLoaded := 0
	for _, file := range matches {
		raw, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠ Skipping %s: %v\n", filepath.Base(file), err)
			continue
		}

		var students []StudentResult
		if err := json.Unmarshal(raw, &students); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ Skipping %s: invalid JSON: %v\n", filepath.Base(file), err)
			continue
		}

		totalLoaded += len(students)
		for i := range students {
			s := &students[i]
			key := strings.ToUpper(s.USN)

			existing, found := usnMap[key]
			if !found {
				usnMap[key] = s
			} else {
				// Keep the one with more semesters
				if len(s.Semesters) > len(existing.Semesters) {
					usnMap[key] = s
				}
			}
		}
	}

	// Collect and sort by USN
	var merged []StudentResult
	for _, s := range usnMap {
		merged = append(merged, *s)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].USN < merged[j].USN
	})

	// Write merged output
	f, err := os.Create(outputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot create %s: %v\n", outputFile, err)
		os.Exit(1)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(merged); err != nil {
		fmt.Fprintf(os.Stderr, "❌ JSON encode error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✅ Merged %d records from %d loaded (deduped %d)\n", len(merged), totalLoaded, totalLoaded-len(merged))
	fmt.Printf("💾 Written to %s\n", outputFile)

	// Also merge failed USN lists
	failMatches, _ := filepath.Glob(filepath.Join(inputDir, "failed_*.json"))
	if len(failMatches) > 0 {
		var allFailed []string
		seen := make(map[string]bool)

		for _, file := range failMatches {
			raw, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			var failed []string
			if err := json.Unmarshal(raw, &failed); err != nil {
				continue
			}
			for _, usn := range failed {
				key := strings.ToUpper(usn)
				if !seen[key] {
					seen[key] = true
					allFailed = append(allFailed, usn)
				}
			}
		}

		sort.Strings(allFailed)
		failOut := strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + "_failed.json"
		ff, err := os.Create(failOut)
		if err == nil {
			enc := json.NewEncoder(ff)
			enc.SetIndent("", "  ")
			enc.Encode(allFailed)
			ff.Close()
			fmt.Printf("📋 Merged %d failed USNs → %s\n", len(allFailed), failOut)
		}
	}

	// Write summary for GitHub Actions
	if summaryFile := os.Getenv("GITHUB_STEP_SUMMARY"); summaryFile != "" {
		summary := fmt.Sprintf("## Merge Summary\n\n| Metric | Value |\n|---|---|\n| Files merged | %d |\n| Total records loaded | %d |\n| Unique students | %d |\n| Duplicates removed | %d |\n",
			len(matches), totalLoaded, len(merged), totalLoaded-len(merged))
		os.WriteFile(summaryFile, []byte(summary), 0644)
	}
}
