package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ────────────────────── CONFIG ──────────────────────
const (
	baseURL        = "https://witresults.contineo.in:7074"
	constCC        = 11
	requestTimeout = 12 * time.Second
	maxConcurrency = 30  // GH Actions can handle more network I/O
	batchSize      = 20  // larger batches = less loop overhead
	maxExamID      = 12
	dummyCaptcha   = "xxxxx"
	maxConsecFail  = 3   // stop descending exam IDs after this many consecutive failures
	emptyBatchCut  = 5   // skip to next branch/dsy after this many consecutive empty batches
	maxRoll        = 350 // upper bound on roll numbers
	flushInterval  = 25  // flush to disk every N results
	maxRetries     = 3   // HTTP retries per exam-ID fetch
)

var (
	defaultYears = []int{22, 23, 24, 25}
	dsyValues    = []int{1, 2}
)

// branchesForYear returns the valid branches for a given admission year.
// Branch 7 (AI/ML) was introduced in year 25, so older years only have 1–6.
func branchesForYear(year int) []int {
	if year >= 25 {
		return []int{1, 2, 3, 4, 5, 6, 7}
	}
	return []int{1, 2, 3, 4, 5, 6}
}

// ────────────────────── TRANSPORT ──────────────────────

var sharedTransport = &http.Transport{
	MaxIdleConns:        200,
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	ForceAttemptHTTP2:   true,
	DisableCompression:  true,
}

// ────────────────────── DATA STRUCTS ──────────────────────

// Course holds one row from the results table.
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

// SemesterResult holds the data for one semester.
type SemesterResult struct {
	ExamID   int      `json:"examId"`
	Semester string   `json:"semester"`
	SGPA     float64  `json:"sgpa"`
	CGPA     float64  `json:"cgpa"`
	Courses  []Course `json:"courses"`
}

// StudentResult is the top-level record for one student.
type StudentResult struct {
	USN       string           `json:"usn"`
	Name      string           `json:"name"`
	Branch    string           `json:"branch"`
	Semesters []SemesterResult `json:"semesters"`
}

// ────────────────────── HTTP HELPERS ──────────────────────

func doPost(client *http.Client, uri string, data url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", uri, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return client.Do(req)
}

// backoffSleep sleeps for exponential backoff: 1s, 2s, 4s, ...
func backoffSleep(attempt int) {
	d := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	time.Sleep(d)
}

// fetchExamPage performs the 3-step handshake for a specific exam ID with retries.
func fetchExamPage(usn string, examID int) (*goquery.Document, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoffSleep(attempt - 1)
		}

		jar, _ := cookiejar.New(nil)
		client := &http.Client{
			Transport: sharedTransport,
			Jar:       jar,
			Timeout:   requestTimeout,
		}

		// Step 1: Session init
		r1, err := doPost(client, baseURL+"/index.php?option=com_examresult&task=getResult", url.Values{
			"usn": {usn}, "securityCode": {dummyCaptcha},
		})
		if err != nil {
			lastErr = err
			continue
		}
		r1.Body.Close()

		// Step 2: Transition
		r2, _ := doPost(client, baseURL+"/index.php", url.Values{})
		if r2 != nil {
			r2.Body.Close()
		}

		// Step 3: Actual result page
		r3, err := doPost(client, baseURL+"/index.php", url.Values{
			"option": {"com_examresult"},
			"task":   {"getResultexam"},
			"usn":    {usn},
			"examId": {strconv.Itoa(examID)},
		})
		if err != nil {
			lastErr = err
			continue
		}
		defer r3.Body.Close()

		doc, err := goquery.NewDocumentFromReader(r3.Body)
		if err != nil {
			lastErr = err
			continue
		}
		return doc, nil
	}

	return nil, fmt.Errorf("all %d retries failed for %s examID=%d: %v", maxRetries, usn, examID, lastErr)
}

// ────────────────────── PARSING ──────────────────────

// normalizeBranch strips the ", Sem X" suffix from branch strings.
func normalizeBranch(raw string) string {
	if idx := strings.LastIndex(raw, ", Sem"); idx != -1 {
		return strings.TrimSpace(raw[:idx])
	}
	return strings.TrimSpace(raw)
}

