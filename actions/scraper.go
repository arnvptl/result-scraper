package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
	requestTimeout = 15 * time.Second
	maxConcurrency = 3    // gentle on shared hosting — 25 jobs × 3 = 75 total concurrent reqs
	batchSize      = 5    // small batches → frequent progress logs
	maxExamID      = 12
	dummyCaptcha   = "xxxxx"
	maxConsecFail  = 3    // stop descending exam IDs after this many consecutive failures
	emptyBatchCut  = 5    // skip to next branch/dsy after this many consecutive empty batches
	maxRoll        = 350  // upper bound on roll numbers
	flushInterval  = 25   // flush to disk every N results
	maxRetries     = 4    // HTTP retries per exam-ID fetch
	serverErrSleep = 30 * time.Second // wait when MySQL is down before retrying
)

var (
	defaultYears = []int{22, 23, 24, 25}
	dsyValues    = []int{1, 2}

	// errServerDown is returned when the site's MySQL is down (detectable from response body).
	// These are retriable server-side errors, NOT permanent "no data" failures.
	errServerDown = fmt.Errorf("server MySQL error")

	// Global counters updated atomically across all goroutines.
	globalFound      int64
	globalEmpty      int64
	globalServerErrs int64
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
// It buffers the response body so it can detect server-side MySQL errors before parsing.
// MySQL errors are retried with a long sleep; network errors use exponential backoff.
func fetchExamPage(usn string, examID int) (*goquery.Document, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			if lastErr == errServerDown {
				fmt.Printf("  ⚠ %s examID=%d — MySQL down, waiting %s before retry #%d\n",
					usn, examID, serverErrSleep, attempt)
				time.Sleep(serverErrSleep)
			} else {
				fmt.Printf("  ↻ %s examID=%d — net error, retry #%d\n", usn, examID, attempt)
				backoffSleep(attempt - 1)
			}
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

		// Buffer body so we can inspect it before handing to the HTML parser.
		bodyBytes, err := io.ReadAll(r3.Body)
		r3.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// Detect server-side MySQL errors — these look like valid HTTP 200 responses
		// but contain an error message instead of student data.
		body := string(bodyBytes)
		if strings.Contains(body, "Database connection error") ||
			strings.Contains(body, "Could not connect to MySQL") ||
			strings.Contains(body, "mysql_connect") {
			atomic.AddInt64(&globalServerErrs, 1)
			lastErr = errServerDown
			continue
		}

		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bodyBytes))
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

	fmt.Printf("🔍 %s\n", usn)
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

// ────────────────────── CHECKPOINT ──────────────────────

// Checkpoint tracks progress so the scraper can resume after timeout/failure.
type Checkpoint struct {
	Year      int  `json:"year"`
	Branch    int  `json:"branch"`
	DSY       int  `json:"dsy"`
	LastRoll  int  `json:"lastRoll"`
	Completed bool `json:"completed"`
}

func loadCheckpoint(path string) *Checkpoint {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cp Checkpoint
	if json.Unmarshal(raw, &cp) != nil {
		return nil
	}
	return &cp
}

func saveCheckpoint(path string, cp *Checkpoint) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ checkpoint save error: %v\n", err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.Encode(cp)
}

