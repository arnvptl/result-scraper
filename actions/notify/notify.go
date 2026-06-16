package main

import (
	"encoding/json"
	"fmt"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	smtpHost     = "smtp.gmail.com"
	smtpPort     = "587"
	senderEmail  = "lenibba1234@gmail.com"
	recipEmail   = "arnavp651@gmail.com"
	maxFailedLog = 200 // max number of failed USNs to include in email body
)

func main() {
	password := os.Getenv("GMAIL_APP_PASSWORD")
	if password == "" {
		fmt.Fprintf(os.Stderr, "❌ GMAIL_APP_PASSWORD env var not set\n")
		os.Exit(1)
	}

	// Determine what to report
	inputDir := "."
	if len(os.Args) > 1 {
		inputDir = os.Args[1]
	}

	// Gather result stats
	var totalStudents int
	resultFiles, _ := filepath.Glob(filepath.Join(inputDir, "results_*.json"))
	for _, f := range resultFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) == nil {
			totalStudents += len(arr)
		}
	}

	// Gather failed USNs
	var allFailed []string
	failFiles, _ := filepath.Glob(filepath.Join(inputDir, "failed_*.json"))
	for _, f := range failFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var failed []string
		if json.Unmarshal(raw, &failed) == nil {
			allFailed = append(allFailed, failed...)
		}
	}

	// Build email
	runID := os.Getenv("GITHUB_RUN_ID")
	repo := os.Getenv("GITHUB_REPOSITORY")
	serverURL := os.Getenv("GITHUB_SERVER_URL")
	runURL := ""
	if runID != "" && repo != "" {
		if serverURL == "" {
			serverURL = "https://github.com"
		}
		runURL = fmt.Sprintf("%s/%s/actions/runs/%s", serverURL, repo, runID)
	}

	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	subject := fmt.Sprintf("WIT Scraper Report — %d students, %d failed — %s", totalStudents, len(allFailed), now)

	var body strings.Builder
	body.WriteString(fmt.Sprintf("WIT Results Scraper — Run Report\n"))
	body.WriteString(fmt.Sprintf("════════════════════════════════════\n\n"))
	body.WriteString(fmt.Sprintf("Time:            %s\n", now))
	body.WriteString(fmt.Sprintf("Students found:  %d\n", totalStudents))
	body.WriteString(fmt.Sprintf("USNs failed:     %d\n", len(allFailed)))

	if runURL != "" {
		body.WriteString(fmt.Sprintf("Workflow run:    %s\n", runURL))
	}

	if len(allFailed) > 0 {
		body.WriteString(fmt.Sprintf("\n\nFailed USNs (%d total):\n", len(allFailed)))
		body.WriteString("──────────────────────\n")
		limit := len(allFailed)
		if limit > maxFailedLog {
			limit = maxFailedLog
		}
		for i := 0; i < limit; i++ {
			body.WriteString(allFailed[i] + "\n")
		}
		if len(allFailed) > maxFailedLog {
			body.WriteString(fmt.Sprintf("\n... and %d more (see artifacts for full list)\n", len(allFailed)-maxFailedLog))
		}
	} else {
		body.WriteString("\n\n🎉 No failures! All USNs scraped successfully.\n")
	}

	// Compose MIME message
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"UTF-8\"\r\n\r\n%s",
		senderEmail, recipEmail, subject, body.String())

	// Send via Gmail SMTP
	auth := smtp.PlainAuth("", senderEmail, password, smtpHost)
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, senderEmail, []string{recipEmail}, []byte(msg))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to send email: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Email sent to %s\n", recipEmail)
	fmt.Printf("   Subject: %s\n", subject)
}