// parsePage extracts student info, courses, SGPA, and CGPA from the result page.
func parsePage(doc *goquery.Document, usn string, examID int) (*StudentResult, error) {
	usnText := doc.Find(".stu-data2 h2").Text()
	if !strings.Contains(strings.ToLower(usnText), strings.ToLower(usn)) {
		return nil, nil
	}

	name := strings.TrimSpace(doc.Find(".stu-data1 h3").Text())
	rawBranch := strings.TrimSpace(doc.Find(".stu-data2 p").Text())
	branch := normalizeBranch(rawBranch)

	sgpa, _ := strconv.ParseFloat(strings.TrimSpace(doc.Find(".credits-sec3 p").Text()), 64)
	cgpa, _ := strconv.ParseFloat(strings.TrimSpace(doc.Find(".credits-sec4 p").Text()), 64)

	semMap := make(map[string]*SemesterResult)
	var semOrder []string

	doc.Find("table.res-table").Each(func(tableIdx int, table *goquery.Selection) {
		table.Find("tbody tr").Each(func(rowIdx int, row *goquery.Selection) {
			cells := row.Find("td")
			if cells.Length() < 9 {
				return
			}

			semNum := strings.TrimSpace(cells.Eq(0).Text())
			code := strings.TrimSpace(cells.Eq(1).Text())
			if semNum == "" || code == "" {
				return
			}

			course := Course{
				Code:          code,
				Name:          strings.TrimSpace(cells.Eq(2).Text()),
				CreditsReg:    strings.TrimSpace(cells.Eq(3).Text()),
				CreditsEarned: strings.TrimSpace(cells.Eq(4).Text()),
				ISE:           strings.TrimSpace(cells.Eq(5).Text()),
				ESE:           strings.TrimSpace(cells.Eq(6).Text()),
				Total:         strings.TrimSpace(cells.Eq(7).Text()),
				Grade:         strings.TrimSpace(cells.Eq(8).Text()),
			}

			semKey := "Sem " + semNum
			if _, exists := semMap[semKey]; !exists {
				semMap[semKey] = &SemesterResult{
					ExamID:   examID,
					Semester: semKey,
					Courses:  []Course{},
				}
				semOrder = append(semOrder, semKey)
			}
			semMap[semKey].Courses = append(semMap[semKey].Courses, course)
		})
	})

	if len(semMap) == 0 {
		return nil, nil
	}

	// Attach SGPA/CGPA to the last (highest) semester found on this page
	if len(semOrder) > 0 {
		lastSem := semOrder[len(semOrder)-1]
		semMap[lastSem].SGPA = sgpa
		semMap[lastSem].CGPA = cgpa
	}

	var semesters []SemesterResult
	for _, key := range semOrder {
		semesters = append(semesters, *semMap[key])
	}

	return &StudentResult{
		USN:       usn,
		Name:      name,
		Branch:    branch,
		Semesters: semesters,
	}, nil
}

// ────────────────────── CORE FETCH ──────────────────────

// fetchAllSemesters tries exam IDs from maxExamID down to 1 sequentially,
// collecting all semesters. Stops after maxConsecFail consecutive failures
// following at least one success.
func fetchAllSemesters(usn string) *StudentResult {
	var student *StudentResult
	seenSemesters := make(map[string]int) // semester key -> index in student.Semesters
	consecFails := 0
	foundAny := false

	for eid := maxExamID; eid >= 1; eid-- {
		doc, err := fetchExamPage(usn, eid)
		if err != nil {
			consecFails++
			if foundAny && consecFails >= maxConsecFail {
				break
			}
			continue
		}

		result, err := parsePage(doc, usn, eid)
		if err != nil || result == nil {
			consecFails++
			if foundAny && consecFails >= maxConsecFail {
				break
			}
			continue
		}

		foundAny = true
		consecFails = 0

		if student == nil {
			student = &StudentResult{
				USN:    result.USN,
				Name:   result.Name,
				Branch: result.Branch,
			}
		}

		for _, sem := range result.Semesters {
			if idx, exists := seenSemesters[sem.Semester]; exists {
				existing := &student.Semesters[idx]
				if len(sem.Courses) > len(existing.Courses) {
					if sem.SGPA == 0 && existing.SGPA != 0 {
						sem.SGPA = existing.SGPA
						sem.CGPA = existing.CGPA
					}
					student.Semesters[idx] = sem
				} else {
					if existing.SGPA == 0 && sem.SGPA != 0 {
						existing.SGPA = sem.SGPA
						existing.CGPA = sem.CGPA
					}
				}
			} else {
				seenSemesters[sem.Semester] = len(student.Semesters)
				student.Semesters = append(student.Semesters, sem)
			}
		}
	}

	return student
}