// shouldSkip returns true if this (year, branch, dsy, roll) combination has
// already been completed according to the checkpoint.
func (cp *Checkpoint) shouldSkip(year, branch, dsy, roll int) bool {
	if cp == nil {
		return false
	}
	if cp.Completed {
		return true
	}
	// Compare progress lexicographically: year → branch → dsy → roll
	if year < cp.Year {
		return true
	}
	if year == cp.Year {
		if branch < cp.Branch {
			return true
		}
		if branch == cp.Branch {
			if dsy < cp.DSY {
				return true
			}
			if dsy == cp.DSY && roll <= cp.LastRoll {
				return true
			}
		}
	}
	return false
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

	// Build per-job output file names (include branch if filtered)
	yearTag := "all"
	if len(years) == 1 {
		yearTag = strconv.Itoa(years[0])
	}
	fileTag := yearTag
	if branchFilter > 0 {
		fileTag = fmt.Sprintf("%s_b%d", yearTag, branchFilter)
	}
	outputFile := fmt.Sprintf("results_%s.json", fileTag)
	failedFile := fmt.Sprintf("failed_%s.json", fileTag)
	checkpointFile := fmt.Sprintf("checkpoint_%s.json", fileTag)

	fmt.Printf("📁 Output: %s | Failed: %s | Checkpoint: %s\n", outputFile, failedFile, checkpointFile)

	// Load checkpoint for resume
	checkpoint := loadCheckpoint(checkpointFile)
	if checkpoint != nil && !checkpoint.Completed {
		fmt.Printf("🔄 Resuming from checkpoint: Year=%d Branch=%d DSY=%d Roll=%d\n",
			checkpoint.Year, checkpoint.Branch, checkpoint.DSY, checkpoint.LastRoll)
	} else if checkpoint != nil && checkpoint.Completed {
		fmt.Printf("✅ Already completed (checkpoint says done). Exiting.\n")
		return
	}

	resultWriter := newBufferedResultWriter(outputFile)
	failWriter := newBufferedFailWriter(failedFile)

	// Current progress tracker (updated after each batch)
	currentCP := &Checkpoint{}

	// Graceful shutdown: flush + save checkpoint on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n⚠ Interrupted! Flushing data + saving checkpoint...")
		resultWriter.flush()
		failWriter.flush()
		saveCheckpoint(checkpointFile, currentCP)
		fmt.Printf("💾 Saved %d results, %d failed. Checkpoint saved.\n", resultWriter.count(), failWriter.count())
		os.Exit(0)
	}()

	startTime := time.Now()
	sem := make(chan struct{}, maxConcurrency)

	var totalScanned int64
	for _, yy := range years {
		branches := branchesForYear(yy)
		if branchFilter > 0 {
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
				// Check if this entire (year, branch, dsy) combo is already done
				if checkpoint != nil && checkpoint.shouldSkip(yy, bb, dd, maxRoll) {
					fmt.Printf("  ⏩ Skipping 20%d | Branch %02d | DSY %d (already done)\n", yy, bb, dd)
					continue
				}

				fmt.Printf("\n━━━ Scanning 20%d | Branch %02d | DSY %d ━━━\n", yy, bb, dd)

				// Determine starting roll: use checkpoint if resuming this exact combo
				rollStart := startRoll
				if checkpoint != nil && yy == checkpoint.Year && bb == checkpoint.Branch && dd == checkpoint.DSY {
					rollStart = checkpoint.LastRoll + 1
					fmt.Printf("  🔄 Resuming from roll %d\n", rollStart)
				}

				rollCursor, emptyBatchCount := rollStart, 0

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
						fmt.Printf("⏳ %s\n", u)
						res := fetchAllSemesters(u)

						if res != nil && len(res.Semesters) > 0 {
							mu.Lock()
							foundInBatch = true
							mu.Unlock()
							resultWriter.add(*res)
							atomic.AddInt64(&globalFound, 1)
							fmt.Printf("✔ %s — %s — %d sem(s)\n", u, res.Name, len(res.Semesters))
						} else {
							failWriter.add(u)
							atomic.AddInt64(&globalEmpty, 1)
							fmt.Printf("✗ %s — no results\n", u)
						}
					}(usn)
					}
					batchWg.Wait()

					// Batch progress summary — visible in GitHub Actions logs
					batchEnd := rollCursor + batchSize - 1
					if batchEnd > maxRoll {
						batchEnd = maxRoll
					}
					fmt.Printf("  📊 [20%d|B%02d|D%d] rolls %03d–%03d | ✔ found: %d | ✗ empty: %d | ⚠ srv-err: %d | ⏱ %s\n",
						yy, bb, dd, rollCursor, batchEnd,
						atomic.LoadInt64(&globalFound),
						atomic.LoadInt64(&globalEmpty),
						atomic.LoadInt64(&globalServerErrs),
						time.Since(startTime).Round(time.Second),
					)

					// Update checkpoint after each batch
					currentCP.Year = yy
					currentCP.Branch = bb
					currentCP.DSY = dd
					currentCP.LastRoll = rollCursor + batchSize - 1
					if currentCP.LastRoll > maxRoll {
						currentCP.LastRoll = maxRoll
					}
					saveCheckpoint(checkpointFile, currentCP)

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

	// Mark as completed
	currentCP.Completed = true
	saveCheckpoint(checkpointFile, currentCP)

	// Final flush
	resultWriter.flush()
	failWriter.flush()

	elapsed := time.Since(startTime)
	srvErrs := atomic.LoadInt64(&globalServerErrs)
	fmt.Print("\n\n" + strings.Repeat("━", 50) + "\n")
	fmt.Printf("✅ DONE\n")
	fmt.Printf("   Students found  : %d\n", resultWriter.count())
	fmt.Printf("   USNs no results : %d\n", failWriter.count())
	fmt.Printf("   Server errors   : %d\n", srvErrs)
	fmt.Printf("   Total scanned   : %d\n", totalScanned)
	fmt.Printf("   Total time      : %s\n", elapsed.Round(time.Second))
	fmt.Printf("   Avg per USN     : %.1fms\n", float64(elapsed.Milliseconds())/float64(max(totalScanned, 1)))
	fmt.Print(strings.Repeat("━", 50) + "\n")

	// Write summary for GitHub Actions step summary
	if summaryFile := os.Getenv("GITHUB_STEP_SUMMARY"); summaryFile != "" {
		summary := fmt.Sprintf("## Scrape Results (%s)\n\n| Metric | Value |\n|---|---|\n| Students found | %d |\n| USNs no results | %d |\n| Server errors (retried) | %d |\n| Duration | %s |\n| Completed | ✅ |\n",
			fileTag, resultWriter.count(), failWriter.count(), srvErrs, elapsed.Round(time.Second))
		os.WriteFile(summaryFile, []byte(summary), 0644)
	}
}