// ────────────────────── BUFFERED WRITERS ──────────────────────

type bufferedResultWriter struct {
	mu       sync.Mutex
	results  []StudentResult
	usnIdx   map[string]int
	outFile  string
	pending  int
	flushed  int
}

func newBufferedResultWriter(outFile string) *bufferedResultWriter {
	w := &bufferedResultWriter{
		outFile: outFile,
		usnIdx:  make(map[string]int),
	}
	// Load existing data
	if raw, err := os.ReadFile(outFile); err == nil {
		json.Unmarshal(raw, &w.results)
		for i, s := range w.results {
			w.usnIdx[strings.ToUpper(s.USN)] = i
		}
		w.flushed = len(w.results)
	}
	return w
}

func (w *bufferedResultWriter) add(res StudentResult) {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := strings.ToUpper(res.USN)
	if idx, found := w.usnIdx[key]; found {
		w.results[idx] = res
	} else {
		w.results = append(w.results, res)
		w.usnIdx[key] = len(w.results) - 1
	}
	w.pending++

	if w.pending >= flushInterval {
		w.flushLocked()
	}
}

func (w *bufferedResultWriter) flushLocked() {
	if w.pending == 0 {
		return
	}
	f, err := os.Create(w.outFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ flush error: %v\n", err)
		return
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(w.results)
	f.Close()
	w.flushed += w.pending
	w.pending = 0
}

func (w *bufferedResultWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *bufferedResultWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.results)
}

type bufferedFailWriter struct {
	mu      sync.Mutex
	failed  []string
	outFile string
	pending int
}

func newBufferedFailWriter(outFile string) *bufferedFailWriter {
	w := &bufferedFailWriter{outFile: outFile}
	if raw, err := os.ReadFile(outFile); err == nil {
		json.Unmarshal(raw, &w.failed)
	}
	return w
}

func (w *bufferedFailWriter) add(usn string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failed = append(w.failed, usn)
	w.pending++
	if w.pending >= flushInterval {
		w.flushLocked()
	}
}

func (w *bufferedFailWriter) flushLocked() {
	if w.pending == 0 {
		return
	}
	f, err := os.Create(w.outFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ flush error: %v\n", err)
		return
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(w.failed)
	f.Close()
	w.pending = 0
}

func (w *bufferedFailWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *bufferedFailWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.failed)
}

// ────────────────────── MAIN ──────────────────────

func main() {
	// Determine which years to scan
	years := defaultYears
	if envYear := os.Getenv("YEAR_FILTER"); envYear != "" {
		if val, err := strconv.Atoi(envYear); err == nil {
			years = []int{val}
			fmt.Printf("🎯 Year filter: 20%d\n", val)
		}
	}

	// Optional branch filter
	var branchFilter int
	if envBranch := os.Getenv("BRANCH_FILTER"); envBranch != "" {
		if val, err := strconv.Atoi(envBranch); err == nil {
			branchFilter = val
			fmt.Printf("🎯 Branch filter: %d\n", val)
		}
	}

	startRoll := 1
	if len(os.Args) > 1 {
		if val, err := strconv.Atoi(os.Args[1]); err == nil && val > 0 {
			startRoll = val
			fmt.Printf("🚀 Starting from roll number: %03d\n", startRoll)
		}
	}

	// Build per-year output file names
	yearTag := "all"
	if len(years) == 1 {
		yearTag = strconv.Itoa(years[0])
	}
	outputFile := fmt.Sprintf("results_%s.json", yearTag)
	failedFile := fmt.Sprintf("failed_%s.json", yearTag)

	fmt.Printf("📁 Output: %s | Failed: %s\n", outputFile, failedFile)

	resultWriter := newBufferedResultWriter(outputFile)
	failWriter := newBufferedFailWriter(failedFile)

	// Graceful shutdown: flush on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n⚠ Interrupted! Flushing data...")
		resultWriter.flush()
		failWriter.flush()
		fmt.Printf("💾 Saved %d results, %d failed\n", resultWriter.count(), failWriter.count())
		os.Exit(0)
	}()

	startTime := time.Now()
	sem := make(chan struct{}, maxConcurrency)

	var totalScanned int64
	for _, yy := range years {
		branches := branchesForYear(yy)
		if branchFilter > 0 {
			// Check if the requested branch is valid for this year
			valid := false
			for _, b := range branches {
				if b == branchFilter {
					valid = true
					break
				}
			}
			if valid {
				branches = []int{branchFilter}
			} else {
				fmt.Printf("⚠ Branch %d not valid for year 20%d, skipping\n", branchFilter, yy)
				continue
			}
		}

		for _, bb := range branches {
			for _, dd := range dsyValues {
				fmt.Printf("\n━━━ Scanning 20%d | Branch %02d | DSY %d ━━━\n", yy, bb, dd)
				rollCursor, emptyBatchCount := startRoll, 0

				for {
					var batchWg sync.WaitGroup
					foundInBatch := false
					var mu sync.Mutex

					for i := 0; i < batchSize; i++ {
						roll := rollCursor + i
						if roll > maxRoll {
							break
						}
						usn := fmt.Sprintf("%02d%02d%02d%d%03d", yy, bb, constCC, dd, roll)

						batchWg.Add(1)
						sem <- struct{}{} // acquire slot

						go func(u string) {
							defer batchWg.Done()
							defer func() { <-sem }() // release slot

							atomic.AddInt64(&totalScanned, 1)
							res := fetchAllSemesters(u)

							if res != nil && len(res.Semesters) > 0 {
								mu.Lock()
								foundInBatch = true
								mu.Unlock()
								resultWriter.add(*res)
								fmt.Printf("✔ %s — %s — %d sem(s)\n", u, res.Name, len(res.Semesters))
							} else {
								failWriter.add(u)
							}
						}(usn)
					}
					batchWg.Wait()

					if foundInBatch {
						emptyBatchCount = 0
					} else {
						emptyBatchCount++
					}
					rollCursor += batchSize
					if emptyBatchCount >= emptyBatchCut {
						fmt.Printf("  ⏭ %d consecutive empty batches, moving on\n", emptyBatchCut)
						break
					}
				}
			}
		}
	}

	// Final flush
	resultWriter.flush()
	failWriter.flush()

	elapsed := time.Since(startTime)
	fmt.Print("\n\n" + strings.Repeat("━", 50) + "\n")
	fmt.Printf("✅ DONE\n")
	fmt.Printf("   Students found : %d\n", resultWriter.count())
	fmt.Printf("   USNs failed    : %d\n", failWriter.count())
	fmt.Printf("   Total time     : %s\n", elapsed.Round(time.Second))
	fmt.Printf("   Avg per USN    : %.1fms\n", float64(elapsed.Milliseconds())/float64(max(totalScanned, 1)))
	fmt.Print(strings.Repeat("━", 50) + "\n")

	// Write summary for GitHub Actions step summary
	if summaryFile := os.Getenv("GITHUB_STEP_SUMMARY"); summaryFile != "" {
		summary := fmt.Sprintf("## Scrape Results (Year %s)\n\n| Metric | Value |\n|---|---|\n| Students found | %d |\n| USNs failed | %d |\n| Duration | %s |\n",
			yearTag, resultWriter.count(), failWriter.count(), elapsed.Round(time.Second))
		os.WriteFile(summaryFile, []byte(summary), 0644)
	}
}
